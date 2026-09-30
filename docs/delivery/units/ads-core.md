# Unit ads-core — 0074/0075 + post-River 0015, `internal/ads` domain, Check, sweepers, merchant HTTP, operator CLI

Role: commerce_worker (mid tier). Base SHA `00c1d94`. Worktree `.worktrees/ads-core`, branch
`unit/ads-core`. No delegation, no network, no Meta token. **Wave 1**, parallel with ads-graph and
ads-ui against the FROZEN blocks here and in `ads-a10.md`. Contract: `contracts/meta-ads-v1.md`
(FROZEN) incl. "Integrator rulings" and round dispositions; the Defaults below bind unless the
integrator overrides. Release write-set boundary: ads owns migrations 0074, 0075, 0080 (0080 is
ads-capi's), `post_river/0015`, `internal/ads`, `internal/integrations/meta_ads`, `cmd/ads-worker`,
admin ads pages. auth (0070, identity password/mail, `internal/mail`), cvs (0072–0073, fulfillment cvs,
ECPay), stripe-live (0077, `post_river/0016`), customers-billing (0078–0079, `internal/customers`,
`internal/billing`), U08 purge (0071) are other units' — never touch them.

**Goal:** merchant connects a Meta ad account (state + pick list + sealed token copy), drafts, approves
and publishes a campaign whose only spend switch is a Check-gated `activate`; pause always works; the
allowance/billing/sandbox rules hold under concurrency; insights ingest and auto-pause.

## Read (by section; `grep -n` headings, `sed -n` ranges)
PROCESS.md; contract §0 (AD1–AD11, §0.1), §2, §3 (money + result grammar + classification only),
§3.1, §4.1, §4.2, §4.4, §5, §6.1–§6.3, §7, §8, §9 (MA01/02/04–07/11 rows), §12, rulings. By symbol:
`0064_meta_claims_intake.sql` (`meta_page_credentials` :231, `load_meta_page_token` :894–927,
`plan_claim_reply` :829, `principal_holds` grant :77, G/M GUC policy pattern), `0003` `resolve_access`,
`0008` operations/events (+ CHECKs, `provider_reference` ≤200), `0016:85–95`, `0035:116–123` (planner
INSERT + events policy pattern), `0040` (hash-authenticated definer), `0062` (definer + COMMENT style),
`post_river/0014` (River guard pattern), `post_river/0004`; Go: `internal/integrations/core/{service.go
(RegisterBinding, Plan, Get), jobs.go}`, `internal/command` (`Run`, `ValidID`), `internal/platform`
(`Scope`, `WithScope`), `internal/httpapi/{handler.go,refunds.go,claimsource.go}` (route + classifier
shape), `cmd/meta-admin/{doc.go,main.go}`.

## Defaults adopted (contract gaps found while writing this brief; shared by every ads unit)
- D1 **Paths.** §7 `/v1/merchant/ads/…` is mounted as `/v1/admin/stores/{store_id}/ads/…` (repo
  convention, `apps/admin/lib/backend.ts callBackend`); store from path + session scope only.
- D2 **Read results ≤ 200 chars**, not §3's 255: `integration.operations.provider_reference` CHECK
  (0008:51) and `core.validReference` cap at 200. Grammar unchanged; >200 → adapter FAILED_FINAL `bad_result`.
- D3 **Coded denials** via `core.DenyPolicy(code)` (ads-a10 A10-D2). Check returns nil when
  `req.Mode == "reconcile"` (X2).
- D4 **Queue.** All ads ops enqueue with `core.InsertOperationJobOn(…, "ads", p)`, pause p=1, every
  other ads/CAPI op p=3; planner definers take the job id (pattern `plan_claim_reply`).
  `post_river/0015_meta_ads_river.sql` (A-5) admits exactly: `external_operation_v1` on queue `ads`
  priority ∈ {1,3} from `commerce_runtime` (publish/pause/end) and `commerce_worker` (sweepers), plus
  the periodic kinds `ads_publish_advance_v1`, `ads_insights_plan_v1`, `ads_oauth_purge_v1` and
  (ads-capi's, frozen name) `capi_purchase_sweep_v1` on queue `ads` from `commerce_worker`; reciprocal commit check "job ⇒ ads op"
  as in 0014. Verify against every existing `river.river_job` trigger (`grep -n river_job migrations/post_river/*.sql`).
- D5 **Two read routes the UI needs** (§7 is silent): `GET ads/meta/states/{state_id}` (connect perms;
  principal+store match; `now<expires_at`; returns pick list) and `GET ads/settings` (`ads:read`).
- D6 **Callback** returns `200 {"state_id"}`; the admin BFF issues the 303 (Go private API is not
  browser-facing). Fixed codes `state_mismatch` (409), `state_expired` (410), `meta_connect_failed` (502).
  The handler never logs the URL/query; `code`/`state` redacted in every formatter.
- D7 **Operator definer** `ads.operator_set_settings(p_tenant uuid, p_store uuid, p_environment text,
  p_sandbox_ad_account text, p_max_active_budget_minor bigint, p_allowance_currency text) → timestamptz`
  (owner `commerce_ads_writer`, EXECUTE `commerce_meta_registrar` only, audited
  `ads.operator_settings_changed`); `cmd/meta-admin ads-settings`. **Needs a §4.4 row (integrator
  ruling R-A; MA02 asserts equality).**
- D8 **Report "orders net of refunds"** = Σ `payments.facts` CAPTURED − Σ succeeded
  `payments.refund_facts` in the window, store timezone; pay-at-pickup orders (no payment facts) are
  not counted and the block says so (`note:"card_payments_only"`). **Needs §4.4 row SELECT + read
  policy on `payments.refund_facts` for `commerce_ads_writer` (ruling R-B).** Until ruled, block = null.
- D9 **Insights cadence:** one hourly periodic `ads_insights_plan_v1`; per draft it plans reads every
  hour while the AD6 test still counts the draft, else only in the store-local 04:00 hour
  (`Asia/Taipei` when the store has no timezone). Semantic key per §6.2. No on-demand refresh route in v1
  (§7 has none) — the UI "refresh" re-GETs.
- D10 **Canonical draft** (AD5): SQL `ads.canonical_draft(p_draft uuid) → text` and Go `CanonicalDraft`
  byte-identical: `v1|<template>|<ad_binding_id>|<ad_binding_version>|<identity_binding_id>|<identity
  binding current version>|<source_ref>|<currency>|<lifetime_budget_minor>|<starts_at unix>|<ends_at
  unix>|<countries sorted, comma>|<age_min>|<age_max>`; hash = SHA-256 of UTF-8.
- D11 **Frozen op requests** (written by the planners, read by ads-graph; no tokens, no PII) — block below.
  Objective: BOOST_POST → `OUTCOME_ENGAGEMENT`, PRODUCT_TRAFFIC → `OUTCOME_TRAFFIC`.
  PRODUCT_TRAFFIC needs a `facebook` identity binding (Page) and freezes `link_url` =
  `https://<active storefront domain>/products/<product public id>` at create_creative planning.
- D12 **spend_cap** (AD4, F10 minimum is "USD 100 equivalent", no FX here): set only for USD drafts
  ≥ 10000 minor; TWD/HKD omit it (lifetime budget is the bound). `ponytail:` comment; MA-S1 may revise.
- D13 §0.1 defaults A-1..A-11 as written (no auto grants; self-approve allowed; `post_river/0015`;
  Graph `v26.0` config; 0080 allocated to ads-capi).
- D14 Store timezone/allowance comparisons in minor units, same currency only (`// I05:` comments).

## FROZEN Go interface (ads-graph, ads-capi, ads-ui, ads-tests and the integrator call exactly these)
```go
package ads // internal/ads — commit types.go + doc.go FIRST, alone (F0); integrator merges F0 at once

// types.go
type SealedToken struct{ KeyID string; Enc, Ciphertext []byte }                    // String/GoString/Format/MarshalJSON redacted
type Pick struct{ Kind string `json:"kind"` /*ad_account|dataset*/; ID string `json:"id"`; Name string `json:"name"`
    Currency string `json:"currency,omitempty"`; Timezone string `json:"timezone,omitempty"`; AccountStatus int `json:"account_status,omitempty"` }
type SealInfo struct{ TenantID, StoreID string }                                    // HPKE info ["livecommerce/meta-ads-token/v1",tenant,store,key_id]
type ConnectResult struct{ ClientBusinessID string; Scopes []string; Picks []Pick; Token SealedToken }
type ConnectFunc func(ctx context.Context, code string, seal SealInfo) (ConnectResult, error) // = (*metaads.OAuth).Connect
type DialogConfig struct{ AppID, ConfigID, RedirectURI, GraphVersion string }       // dialog host www.facebook.com
var ErrConnectFailed error                                                          // → 502 meta_connect_failed

// service
func NewService(jobs *river.Client[pgx.Tx], connect ConnectFunc, dialog DialogConfig) (*Service, error) // jobs: insert-only, Schema "river"
func CanonicalDraft(d DraftCanon) []byte                                           // D10; DraftCanon = the 14 fields in D10 order
func NewChecker(pool *pgxpool.Pool) *Checker
func (c *Checker) Check(ctx context.Context, req core.DispatchRequest) error        // §6.1 meta_ads actions; PG only; nil in reconcile mode; core.DenyPolicy(code)
func AddWorkers(w *river.Workers, pool *pgxpool.Pool) error                         // ads_publish_advance_v1, ads_insights_plan_v1, ads_oauth_purge_v1
func PeriodicJobs() []*river.PeriodicJob                                           // 30 s / hourly / 5 min, queue "ads"

package httpapi // internal/httpapi/ads.go (new file only)
func registerAdsRoutes(mux *http.ServeMux, pool *pgxpool.Pool, svc *ads.Service) // nil svc → unmounted
```

**Frozen op request JSON** (`integration.operations.request`, provider `meta_ads`, D11; every key required unless marked):
```text
create_campaign  {"v":1,"draft_id","attempt","name":"lc-<op uuid>","objective","currency","spend_cap_minor"(0=omit)}
create_adset     {"v":1,"draft_id","attempt","name","campaign_id","template","currency","lifetime_budget_minor","start_time","end_time"(RFC3339 UTC),"countries":[..],"age_min","age_max"}
create_creative  {"v":1,"draft_id","attempt","name","template","page_id","object_story_id"?|"source_instagram_media_id"?+"instagram_user_id"?|"link_url"?}
create_ad        {"v":1,"draft_id","attempt","name","adset_id","creative_id"}
preflight_account{"v":1,"draft_id","attempt","seq"}            activate|pause {"v":1,"draft_id","attempt","seq","campaign_id"}
read_insights    {"v":1,"draft_id","campaign_id","day":"YYYY-MM-DD"}
```
The ad account id is `DispatchRequest.ExternalAssetID` (the `meta_ads` binding asset), never in the body.

**Frozen HTTP** (base `/v1/admin/stores/{store_id}/ads`; errors `{"error":"<code>"}`):
| Route | Perm | Body → 2xx |
| --- | --- | --- |
| `POST meta/connect` (Idempotency-Key) | ads:manage+integration:manage | – → 201 `{state_id,dialog_url,expires_at}` |
| `GET meta/callback?code&state` | session; state principal+store | → 200 `{state_id}` (D6) |
| `GET meta/states/{state_id}` (D5) | same | → `{state_id,expires_at,client_business_id,picks:[Pick]}` |
| `POST meta/bindings` (Idempotency-Key) | same | `{state_id,ad_account_id,dataset_id?}` → 201 `{ad_binding_id,dataset_binding_id?}` |
| `GET settings` (D5) | ads:read | → `{environment,allowance_currency,max_active_budget_minor,sandbox_ad_account?,capi:{enabled,dataset_binding_id?,test_event_code?},connections:[{binding_id,provider,asset_id,client_business_id,enabled,connected_at}],identities:[{binding_id,provider,asset_id}]}` |
| `GET drafts`, `GET drafts/{id}` | ads:read | → `{items:[Draft]}` / `Draft` |
| `POST drafts` (Idempotency-Key), `PUT drafts/{id}` (Idempotency-Key + `If-Match: <revision>`) | ads:manage | `DraftInput` → 201/200 `Draft` |
| `POST drafts/{id}/approve` | ads:approve | `{revision}` → `Draft` |
| `POST drafts/{id}/publish` (Idempotency-Key) | ads:approve | `{publish_attempt}` → `Draft` |
| `POST drafts/{id}/pause`, `…/end` (Idempotency-Key) | ads:manage / ads:approve | – → `Draft` |
| `GET report?from&to` (YYYY-MM-DD, ≤ 92 d) | ads:read | → `{window:{from,to},timezone,orders:{captured_minor,refunded_minor,net_minor,currency,note,fetched_at}|null,meta_delivery:{spend_minor,impressions,clicks,currency,account_timezone,fetched_at,final_through},meta_reported:{purchases,purchase_value_minor,currency,fetched_at}}` |
| `PUT capi` (Idempotency-Key) | ads:manage | `{enabled,dataset_binding_id,test_event_code}` → settings.capi |

`DraftInput` = `{ad_binding_id,identity_binding_id,template,source_ref,currency,lifetime_budget_minor,starts_at,ends_at,countries,age_min,age_max}`;
`Draft` = DraftInput + `{id,revision,publish_attempt,status(§5.1),approved_revision?,ended_at?,created_at,remote:{campaign_id?,adset_id?,creative_id?,ad_id?},ops:[{kind,seq,attempt,state,code?,updated_at}],ads_manager_url?}`.
Codes: `state_mismatch`, `state_expired`, `meta_connect_failed`, `not_in_pick_list`, `client_business_changed`,
`revision_changed`, `over_allowance`, `billing_restricted`, `attempt_changed`, `prior_attempt_not_paused`,
`draft_approved`, `budget_below_minimum`, `not_whole_unit`, `currency_mismatch`, `starts_too_soon`,
`binding_disabled`, `source_not_owned`, `product_not_published`, `forbidden`, `not_found`, `invalid_request`.

**Frozen SQL** (signatures exactly; bodies per contract): `integration.register_meta_ads_token(bytea,uuid,uuid,uuid,bigint)→bigint`;
`integration.load_meta_ads_token(uuid,bigint,bytea) RETURNS TABLE(tenant_id uuid,store_id uuid,binding_id uuid,provider text,asset_id text,version bigint,key_id text,nonce bytea,ciphertext bytea,scopes_attested text[])`
(0064 loader's shape; `nonce` = HPKE enc); `ads.set_capi(bytea,uuid,boolean,uuid,text)`; `ads.put_insights_day(uuid)` (op id; parses the op's `provider_reference`);
`ads.canonical_draft(uuid)→text`; `ads.operator_set_settings(…)` (D7); `ads.check_*`/`ads.advance_*`/`ads.plan_*` names are
yours, EXECUTE per §4.4. **No `store_grants` rows.**

## Build
1. **F0** `internal/ads/{doc.go,types.go}` exactly as frozen → hand to integrator.
2. **SQL** `0074_meta_ads.sql` (§4.1 + D7 + D10), `0075_meta_ads_insights.sql` (§4.2),
   `post_river/0015_meta_ads_river.sql` (D4). `pg_get_constraintdef` re-derivations for widened CHECKs;
   §4.4 grants exactly (+ R-A/R-B rows only once ruled); FORCE RLS + policies; `COMMENT ON` everything.
   Ads definers call `billing.store_standing` (0079/0080) by name at run time — fail closed (definer
   raises) until 0080 is applied (ads-capi, wave 2).
3. **Allowlists** (step 3 is an integrator-reviewed diff; list every before/after): T06 approved
   `integration` function rows +2 (`register_meta_ads_token` registrar/runtime, `load_meta_ads_token`
   worker) in `tests/foundation/external_operation_authority_test.go`; schema-set assertions that
   enumerate schemas (grep the files listed under Write paths) gain `ads`. Never loosen a negative.
   Hand steps 1–3 to the integrator first = **F1c** (customers-core and cvs units also edit T06/KC03 rows; integrator merges).
4. **Go** `internal/ads`: service (connect/state/bind via `core.RegisterBinding` + registrar definer;
   drafts; approve; publish CAS; pause/end; report; set_capi), local validation §5.2 (also in Check),
   Checker §6.1 (activate: `pause_requested`, AD5 hash + approver, standing, AD6 under store lock, AD9,
   fresh preflight; creates; provider `meta_ads` only — any other action → deny `unknown_action`; the
   CAPI Check is ads-capi's own route Check), sweepers §6.2/§6.3 (no
   network, no lock across I/O, `approved_at` order, same-run room test, pause seq+1 after 15 min / after
   late activate, never activate after any pause or `ended_at`, X7), OAuth-state purge (pending
   ciphertext deleted at `expires_at`).
5. **HTTP** `internal/httpapi/ads.go`: own classifier (refunds.go shape), strict JSON, D1/D5/D6.
6. **Operator CLI** `cmd/meta-admin/ads.go`: `ads-settings --tenant --store [--environment] [--sandbox-ad-account]
   [--max-active-budget-minor] [--allowance-currency]`; stdout ids/timestamps only; doc.go gains the subcommand.

## PROCESS §5 comment/dependency rules (binding)
Package comment `// Package ads owns …` / `// It never …` (dials Meta, holds a token, pays for ads);
cross-domain SQL call sites name table/function + why (`// billing.store_standing: BD5 …`); money
compares `// I05:`; every UNKNOWN/retry branch says why same key / why never; external constants carry
URL + retrieval date (Meta facts F#); `COMMENT ON` every table/column/function/role/policy naming owner
package, allowed roles, non-goals; no hand-written depends-on lists — run `scripts/dev/depmap.sh`.

## Write paths
`migrations/0074_meta_ads.sql`, `migrations/0075_meta_ads_insights.sql`, `migrations/post_river/0015_meta_ads_river.sql`,
`internal/ads/**`, `internal/httpapi/{ads.go,ads_test.go}`, `cmd/meta-admin/{ads.go,ads_test.go,doc.go}` + the one
`case "ads-settings":` line in `cmd/meta-admin/main.go`, step 3 only: `tests/foundation/external_operation_authority_test.go`
and the schema-enumerating assertions in `tests/foundation/{meta_claims_intake_schema,live_claims_schema,local_recovery}_test.go`,
`docs/engineering/dependency-map.md` (generated), `output/ads-core/**`.
Forbidden: `handler.go`, `cmd/api/**`, `cmd/ads-worker/**`, `internal/integrations/**`, 0080, contracts,
`core-openapi.json`, `apps/**`, go.mod, other `tests/**`.

## Gates
Implementer (unit, no `TestMetaAdsMA` prefix): `CanonicalDraft` vectors, local validation, allowance
arithmetic, handler strictness (`httptest`), CLI flag validation. Independent (ads-tests): MA01 (hash
part), MA02, MA04–MA07, MA09 (admin part), MA11 (loader part) — red-then-green there, not here.

## Verify
```sh
GOTOOLCHAIN=go1.27.1 go vet ./... && gofmt -l internal cmd
GOTOOLCHAIN=go1.27.1 go test -race -count=1 ./internal/ads/... ./internal/httpapi ./cmd/meta-admin
LC_FOCUSED_TIMEOUT=1800s bash scripts/dev/test-focused.sh '^Test(T06|MetaClaimsMCI0[1-4]|LiveClaims.*Schema|LocalRecovery|Pool)'
bash scripts/dev/depmap.sh && python3 scripts/check_packet.py
```
Logs → `/Volumes/data/live_commerce_architecture_v1/output/ads-core/`.

## NOT_RUN (expected)
Billing/consent paths until 0079+0080 merged (F3); every MOCK chain until ads-graph merged; all SANDBOX/LIVE.

## Integrator hooks (integrator only; this unit exposes the function)
`internal/httpapi/handler.go`: `Options.Ads *ads.Service` + `registerAdsRoutes(mux, pool, configured.Ads)`;
`core-openapi.json` ads paths from the Frozen HTTP table; `tests/foundation` privilege-matrix row merge with cvs-core;
§4.4 rows for rulings R-A/R-B; mount only after 0080 (contract §4.3).

## Order / Non-goals / Return
F0 → F1c (SQL+allowlists) → full merge F2 with ads-graph. No 0080, no CAPI sweeper/feed, no UI, no LIVE,
no budget change of a remote object, no re-activation (X7), no page-token self-serve connect (operator CLI
registers Page tokens for the pilot). Return SHA, model/reasoning, base, paths, commands + exits + counts,
evidence, D1–D14 handling, risks, NOT_RUN. Any deviation from a frozen signature/JSON = stop and escalate.
