# Unit stripe-b1-start-http — Stripe start/handoff/signals in checkout + buyer routes

Role: commerce_worker. Base `b563bb5`. Worktree `.worktrees/stripe-b1-start-http`, branch
`unit/stripe-b1-start-http`. No delegation, network or Stripe key. Parallel with
`stripe-b1-ingress-assembly` (disjoint paths, no compile dependency).

**Goal:** buyer prepares `stripe_checkout`, polls the view, takes a repeatable REDIRECT handoff and
requests refresh/cancel via the existing hosted service and `/v1/buyer` routes. PAYUNi behavior
and bytes stay identical. No provider I/O in the API process (SP07 "zero calls").

## Read (by section; grep headings, sed ranges)
`docs/delivery/PROCESS.md`; `contracts/stripe-psp-v1.md` §0.2 (overrides), §4, §6.4 rows
start/take/view_v2/request_signal, §7 (reservation table), §9.2, §9.3, §11 (buyer prepare row),
§12 (`COMMERCE_STRIPE_CHECKOUT_ENABLED`). Code by symbol: `internal/checkout/hosted.go`
(`NewHostedPaymentStarter`, `BeginHosted`, `TakeHosted`), `payment.go` (`startPaymentTx`,
`lockPaymentKey`, `readPaymentReceipt`), `payment_view.go`, `internal/buyerhttp/handler.go`
(`matchRoute`, `allowed`, `noReplayKey`, `classify`), `payment.go`, `cmd/api/buyer_payment.go`.

## FROZEN Go interface (brief 3 tests call exactly these)
```go
package checkout
type StripeHostedConfig struct{ ReturnURL string } // https, validated like psp/stripe return URL
func (c StripeHostedConfig) CanonicalDigest() (StripeHostedConfig, [32]byte, error)
// digest = sha256(json{"version":"stripe-hosted-v1","return_url","api_version":stripe.APIVersion,
//   "session_ttl_seconds":2400,"send_window_seconds":420,"handoff_margin_seconds":300}) (§9.2)
type HostedProviders struct{ PAYUNi *HostedConfig; Stripe *StripeHostedConfig }
func NewHostedPaymentService(ctx context.Context, hostedPool *pgxpool.Pool,
    jobs *river.Client[pgx.Tx], profile string, keys *accounts.Keyring,
    p HostedProviders) (*HostedPaymentStarter, error)
// NewHostedPaymentStarter(ctx,pool,jobs,profile,keys,cfg) keeps its signature and ==
// NewHostedPaymentService(..., HostedProviders{PAYUNi:&cfg}).
type PaymentSignal struct{ OrderID string `json:"order_id"`; Scheduled bool `json:"scheduled"` }
func (s *HostedPaymentStarter) RefreshPayment(ctx context.Context, token, storeID, orderID string) (PaymentSignal, error)
func (s *HostedPaymentStarter) CancelPayment(ctx context.Context, token, storeID, orderID string) (PaymentSignal, error)
// Field additions (existing types):
//   HostedHandoff.RedirectURL  string `json:"redirect_url,omitempty"`
//   OrderPayment.CancelRequested *bool `json:"cancel_requested,omitempty"`
```
Constructor errors: `command.ErrInvalid` (nil ctx/jobs, both providers nil, keys nil while
PAYUNi set, invalid digest, Stripe with profile outside PROVIDER_MOCK|SANDBOX); pool errors from
`platform.ValidateHostedPool` unchanged. Method errors go through existing `safeError` →
`command.ErrNotFound|ErrConflict|ErrInvalid`, `buyer.ErrUnauthorized`. Refresh/Cancel on a
service without Stripe → `command.ErrNotFound`. `buyerhttp.New` signature is unchanged.

## Behavior (SQL each Go call uses)
- **BeginHosted**, `MethodCode=="stripe_checkout"` (admitted only when Stripe set): request
  digest = sha256(json{input,profile,config_digest:hex(stripeDigest)}); PAYUNi digest stays the
  exact old bytes so pre-change receipts replay. Reuse `lockPaymentKey`/`readPaymentReceipt`/
  `checkCapability`/`validPaymentResult`. New attempt: `gen_random_uuid()`, River `InsertTx`
  `payment_query_v1{operation_id,version:1}` on `jobqueue.ForProfile`, **no ScheduledAt delay**
  (§8 "ScheduledAt=now"), then `checkout.start_stripe_payment($hash,$store,$key,$reqhash,$order,
  $method,$version,$profile,$attempt,$job,$locale,$stripeDigest,$returnURL)`. No
  `save_hosted_page`, no keyring use, no provider call.
- **TakeHosted** with Stripe set: SAVEPOINT; `checkout.take_stripe_handoff($hash,$store,$order,
  $profile,$stripeDigest)`. Only on SQLSTATE `PT409` with message exactly
  `Stripe attempt unavailable` roll back to the savepoint and run the unchanged PAYUNi
  `take_hosted_page` path; every other error returns as is. Stripe result decodes strictly into
  `HostedHandoff{order_id,disposition∈CREATING|REDIRECT|CLOSED|UNAVAILABLE,expires_at,
  redirect_url?}`; `redirect_url` present iff REDIRECT and matches
  `^https://checkout\.stripe\.com/[!-~]{1,4000}$` in every profile (the fake and the SQL CHECK
  already use that host). Never log or persist the URL. Stripe nil → existing code path only.
- **PaymentView**: Stripe nil → existing `hosted_payment_view` (bytes unchanged, CancelRequested
  nil). Stripe set → `checkout.hosted_payment_view_v2($hash,$store,$order,$profile,
  $payuniDigest|NULL,$stripeDigest)`; validator accepts ≤2 methods (payuni_credit,
  stripe_checkout), `payment_state` + `CLOSED_UNPAID`, handoff states NONE|CREATING|READY|CLOSED|
  UNAVAILABLE, requires `cancel_requested`.
- **Refresh/Cancel** (kind `REFRESH`/`CANCEL`) in one `buyer.WithScope` tx: attempt id =
  `response->>'attempt_id'` of the scoped `checkout.command_results` row
  (`operation='checkout.payment.start' AND order_id=$order`). No row → scoped `checkout.orders`
  probe: absent → `ErrNotFound`, present → `ErrConflict`. Signal id `gen_random_uuid()`;
  `InsertTx payment_signal_v1{operation_id,signal_id,version:1}` on the profile queue with **no
  ScheduledAt** (SQL requires state `available`, attempt 0); then
  `checkout.request_stripe_signal($hash,$store,$order,$profile,$stripeDigest,$kind,$signal,$job)`.
  `scheduled=false` → roll the whole tx back (no orphan job) and return `{order_id,false}`.
  The SQL fence rechecks exact args, so a wrong attempt id can only fail closed.

## buyerhttp routes (private; same auth, CSRF-free bearer model as handoff)
Add `POST /v1/buyer/orders/{id}/payment/refresh` and `.../payment/cancel`: matched before
`/payment`; keyless (`noReplayKey`), strict no body, JSON `PaymentSignal`. Prepare admits
`method_code∈{payuni_credit,stripe_checkout}`; other fields/validation unchanged. Handoff returns
`HostedHandoff` as is. 404 for foreign/other-store orders via existing classify.
**Public BFF mirror (requirements only, ui_worker R1-2 implements):** `/api/buyer/` mirrors the
two new routes with the handoff keyless-POST exception (CSRF/Origin/context kept), validator
deltas of §9.2 (URL regex, enums, `cancel_requested`, methods ≤2), nonretryable refresh/cancel
errors, handoff only on explicit click. Do not edit `apps/**` in this unit.

## cmd/api wiring
`cmd/api/buyer_payment.go`: nested flag `COMMERCE_STRIPE_CHECKOUT_ENABLED` (""/0/1) read only
after buyer payment is enabled; when 1 → `StripeHostedConfig{ReturnURL: COMMERCE_PAYMENT_RETURN_URL}`,
profile must be PROVIDER_MOCK|SANDBOX, then `NewHostedPaymentService`. PAYUNi config stays
required (no Stripe-only deployment in B1). Never read `STRIPE_*`.

**Comments (PROCESS.md §5):** file headers owns / never / Depends on (checkout SQL, `river_payment`)
/ Used by (buyerhttp, cmd/api); each SQL call names the definer + why (e.g. `// checkout.
request_stripe_signal: DB throttle + set-once cancel; never provider I/O inline`); retry notes say
why not. No new module dependencies.

## Write paths
`internal/checkout/stripe.go`, `internal/checkout/stripe_test.go` (UNIT only),
`internal/checkout/hosted.go`, `internal/checkout/payment_view.go` (minimal seams),
`internal/buyerhttp/handler.go`, `internal/buyerhttp/payment.go`,
`internal/buyerhttp/{payment,handler}_test.go` (route/validator unit cases),
`cmd/api/buyer_payment.go`, `cmd/api/buyer_payment_test.go`, `output/stripe-b1-start-http/**`.
No SQL, contracts, `apps/**`, `cmd/api/main.go`, `tests/foundation/**`, go.mod.

## Verify
```sh
GOTOOLCHAIN=go1.27.1 go test -race -count=1 ./internal/checkout ./internal/buyerhttp ./cmd/api
GOTOOLCHAIN=go1.27.1 go vet ./internal/checkout ./internal/buyerhttp ./cmd/api && gofmt -l internal cmd
bash scripts/dev/test-focused.sh '^Test(BuyerPayment|BuyerHTTP|StripeSP21Pinned)'   # regression; serialized machine-wide
```
Logs → `output/stripe-b1-start-http/`. SP07/SP14 are brief 3's gates: do not claim them.

## Non-goals / Return
No webhook, worker, registrar, UI/BFF code, refunds, LIVE, new SQL, rate limiting. Return commit
SHA, model/reasoning, base, paths, commands + exit codes + PASS/FAIL/SKIP counts, evidence, risks,
NOT_RUN. Any deviation from the frozen signatures = stop and escalate.
