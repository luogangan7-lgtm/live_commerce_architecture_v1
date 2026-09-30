# Unit stripe-live-core — 0077 + post-River 0016, LIVE admission in adapter/worker/ingress/API, registrar live-approve/canary/revoke

Role: commerce_worker (mid tier); it also carries the integration_worker paths of contract §12
(`psp/stripe`, `accounts/stripe_crypto.go`) so the adapter and its only caller freeze in one diff.
Base `00c1d94`. Worktree `.worktrees/stripe-live-core`, branch `unit/stripe-live-core`. No delegation,
no network, no Stripe key of any kind (SL08 is stripe-live-tests'; SL-LIVE01/02 are the owner's).
Parallel with `stripe-live-ops`, `legal-pages`, `stripe-live-tests` (authoring) — disjoint paths.
Contract: `contracts/stripe-live-enable-v1.md` (v1 FROZEN 2026-09-30) incl. §15 rulings; the
"Defaults adopted" below bind unless the integrator overrides.

**Goal:** a store can be approved, LIVE-qualified, canaried and opened on Stripe LIVE, enforced in SQL
(LD5), with an always-available per-store kill switch (LD6) and LIVE refunds for every LIVE capture
(LD7). Nothing in this unit moves money or reads a live key.

## Read (by section; grep headings, `sed -n` ranges)
PROCESS.md; `r2-design-rulings.md` (Stripe live defaults + X6); contract §0 table, §3 (all), §4,
§5.1–§5.4, §6, §7, §9, §11 SL01, §13, §15. By symbol / the §5.3 line list: `0061_stripe_psp.sql`
(`stripe_amount_ok`, `stripe_qualification_no_live_check`, `register_stripe_account`, `rotate_stripe_key`,
`stripe_endpoint_account`, `stripe_registrar_credential`, `set_stripe_webhook_endpoint`,
`qualify_stripe_method`, `set_stripe_method` :1570-1630, `require_stripe_registrar_scope`,
`stripe_registry_qualification_lock`), `0062_stripe_refund.sql` (:91, :941-981 `apply_stripe_refund`,
:1211-1236 `hosted_payment_view_v2`, `read_merchant_refunds`), `post_river/0012_stripe_payment.sql`
(:149-281 `start_stripe_payment`, `request_stripe_signal`), `post_river/0013_stripe_refund.sql`
(:427, :533, :640-680), `0065` store-creator grant set; Go: `psp/stripe/{config,client,probe}.go`,
`accounts/stripe_crypto.go:80-95`, `payments/{runtime,stripe_runtime,stripe_query,stripe_refund}.go`,
`stripewebhook/{handler,inbox}.go`, `stripeadmin/registrar.go`, `checkout/hosted.go:80-88`,
`merchantorders/refunds.go:100-176`, `httpapi/refunds.go:25-60`, `platform/stripe_runtime.go:55-72`,
`cmd/stripe-admin/main.go`, `cmd/payment-worker/main.go:60-110`, `cmd/api/{stripe_webhook,buyer_payment}.go`.

## Defaults adopted (P2s the frozen text left open)
- S1 LR-1: post-River file = `migrations/post_river/0016_stripe_live.sql` (0015 is meta-ads').
- S2 LR-3 accepted: `admit` refuses `sk_live_` in LIVE with `stripe_live_key_unrestricted`, checked
  **before** the flag+ref pair. SP01 vector "live key LIVE approved" (config_test.go:37) becomes an
  `rk_live_` vector; add `sk_live_`+approved → `stripe_live_key_unrestricted`. List every changed
  assertion before/after in the return; no other SP01/SP19 vector changes.
- S3 Readiness absence (LD8): each field is a pointer, nil = absent **or JSON null**. Exceptions,
  both only when the parent object is present: `settings.card_payments.statement_descriptor_prefix`
  null or "" → `PrefixLength=0` (Stripe: no prefix set, L7); `requirements.currently_due` `[]` → 0.
  Lengths count runes. `settings.payments.statement_descriptor` null → nil (approval then fails
  closed). `decline_on.{cvc,avs}_failure` absent → nil (SQL requires all 8 keys; fails closed).
- S4 `live-approve` flags: `--approval --connection --expected-version --currency --approval-ref
  --approved-at (RFC3339) --canary-max --max --attest (comma list)`; `approved_by` = `--principal`.
  `--expected-version` unseals the **stored** key exactly like SANDBOX `qualify` (S4 pattern); no key
  input. `live-canary`: `--approval --attempt --refund`. `live-revoke`: `--approval --revoke-ref`.
- S5 Refund environment guard (§5.2): Go cannot read `checkout.payment_attempts.environment` as
  `commerce_runtime`. 0077 adds `identity.merchant_refund_environment(p_token_hash bytea, p_store uuid,
  p_order uuid) RETURNS text` — owner `commerce_auth`, SECURITY DEFINER, `search_path=pg_catalog`,
  EXECUTE `commerce_runtime` only, PUBLIC revoked, same fresh-final-auth prelude as
  `identity.read_merchant_refunds` but permission `payments:refund`; returns the order's attempt
  `environment` (same order and attempt selection as `request_stripe_refund`, 0013:655-663, without its locks) or NULL. Go refuses
  with `ErrNotRefundable` when the result `IS DISTINCT FROM` the deployment environment (NULL included),
  after the replay pre-check, before the River insert. Rejected: a GUC input to
  `request_stripe_refund` (contract keeps that definer profile-free); reading `read_merchant_orders`
  (needs `orders:read`, which a refunder may lack).
- S6 Environment of a profile: `PROVIDER_MOCK`,`SANDBOX` → `SANDBOX`; `LIVE` → `LIVE`; anything else
  is a config error. One Go helper (below); no second copy.
- S7 Additive constructors only: `NewStripeRuntime` and `stripeadmin.Open` keep refusing LIVE
  (existing SP15/SP21 negatives stay); LIVE goes through `NewLiveStripeRuntime` / `OpenLive`.
- S8 `LiveRevoke` and `SetMethod(Enabled=false)` work on a registrar from `Open` (no pair; LD6). SQL
  alone decides LIVE `method --enabled` (no pair in Go for `method`).
- S9 Any `'SANDBOX'` literal or LIVE refusal found outside the §5.3 inventory = stop and escalate;
  never widen it silently.

## FROZEN Go interface (tests, ops and integrator call exactly these)
```go
package stripe // internal/integrations/psp/stripe
// admit: rk_live_ only in LIVE (+pair); sk_live_ → refuse(ErrLiveRefused,"stripe_live_key_unrestricted").
// ProbeCheckout: allowed on a LIVE-admitted client; requires expired ∧ unpaid ∧ livemode==(env=="LIVE").
type Readiness struct {
    ChargesEnabled, PayoutsEnabled, DetailsSubmitted, CVCRule, AVSRule *bool
    CurrentlyDueCount, DescriptorLength, PrefixLength                  *int
} // String/GoString/MarshalJSON print "stripe.Readiness{redacted}"; only the L12 fields are parsed (no PII)
func (c *Client) AccountReadiness(ctx context.Context) (Readiness, CallMeta, error) // GET /v1/account; id must equal cfg.AccountID (as VerifyAccount); any env
func (r Readiness) JSON() ([]byte, error) // exact 8 keys ChargesEnabled…AVSRule, sorted; any nil → refuse(ErrUncertain,"stripe_live_readiness_unknown")

package payments
func NewLiveStripeRuntime(ctx context.Context, pool *pgxpool.Pool, keys *accounts.Keyring,
    live stripe.LiveApproval) (*StripeRuntime, error) // profile "LIVE", real transport only, pair validated like admit
func ProfileEnvironment(profile string) (string, bool) // S6

package stripewebhook // NewInbox(…, "LIVE") and NewHandler admit LIVE; signatures unchanged

package stripeadmin
func OpenLive(ctx context.Context, dsn string, apiKeys, signingKeys *accounts.Keyring,
    live stripe.LiveApproval) (*Registrar, error) // Register/Rotate register environment LIVE (rk_live_ only); Qualify/SetWebhookEndpoint admit Profile "LIVE"
type LiveApproveInput struct {
    ApprovalID, ConnectionID, Currency, ApprovalRef string
    ApprovedAt                                      time.Time
    ExpectedVersion, CanaryMaxMinor, MaxMinor       int64
    Checklist                                       []string
}
var LiveChecklist = []string{"account_active", "canary_private", "descriptor", "dispute_notice", "managed_off",
    "payout_bank", "policy_pages", "radar_default", "rak_live", "three_ds", "webhook_live"} // §9, sorted
func (r *Registrar) LiveApprove(ctx context.Context, s Scope, in LiveApproveInput) (string, error)      // OpenLive only; AccountReadiness → payments.approve_stripe_live
func (r *Registrar) LiveCanary(ctx context.Context, s Scope, approvalID, attemptID, refundID string) (time.Time, error) // OpenLive only; payments.record_stripe_live_canary
func (r *Registrar) LiveRevoke(ctx context.Context, s Scope, approvalID, revokeRef string) (time.Time, error)         // Open or OpenLive; payments.revoke_stripe_live
// Errors stay ErrConfig/ErrDatabase/ErrRejected/ErrProvider; SQLSTATE 22023/42501/PT409 → ErrRejected.

package merchantorders
func RequestRefundIn(ctx context.Context, tx pgx.Tx, jobs *river.Client[pgx.Tx], scope platform.Scope,
    environment, token, key, orderID string, in RefundRequest) (RefundResult, error) // S5 guard, then RequestRefund's body
// RequestRefund(…) unchanged signature ≡ RequestRefundIn(…, "SANDBOX", …)

package httpapi
func registerRefundRoutesIn(mux *http.ServeMux, pool *pgxpool.Pool, jobs *river.Client[pgx.Tx], environment string) // env ∉ {SANDBOX,LIVE} → not mounted
// registerRefundRoutes(mux,pool,jobs) ≡ registerRefundRoutesIn(…, "SANDBOX")
```
CLI (`cmd/stripe-admin`): `register|rotate --environment SANDBOX|LIVE` (default SANDBOX); `webhook|qualify
--profile LIVE`; new `live-approve|live-canary|live-revoke` (S4). LIVE register/rotate/webhook/qualify,
`live-approve`, `live-canary` require `COMMERCE_STRIPE_LIVE_ENABLED=1` + valid
`COMMERCE_STRIPE_LIVE_APPROVAL_REF` in the env; `live-revoke`, `method` never read them. Output stays one
JSON line of ids/versions/booleans; stderr one fixed code.

## Build
1. **SQL 0077** (§3.1–§3.4 exactly + S5): widened CHECKs re-derived from `pg_get_constraintdef`
   (0061/0062 pattern); `stripe_min_minor`; `stripe_live_approvals` + set-once/immutable trigger + FORCE
   RLS + policies; `account_qualification_revoke_only` trigger (all rows, all roles); revoke policy;
   three new definers + their audits; re-created 0061/0062 functions with body deltas only (same
   signature/owner/grants/`proconfig`); lock order account → binding → approval → qualification →
   method head. `COMMENT ON` every new/changed object (owner package, roles, non-goals). No DELETE grant.
2. **SQL post_river/0016** (S1): `start_stripe_payment`, `request_stripe_signal`, the two observation
   recorders, `request_stripe_refund` — deltas of §3.4 only.
3. **Adapter** `psp/stripe`: `admit` (S2), `probe.go:46,76`, `readiness.go` (S3; strict decode of the
   listed paths only, docs URL + retrieval date per field), `accounts/stripe_crypto.go` scope rules.
   Unit gate **SL01** `TestStripeSL01LiveConfig` in `psp/stripe/live_test.go` and
   `accounts/stripe_live_test.go` (both packages, same top-level name): §11 SL01 list. Record red first
   (run SL01 against base `admit` → `sk_live_` admitted) then green.
4. **Worker/ingress/checkout**: §5.2 `cmd/payment-worker` + `NewLiveStripeRuntime`,
   `validStripeSnapshot`/`stripeSessionIdentity`/refund loader env rules (`stripe_query.go:236`,
   `stripe_refund.go:116,122`, `stripe_runtime.go:48,106`); `stripewebhook` LIVE; `hosted.go:84`.
   Each changed guard comments the rule (`// LD1: env = profile env`), each retry branch keeps its
   why-comment. Package-internal unit tests (names must not start with `TestStripeSL`).
5. **Registrar + CLI**: `OpenLive`, `LiveApprove/Canary/Revoke`, `registrar.go:39,327,377,387`,
   `main.go:163` + new subcommands in `cmd/stripe-admin/live.go`; `platform/stripe_runtime.go`
   `stripeRegistrarFunctions` + the 3 new signatures (exact `regprocedure` text) — registrar list only;
   billing-core's ingress-list line in the same file is an integrator hook applied after your merge. `cmd/stripe-admin` unit tests
   extend `main_test.go` table (usage/config refusals, no pair → refused, revoke without pair admitted).
6. **API**: `cmd/api/stripe_webhook.go:71` + `buyer_payment.go:65` admit LIVE only with the pair;
   `merchantorders.RequestRefundIn` + `httpapi.registerRefundRoutesIn` (S5/S6); `cmd/api/stripe_live.go`
   holds `loadStripeLiveApproval(getenv) (stripe.LiveApproval, error)` (both-or-neither, pattern) used by
   both config loaders. `cmd/api` still never reads `STRIPE_SECRET_KEY` (SP15).

## Write paths
`migrations/0077_stripe_live_enable.sql`, `migrations/post_river/0016_stripe_live.sql`,
`internal/integrations/psp/stripe/{config.go,config_test.go,probe.go,probe_test.go,readiness.go,live_test.go}`,
`internal/integrations/accounts/{stripe_crypto.go,stripe_live_test.go}`,
`internal/payments/{stripe_runtime.go,stripe_query.go,stripe_refund.go,runtime.go,live_env_test.go}`,
`internal/payments/stripewebhook/{handler.go,inbox.go,live_test.go}`,
`internal/payments/stripeadmin/{doc.go,registrar.go,live.go,live_test.go}`,
`internal/checkout/{hosted.go,hosted_live_test.go}`, `internal/merchantorders/{refunds.go,refunds_env_test.go}`,
`internal/httpapi/{refunds.go,refunds_env_test.go}`, `internal/platform/stripe_runtime.go`,
`cmd/stripe-admin/{main.go,live.go,main_test.go}`, `cmd/payment-worker/{main.go,main_test.go}`,
`cmd/api/{stripe_webhook.go,buyer_payment.go,stripe_live.go,stripe_live_test.go}`, `output/stripe-live-core/**`.
Forbidden: `internal/httpapi/handler.go`, `cmd/api/main.go`, `deploy/**`, `docs/runbooks/**`,
`tests/**`, `stripetest/**`, contracts, `core-openapi.json`, `apps/**`, go.mod/go.sum, 0061/0062/0012/0013.

## Comments / dependency annotations (PROCESS §5, binding)
Package comments of `stripeadmin` and `psp/stripe` updated (owns / never; `api.stripe.com` GET
`/v1/account` now also for readiness). Every cross-domain SQL call site names the definer and why
(`// payments.approve_stripe_live: LD2 owner grant predicate + LD8 readiness, one audit row`). Money
comparisons `// I05:`; every wire field in `readiness.go` has docs URL + retrieval date. Every new table,
column, function, trigger, policy: `COMMENT ON` (owning package, allowed roles, non-goals). No hand-written
"used by" lists; the integrator regenerates `docs/engineering/dependency-map.md`.

## Gates
Implementer: SL01 (UNIT, red → green) + package unit tests. Independent (stripe-live-tests, not you):
SL02–SL06, SL08, SL09. Integrator: SL07 regression. Owner: SL-LIVE01/02.

## Verify
```sh
GOTOOLCHAIN=go1.27.1 go vet ./... && gofmt -l internal cmd
GOTOOLCHAIN=go1.27.1 go test -race -count=1 ./internal/integrations/psp/stripe/... ./internal/integrations/accounts ./internal/payments/... ./internal/checkout ./internal/merchantorders ./internal/httpapi ./cmd/stripe-admin ./cmd/payment-worker ./cmd/api
LC_FOCUSED_TIMEOUT=2400s bash scripts/dev/test-focused.sh '^Test(T06|StripeSP|StripeRF|StripeAuthority|BuyerPayment|MerchantOrders|PaymentCapture|Pool)'
python3 scripts/check_packet.py && bash scripts/dev/check-pkgdocs.sh
```
Logs → `/Volumes/data/live_commerce_architecture_v1/output/stripe-live-core/` (command, SHA, exit, counts).

## Integrator hooks (only the integrator edits these; the unit exposes the functions)
- `internal/httpapi/handler.go`: `Options` gains `PaymentEnvironment string`; line 134 becomes
  `registerRefundRoutesIn(mux, pool, configured.RefundJobs, configured.PaymentEnvironment)`.
- `cmd/api/main.go`: `env, ok := payments.ProfileEnvironment(getenv("COMMERCE_PAYMENT_PROFILE"))`; refuse
  start on `!ok` when refunds are mounted; pass `PaymentEnvironment: env`.
- `deploy/compose.yml` and runbooks: see `stripe-live-ops.md` Integrator hooks.
- Migration merge (0077, post_river/0016), `dependency-map.md` regen, `contracts/tasks.json` evidence.
- OpenAPI, Caddyfile, go.mod, apps/admin nav: no change (§5.4 no new route; no dependency).

## Order / Non-goals / Return
F1 = steps 1–2 handed to the integrator first (unblocks SL02/SL03 PG runs); F2 = full merge + hooks.
No UI, no LIVE call, no watchdog/deploy/runbook edits, no forced session expiry (§13), no Connect.
Return commit SHA, model/reasoning, base, paths, commands + exit codes + PASS/FAIL/SKIP, SL01 red/green
logs, how S1–S9 were applied, SP01 before/after list, risks, NOT_RUN (SL02–SL09 are not yours). Any
deviation from the frozen block = stop and escalate.
