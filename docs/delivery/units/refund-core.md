# Unit refund-core — Stripe refund SQL, adapter, worker, merchant service + admin routes

Role: commerce_worker (mid tier). Base = integrator-recorded SHA at dispatch (≥ `8f491dc`).
Worktree `.worktrees/refund-core`, branch `unit/refund-core`. No delegation, network or Stripe key
(RF10 SANDBOX is refund-fulfilment-tests'). Parallel with `fulfilment-core` (disjoint paths).
Contract: `contracts/stripe-refund-v1.md` (FROZEN) incl. both "Integrator rulings" sections; the
"Defaults adopted" below bind unless the integrator overrides.

**Goal:** merchant (`payments:refund`) refunds a captured Stripe attempt, sent once per idempotency key,
tracked to a fact, shown to the buyer. No stock/order/fulfilment side effect (RD6).

## Read (by section; grep headings, `sed -n` ranges)
PROCESS.md; contract §0 table, §2–§8, §10, rulings (§12 is owner-provisioning's). By symbol:
`0061_stripe_psp.sql` (`start_stripe_payment` op insert, `stripe_webhook_prepare`,
`apply_stripe_observation`, `apply_capture`, `hosted_payment_view_v2`, `require_stripe_query`),
`post_river/0012_stripe_payment.sql` (`payment_job_queue`, `stripe_webhook_commit`,
`guard_stripe_receipt_link`), `0027_merchant_orders.sql` (fresh-final-auth), `0060_live_claims.sql:45`;
`internal/payments/{runtime,stripe_runtime,stripe_signal,stripe_query}.go`, `stripewebhook/inbox.go`,
`internal/integrations/psp/stripe/{client,session,webhook}.go` (`call`, `classify`, `CallMeta`, `Event`),
`internal/checkout/payment_view.go` (`dropCancelUnlessStripe`), `internal/httpapi/{handler,claims}.go`
(`scopedAs`, `claimsBody`), `cmd/api/buyer_payment.go`, `internal/platform/{platform,stripe_runtime}.go`.

## Defaults adopted (review P2s the frozen text left open)
- D1 Refund op born like `start_stripe_payment`'s: `UNKNOWN`, `generation=1`, `lease_mode=''`, no lease.
- D2 `stripe_webhook_prepare` = 16 existing args + `p_payment_intent text, p_metadata_refund text`;
  prepare locks the object's counter `FOR UPDATE` and returns IGNORED `signal_cap` at 64; commit
  re-checks, PT409 only on a race. Update the stripe_ingress allowlist in `platform/stripe_runtime.go`.
- D3 List with `ListMatchCount=0 ∧ now < last_sent_at+15 min` → record, snooze to +15 min 5 s, list again.
- D4 0062 adds `identity.read_merchant_refunds(hash bytea, store uuid, order uuid) RETURNS jsonb`
  (owner `commerce_auth`, EXECUTE `commerce_runtime`, `orders:read`, 0027 fresh-final-auth). Item
  state: FAILED/CANCELED > SUCCEEDED > REJECTED facts; else PENDING if pinned; else UNKNOWN if sent ∧
  now ≥ `resend_until`; else SUBMITTING if sent; else REQUESTED. `updated_at` = greatest(requested,
  first/last sent, pinned, max fact received). `commerce_auth` columns add `last_sent_at, pinned_at`.
- D5 Policy names: `checkout_command_result_insert/_select`, `checkout_audit_insert` (+ RESTRICTIVE `…_guard`), `auth_refund_read`, `auth_refund_fact_read`.
- D6 **0062 does not replace `identity.read_merchant_orders`.** Its §7.1 refund fields move to 0063
  with the Go projection; fulfilment-core is the single owner of the merchant projection.
- D7 Charge-branch retrieves keep B1 `load_stripe_credential` (not in §4.6's replace list); after a
  revoking rotation `charge.refunded` reads end UNKNOWN (known limit). Refund-op calls use current head.
- D8 0062 `CREATE OR REPLACE checkout.hosted_payment_view_v2` (same signature) adds the refund states
  and `refund`; Go emits `refund` only for Stripe-attempt orders with refund activity (absent ≡ null),
  so PAYUNi bytes stay identical (B1 ruling 4 precedent).

## FROZEN Go interface (tests, UI and integrator call exactly these)
```go
package stripe // internal/integrations/psp/stripe
type RefundParams struct{ PaymentIntentID, Currency, Reason, RefundRef, AttemptRef string; AmountMinor int64 }
type Refund struct{ ID, Status, FailureReason, PendingReason, Currency, PaymentIntentID, MetadataRefund, MetadataAttempt string; Amount int64; Livemode bool }
type PaymentCharge struct{ PaymentIntentID, ChargeID, Currency string; AmountCaptured, AmountRefunded int64; Refunded, Disputed, Livemode bool }
func RefundIdempotencyKey(refundID string) string            // "lc:stripe:refund:v1:"+uuid
func RefundAmountOK(currency string, amountMinor int64) bool  // twin of payments.stripe_refund_amount_ok
func EncodeRefundBody(p RefundParams) ([]byte, error)       // ruling 18: exact POST bytes, pins body_sha256 before send
func (c *Client) CreateRefund(ctx context.Context, p RefundParams) (Refund, CallMeta, error)
func (c *Client) RetrieveRefund(ctx context.Context, id string) (Refund, CallMeta, error)
func (c *Client) ListRefunds(ctx context.Context, paymentIntentID, startingAfter string) ([]Refund, CallMeta, error)
func (c *Client) RetrievePaymentCharge(ctx context.Context, paymentIntentID string) (PaymentCharge, CallMeta, error)
// Event gains PaymentIntentID, MetadataRefund string; ObjectType admits "refund", "charge".

package merchantorders // new file refunds.go only
type RefundRequest struct{ AmountMinor int64 `json:"amount_minor"`; Reason string `json:"reason"`; ExpectedRefundableMinor int64 `json:"expected_refundable_minor"` }
type RefundResult struct{ RefundID string `json:"refund_id"`; State string `json:"state"`; AmountMinor int64 `json:"amount_minor"`; Currency string `json:"currency"`; RefundableMinor int64 `json:"refundable_minor"` }
type RefundItem struct{ RefundID string `json:"refund_id"`; AmountMinor int64 `json:"amount_minor"`; Reason string `json:"reason"`; State string `json:"state"`
    RequestedAt string `json:"requested_at"`; UpdatedAt string `json:"updated_at"`; FailureReason *string `json:"failure_reason,omitempty"`; StripeRefundID *string `json:"stripe_refund_id"` }
type RefundList struct{ CapturedMinor int64 `json:"captured_minor"`; RefundedMinor int64 `json:"refunded_minor"`; PendingMinor int64 `json:"pending_minor"`
    RefundableMinor int64 `json:"refundable_minor"`; Currency string `json:"currency"`; Items []RefundItem `json:"items"` }
type RefundSignal struct{ RefundID string `json:"refund_id"`; Scheduled bool `json:"scheduled"` }
func RequestRefund(ctx context.Context, tx pgx.Tx, jobs *river.Client[pgx.Tx], scope platform.Scope, token, key, orderID string, in RefundRequest) (RefundResult, error)
func ListRefunds(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, orderID string) (RefundList, error)
func RefreshRefund(ctx context.Context, tx pgx.Tx, jobs *river.Client[pgx.Tx], scope platform.Scope, token, orderID, refundID string) (RefundSignal, error)
var ErrRefundableChanged, ErrExceedsRefundable, ErrAmountStep, ErrNotRefundable, ErrRefundBlockedReview, ErrRefundLimit error // §7.1 codes

package checkout
type OrderRefund struct{ RefundedMinor int64 `json:"refunded_minor"`; PendingMinor int64 `json:"pending_minor"` }
// OrderPayment gains Refund *OrderRefund `json:"refund,omitempty"` (D8)

func registerRefundRoutes(mux *http.ServeMux, pool *pgxpool.Pool, jobs *river.Client[pgx.Tx]) // httpapi/refunds.go; integrator adds Options.RefundJobs, mounts iff non-nil
func newMerchantRefundJobs(pool *pgxpool.Pool) (*river.Client[pgx.Tx], error) // cmd/api/merchant_refund.go: insert-only, Schema "river_payment"; integrator calls it
```
Unchanged: `payments.NewPaymentWorkerClient`, `NewStripeRuntime`, `NewSignalWorker`,
`stripewebhook.NewInbox/NewHandler`. The `payment_refund_v1` worker (args `{operation_id,version:1}`)
is unexported and registered inside `NewPaymentWorkerClient`. Job args stay per-package unexported
copies (repo pattern); InsertTx on queue `default` (deferred `route_payment_queue_v1` routes), no ScheduledAt.

## Build
1. **SQL** `0062_stripe_refund.sql` + `post_river/0013_stripe_refund.sql`: §4.1–§4.6 exactly + D1–D5,
   D8; `pg_get_constraintdef` re-derivations; `COMMENT ON` everything; **no** `store_grants` rows.
2. **Platform + allowlists** (integrator-owned, assigned here): runtime authority admits exactly
   `USAGE river_payment`, `SELECT,INSERT,UPDATE(kind)` on `river_job`, sequence `USAGE`; still rejects
   DELETE/TRUNCATE, any other column UPDATE, every other River schema (reuse the
   `validateStripeAuthority` CASE shape). T06 approved list +6 integration functions (require_/load_/
   mark_…_sent/record_…refund_observation/record_…charge_observation/finish_stripe_refund; count
   23→29, flags per §4.4) and D2's ingress signature. Change existing assertions only to frozen values;
   list every before/after. Never loosen a negative. **Hand steps 1–2 to the integrator first = F1.**
3. **Adapter** §3/§3.1: stdlib only, exact sorted form keys, forbidden keys → `ErrInvalid`, docs URL +
   retrieval date per wire constant, redacted `String()`. Unit tests in `refund_test.go`/`webhook_test.go`
   (names must not start with `TestStripeRF`; RF01/RF02 gates are the independent test unit's).
4. **Worker** `payments/stripe_refund.go`: §6 + D3; SignalWorker refund/charge branches (§4.6);
   `blocked_binding` → LOCAL escalate. Each retry branch comments why same key / why no retry.
5. **Ingress** `stripewebhook`: new prepare args; job `operation_id` = `coalesce(refund_id, attempt_id)`.
6. **Merchant** `merchantorders/refunds.go` (reuse `mapError`, `validAuthorityInput`, `exact`; never edit
   orders.go) + `httpapi/refunds.go` (own classifier via `scopedAs`; §7.1 transport: POST needs
   Idempotency-Key, strict body, no query; refresh keyless, no body; `payments:refund` on POSTs,
   `orders:read` on GET).
7. **Buyer view** `payment_view.go`: states + `refund` per D8.

## Write paths
`migrations/0062_stripe_refund.sql`, `migrations/post_river/0013_stripe_refund.sql`,
`internal/integrations/psp/stripe/{refund.go,refund_test.go,webhook.go,webhook_test.go}`,
`internal/payments/{stripe_refund.go,stripe_refund_test.go,stripe_signal.go,stripe_runtime.go,runtime.go}`,
`internal/payments/stripewebhook/**`, `internal/merchantorders/{refunds.go,refunds_test.go}`,
`internal/checkout/{payment_view.go,payment_view_test.go}`, `internal/httpapi/{refunds.go,refunds_test.go}`,
`cmd/api/{merchant_refund.go,merchant_refund_test.go}`, `internal/platform/{platform.go,stripe_runtime.go}`,
`tests/foundation/{external_operation_authority_test.go,stripe_authority_test.go}` (step 2 only),
`output/refund-core/**`. Forbidden: `orders.go`, `stripetest/**`, other `tests/**`, `handler.go`,
`cmd/api/main.go`, contracts, `core-openapi.json`, `apps/**`, go.mod.

## Verify
```sh
GOTOOLCHAIN=go1.27.1 go vet ./... && gofmt -l internal cmd
GOTOOLCHAIN=go1.27.1 go test -race -count=1 ./internal/integrations/psp/stripe/... ./internal/payments/... ./internal/merchantorders ./internal/checkout ./internal/httpapi ./cmd/api
LC_FOCUSED_TIMEOUT=1800s bash scripts/dev/test-focused.sh '^Test(T06|StripeSP|StripeAuthority|BuyerPayment|MerchantOrders|PaymentCapture|Pool)'
python3 scripts/check_packet.py
```
Logs → `output/refund-core/`. RF01–RF12 are refund-fulfilment-tests' gates; do not claim them.

## Order / Non-goals / Return
F1 (steps 1–2 merged) unblocks fulfilment-core PG runs; full merge + integrator mount = F2. No 0063/0065,
no projection change (D6), no UI/BFF, no LIVE, no cancel-and-release (R-3), no 30-day poller (R-8).
Return commit SHA, model/reasoning, base, paths, commands + exit codes + PASS/FAIL/SKIP counts,
evidence, how each round-3 P2 and D1–D8 was handled, risks, NOT_RUN. Any deviation from the frozen
signatures = stop and escalate.
