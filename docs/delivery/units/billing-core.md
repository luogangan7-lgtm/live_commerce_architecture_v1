# Unit billing-core — 0079 platform billing SQL, standing guard, Stripe Billing platform client, webhook, routes

Role: integration_worker (mid tier). Base `00c1d94`. Worktree `.worktrees/billing-core`, branch `unit/billing-core`.
No delegation, no network, no Stripe key (all Stripe calls go through an injectable `http.RoundTripper`; CB10 SANDBOX
is `customers-billing-tests`'). Parallel with `customers-core`; author at dispatch, run PG tests after F1 (0078 merged:
0079 preconditions need it). Contract: `contracts/customers-billing-v1.md` (FROZEN) incl. §0.1 and §12.

**Goal:** T17 — platform fee via Stripe Billing on the platform's own SANDBOX account: pin customer, Checkout
subscription (one open session, trial once), portal, webhook → retrieve → mirror, derived standing, and the
`live.claim_windows` guard that refuses only new opens under RESTRICTED (PT412 → 402 `billing_restricted`).

## Read (by section)
PROCESS.md; contract §0 BD1–BD8 + rejected list, §0.1 C-3/C-4/C-5/C-8, §1 F-B1..F-B13, §2 billing lines, §3.2,
§4 standing, §5 billing rows + checkout steps + Platform paragraph, §6 rows 3–7, §7, §8 CB06–CB08/CB10, §9–§10.
By symbol: `0065_owner_provisioning.sql` (`create_initial_store` body, grant array), `0063` (GUC check, audit,
permission CHECK re-derivation), `0060_live_claims.sql:240-241` (runtime UPDATE on `live.claim_windows`),
`0014`/`0061` `integration.merchant_accounts`; Go (read only): `internal/integrations/psp/stripe/{webhook.go
(NewWebhookVerifier, Verify, Event), config.go (APIVersion), client.go (call/classify pattern)}`,
`cmd/api/stripe_webhook.go` (ingress pool open/validate/mount pattern), `internal/claims/claims.go` (`mapError`),
`internal/httpapi/{claims.go (claimsClassify), refunds.go}`, `internal/platform/stripe_runtime.go`.

## Defaults adopted (P2s the frozen text left open)
- B1 C-4: the webhook reuses the `commerce_stripe_ingress` login (`COMMERCE_STRIPE_INGRESS_DATABASE_URL`) with EXECUTE
  on `billing.apply_subscription` + `billing.platform_account_conflict` only; `commerce_runtime` never gets
  `apply_subscription`/`store_standing`.
- B2 C-3/C-8: `billing_restricted` = HTTP **402** on this contract's surfaces. Aligning meta-ads' 409 is the
  integrator's ruling in the meta-ads brief, not this unit.
- B3 Env (contract names kept): `LC_BILLING_ENABLED` (`1` or unset), `LC_PLATFORM_STRIPE_SECRET_KEY`
  (`sk_test_`/`rk_test_` only), `LC_PLATFORM_STRIPE_WEBHOOK_SECRET` (`whsec_`), `LC_BILLING_PRICE_IDS` (1..10
  `price_…`), `LC_BILLING_RETURN_ORIGIN` (https origin of the admin). Return URLs: `<origin>/billing?store=<id>
  &checkout=done|cancel` and portal `<origin>/billing?store=<id>` (admin `proxy.ts` adds the locale). Unset
  `LC_BILLING_ENABLED` → `Service` nil: standing + `GET billing` still work (plans `[]`), POSTs 503 `billing_unavailable`.
- B4 Startup: `GET /v1/account` then `billing.platform_account_conflict(id)` on the runtime pool; unreadable or
  conflict → service disabled for the process lifetime (503 + one `billing_account_conflict` log line); no retry loop.
- B5 Customer create `POST /v1/customers` body `metadata[lc_store]=<store>` only (email is typed in Checkout, F-B4),
  key `lc:billing:customer:v1:<store>:SANDBOX`, then `pin_customer`.
- B6 Checkout body (sorted form keys): `cancel_url, client_reference_id, customer, expires_at, line_items[0][price],
  line_items[0][quantity]=1, mode=subscription, subscription_data[metadata][lc_store], success_url` +
  `subscription_data[trial_period_days]=14` only under BD2. No Idempotency-Key (§6). `price_id` ∉ configured set → 422.
- B7 Plans: `GET /v1/prices/{id}?expand[]=product` per configured id; keep only `active`, `recurring`,
  `livemode=false`; 10-min process cache; a failed fetch omits that plan and logs.
- B8 Subscription projection (APIVersion `2026-08-26.dahlia`): `id, customer, status, created, livemode,
  cancel_at_period_end, metadata.lc_store, items.data[0].price.id, items.data[0].current_period_start/end` (F-B10);
  ≠ 1 item → not applied, log `billing_multi_item`, webhook 200.
- B9 Webhook: body ≤ 64 KiB; `stripe.NewWebhookVerifier{Secrets:[secret], AccountID: startup acct, Environment:
  SANDBOX}`; signature error / malformed / `livemode=true` → 400; handled types per §5 else 200. Subscription id:
  `customer.subscription.*` → `Event.SessionID` (object id); `invoice.*` → re-read the verified raw body with stdlib
  `encoding/json` for `data.object.parent.subscription_details.subscription` (absent → 200 ignored). Retrieve
  `GET /v1/subscriptions/{id}` with no tx open; failure → 503. `apply_subscription` results `applied|stale` → 200;
  `duplicate|mismatch|unknown_customer` → 200 + one `billing_ops_alert code=<result>` log line (no ids beyond sub id).
- B10 Refresh-on-read: `GET billing` with a pinned customer and any row `retrieved_at` older than 10 min lists
  `GET /v1/subscriptions?customer=&status=all&limit=100` and applies each via `refresh_subscription` (one short tx
  each); Stripe error → DB state with `stale:true`.
- B11 Operator script `scripts/ops/grant-r2-permissions.sql`: psql vars `store_id`, `principal_id`; aborts unless that
  principal holds the full 0065 creator grant set on the store; inserts the 3 grants only if absent + audit
  `store.permissions_granted`; single tx; never run automatically.
- B12 Claims PT412: `internal/claims/claims.go` `mapError` gains `case "PT412": return ErrBillingRestricted`
  (new exported sentinel); `httpapi/claims.go` `claimsClassify` maps it to 402 `billing_restricted`. Nothing else in
  those files changes.

## FROZEN interface (UI, tests and integrator call exactly these)
SQL (0079; R3 `0080_meta_capi.sql` grants `store_standing` to `commerce_ads_writer`, C-8):
```sql
billing.store_standing(p_tenant uuid, p_store uuid) RETURNS text   -- 'GOOD'|'GRACE'|'RESTRICTED'|'UNBILLED'
  -- STABLE SECURITY DEFINER SET search_path=pg_catalog, owner commerce_billing_writer, EXECUTE commerce_auth only.
billing.apply_subscription(text,text,text,text,text,timestamptz,timestamptz,boolean,timestamptz,timestamptz,uuid,boolean) RETURNS text
billing.refresh_subscription(bytea,uuid, <the 12 apply_subscription args, same order/types>) RETURNS text
billing.platform_account_conflict(text) RETURNS boolean
-- plus §3.2 read_billing(bytea,uuid) jsonb, read_billing_standing(bytea,uuid) text, pin_customer, record_checkout_session.
```
```go
package billing // internal/billing
type Standing string
const Good, Grace, Restricted, Unbilled Standing = "GOOD", "GRACE", "RESTRICTED", "UNBILLED"
func StandingOf(statuses []string) Standing // pure BD4 twin of billing.store_standing (CB01)
type Config struct{ SecretKey, WebhookSecret, ReturnOrigin string; PriceIDs []string } // String/GoString/MarshalJSON redacted
func LoadConfig(getenv func(string) string) (cfg Config, enabled bool, err error)
type Subscription struct{ Status string `json:"status"`; PriceID string `json:"price_id"`; CurrentPeriodStart *string `json:"current_period_start"`
    CurrentPeriodEnd *string `json:"current_period_end"`; CancelAtPeriodEnd bool `json:"cancel_at_period_end"`; RetrievedAt string `json:"retrieved_at"` }
type Usage struct{ PeriodStart string `json:"period_start"`; PeriodEnd string `json:"period_end"`; PaidOrders int64 `json:"paid_orders"`
    ClaimWindowsOpened int64 `json:"claim_windows_opened"`; PrivateRepliesSent int64 `json:"private_replies_sent"`; Members int64 `json:"members"` }
type Plan struct{ PriceID string `json:"price_id"`; Name string `json:"name"`; AmountMinor int64 `json:"amount_minor"`; Currency string `json:"currency"`; Interval string `json:"interval"` }
type Status struct{ Standing Standing `json:"standing"`; PaymentPending bool `json:"payment_pending"`; CustomerPinned bool `json:"customer_pinned"`
    Subscriptions []Subscription `json:"subscriptions"`; Usage Usage `json:"usage"`; Plans []Plan `json:"plans"`; Stale bool `json:"stale"` }
type Service struct{ /* unexported */ }
func New(ctx context.Context, runtime *pgxpool.Pool, cfg Config, rt http.RoundTripper) (*Service, error) // rt nil → http.DefaultTransport; B4
func (s *Service) Enabled() bool
func (s *Service) Status(ctx context.Context, pool *pgxpool.Pool, scope platform.Scope, token string) (Status, error) // nil receiver OK
func ReadStanding(ctx context.Context, tx pgx.Tx, scope platform.Scope, token string) (Standing, error)
func (s *Service) StartCheckout(ctx context.Context, pool *pgxpool.Pool, scope platform.Scope, token, priceID string) (url string, err error)
func (s *Service) OpenPortal(ctx context.Context, pool *pgxpool.Pool, scope platform.Scope, token string) (url string, err error)
func (s *Service) WebhookHandler(ingress *pgxpool.Pool) http.Handler
var ErrUnavailable, ErrSubscriptionExists, ErrNoCustomer, ErrUnknownPrice error // 503 billing_unavailable, 409, 409 no_billing_customer, 422

package claims
var ErrBillingRestricted error // PT412 (B12)

func registerBillingRoutes(mux *http.ServeMux, pool *pgxpool.Pool, svc *billing.Service) // internal/httpapi/billing.go; nil svc still mounts GETs
// cmd/api/platform_billing.go — integrator calls these from main.go:
func buildPlatformBilling(ctx context.Context, mainPool *pgxpool.Pool, getenv func(string) string) (svc *billing.Service, webhook http.Handler, closeFn func(), err error)
func mountPlatformBilling(fallback, webhook http.Handler) http.Handler // exactly POST /v1/platform/stripe/webhook; nil webhook → fallback
```
URLs returned by `StartCheckout`/`OpenPortal` are bearer links: never logged, stored or put in errors (I11).

## Build
1. **SQL** `0079_platform_billing.sql`: §3.2 exactly; permission CHECK re-derived (`pg_get_constraintdef`), add
   `billing:manage` only if absent; `create_initial_store` sixth version = 0065 body byte for byte except the grant
   array (+`customers:read`,`customers:privacy`,`billing:manage`); set-once + canceled-terminal trigger; guard trigger
   `SECURITY DEFINER`; every function per its row; `COMMENT ON` all. No grant to any R3 role.
2. **Stripe platform client** in `internal/billing` (stdlib `net/http` + `encoding/json`, `Stripe-Version:
   stripe.APIVersion`, 10 s timeout, no in-request retry, `sk_test_`/`rk_test_` only, form keys sorted, docs URL +
   retrieval date per wire constant). Reuse `stripe.NewWebhookVerifier`; do not edit `internal/integrations/psp/stripe/**`
   (stripe-live-enable owns it this release).
3. **Service/webhook/routes** per frozen block + B4–B10; `httpapi/billing.go` own classifier via `scopedAs`
   (`billing:manage` for GET billing and POSTs, `store:read` for standing; POSTs strict body, no Idempotency-Key).
4. **Claims mapping** B12; **operator script** B11; `cmd/api/platform_billing.go` (config, ingress pool open +
   `ValidateSameDatabase`, `billing.New`, webhook).
5. Hand step 1 + the hooks patch (`output/billing-core/integrator-hooks.patch`) to the integrator first.

## Comments (PROCESS §5)
Package comment: "owns platform-fee subscription mirror and standing; It never charges buyers, touches merchant PSP
accounts, blocks refunds/fulfilment/checkout, or stores invoices; calls api.stripe.com (platform account) for
customers, checkout sessions, portal sessions, prices, subscriptions". Each Stripe constant: docs URL + 2026-09-29.
Each retry/UNKNOWN branch says why (`// no key: expires_at changes the params; §5 step 4 keeps one open session`,
`// 503: Stripe retries up to 3 days (F-B9)`). Cross-domain SQL named (`// billing.guard_window_open fires on
live.claim_windows; the only billing effect outside billing.*`). `COMMENT ON` every 0079 object.

## Write paths
`migrations/0079_platform_billing.sql`, `internal/billing/*.go` (not `internal/billing/billingtest/**`, which is the
test unit's), `internal/httpapi/{billing.go,billing_test.go}`, `internal/claims/claims.go` (B12 case only),
`internal/httpapi/claims.go` (B12 classifier line only), `cmd/api/{platform_billing.go,platform_billing_test.go}`,
`scripts/ops/grant-r2-permissions.sql`, `output/billing-core/**`. Forbidden: `cmd/api/main.go`,
`internal/httpapi/handler.go`, `internal/platform/**`, `internal/integrations/psp/stripe/**`, 0078, `tests/**`,
`apps/**`, deploy, contracts, OpenAPI, go.mod.

## Verify (unit tests must not start with `TestCustomersBillingCB`)
```sh
GOTOOLCHAIN=go1.27.1 go vet ./... && gofmt -l internal cmd && bash scripts/dev/check-pkgdocs.sh
GOTOOLCHAIN=go1.27.1 go test -race -count=1 ./internal/billing/... ./internal/claims ./internal/httpapi ./cmd/api
LC_FOCUSED_TIMEOUT=1800s bash scripts/dev/test-focused.sh '^Test(T0[46]|LiveClaims|Identity|OwnerProvisioning|Migration|StripeSP|StripeRF0[3-9]|ManualFulfilmentMF0[2-6])'
python3 scripts/check_packet.py
```
Unit tests: `StandingOf` over 8 statuses + empty + mixed, form body bytes via an in-test RoundTripper, webhook
subscription-id resolution, URL absent from logs. Exact-set privilege test failures → list for the integrator.

## Integrator hooks (integrator only)
- `cmd/api/main.go`: `svc, webhook, closeBilling, err := buildPlatformBilling(startup, pool, os.Getenv)`; `defer
  closeBilling()`; `httpapi.Options{…, Billing: svc}`; `handler = mountPlatformBilling(handler, webhook)`.
- `internal/httpapi/handler.go`: `Options.Billing *billing.Service`; `registerBillingRoutes(mux, pool, configured.Billing)`.
- `internal/platform/stripe_runtime.go`: add the `billing.apply_subscription(...)` ABI to the ingress allowlist so any
  other pool holding it fails validation (CB06 negative).
- `deploy/compose.yml` + `deploy/env/*` + `deploy/secrets.manifest.tsv`: B3 names (two secret files per O-D);
  `deploy/caddy` Caddyfile: public route `POST /v1/platform/stripe/webhook` → api (like the PSP webhook).
- `contracts/core-openapi.json`, `contracts/tasks.json` T17; OP01/identity tests "latest `create_initial_store`" →
  0079 (frozen value only); `scripts/dev/depmap.sh` regen. No go.mod change (stdlib).

## Gates
Implementer: unit tests above. Independent (`customers-billing-tests`, red-then-green): CB01 (standing part), CB02
(0079 half), CB06, CB07, CB08, CB09 (billing rows), CB10 (SANDBOX, key-gated), CB11 (billing/studio pages).

## Order / Non-goals / Return
F1 (0078) before PG runs; F2 = this unit + hooks merged. Non-goals: LIVE (BD8), meters/usage ledger, invoice mirror,
River job, tenant-level billing, platform admin UI, ads standing checks (meta-ads). NOT_RUN: CB10 without platform
sandbox key, CB12 LIVE. Return commit SHA, model/reasoning, base, paths, commands + exit codes + counts, evidence,
B1–B12 handling, exact-set test deltas, risks, NOT_RUN. Deviation from the frozen block = stop and escalate.
