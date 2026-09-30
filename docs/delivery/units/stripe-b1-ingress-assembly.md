# Unit stripe-b1-ingress-assembly — webhook handler, process assembly, registrar

Role: integration_worker. Base `b563bb5`. Worktree `.worktrees/stripe-b1-ingress-assembly`, branch
`unit/stripe-b1-ingress-assembly`. No delegation, no real Stripe call.
**PG dependency:** `stripe-b1-pool-fix` (P1-A/B) must merge first, else real ingress/registrar logins
fail `platform` admission (expected red). Parallel with `stripe-b1-start-http` (disjoint paths).

**Goal:** `POST /v1/stripe/webhook/{endpoint_id}` admits verified events atomically (SP13); the
API mounts it; payment-worker assembles `StripeRuntime` (SP15); `cmd/stripe-admin` is the only
Stripe account/key/endpoint/qualification/method writer (SP21).

## Read (by section)
PROCESS.md; contract §0.2 (overrides §9.1 route/env, §12 API/worker env, §13), §5.2, §5.8, §6.4
webhook/registrar rows, §8 Assembly, §9.1, §11–13, §15. Symbols: `cmd/api/meta.go` (`*Meta*`),
`meta/{handler,inbox}.go`, `cmd/payment-worker`, `payments/*runtime.go`, `accounts/stripe_crypto.go`.

## FROZEN Go interface
```go
package stripewebhook // internal/payments/stripewebhook
var ErrConfig, ErrDatabase error // "stripewebhook: invalid config" / "stripewebhook: database unavailable"
type Inbox struct{ /* borrowed ingress pool, insert-only River client, signing keyring, profile; redacted */ }
func NewInbox(ctx context.Context, ingressPool *pgxpool.Pool, signingKeys *accounts.Keyring,
    profile string) (*Inbox, error) // PROVIDER_MOCK|SANDBOX; platform.ValidateStripeIngressPool
func NewHandler(inbox *Inbox) (http.Handler, error) // only entry; nothing accepts an unverified event
package stripe // psp/stripe, new probe.go — adapter amendment, pending integrator ruling
func (c *Client) ProbeCheckout(ctx context.Context, qualificationID, currency string,
    amountMinor int64, returnURL string) (sessionID string, meta CallMeta, err error)
package stripeadmin // internal/payments/stripeadmin
var ErrConfig, ErrDatabase, ErrRejected, ErrProvider error // fixed "stripeadmin: <word>"
type Scope struct{ TenantID, StoreID, PrincipalID string }
type EndpointInput struct{ ConnectionID, EndpointID, AccountID, Profile string; ExpectedVersion int64 // EndpointID "" iff ExpectedVersion==0
    Enabled bool; Secrets accounts.StripeWebhookSecrets }
type QualifyInput struct{ ConnectionID, AccountID, SecretKey, Profile, Currency, ReturnURL string; ExpectedVersion, AmountMinor int64 }
type MethodInput struct{ MarketID, Country, ConnectionID, QualificationID string; ExpectedVersion int64
    Enabled, Visible bool; Sort int32; MinMinor, MaxMinor int64; NameHans, NameHant, NameEN string }
type Registrar struct{ /* owned pool, keyrings, optional transport; redacted */ }
func Open(ctx context.Context, dsn string, apiKeys, signingKeys *accounts.Keyring,
    mockTransport ...http.RoundTripper) (*Registrar, error) // platform.OpenStripeRegistrarPool
func (r *Registrar) Close()
func (r *Registrar) Register(ctx context.Context, s Scope, accountID, secretKey string) (connectionID string, err error)
func (r *Registrar) Rotate(ctx context.Context, s Scope, connectionID string, expectedVersion int64, accountID, secretKey string) (int64, error)
func (r *Registrar) SetWebhookEndpoint(ctx context.Context, s Scope, in EndpointInput) (endpointID string, version int64, err error)
func (r *Registrar) Qualify(ctx context.Context, s Scope, in QualifyInput) (qualificationID string, err error)
func (r *Registrar) SetMethod(ctx context.Context, s Scope, in MethodInput) (int64, error)
```
```go
package main // cmd/api — seams brief 3 calls
func loadStripeWebhookConfig(getenv func(string) string, addr string) (stripeWebhookConfig, error)
func buildStripeWebhookHandler(ctx context.Context, mainPool *pgxpool.Pool, c stripeWebhookConfig) (http.Handler, func(), error)
func mountStripe(fallback, stripeHandler http.Handler) http.Handler // errStripeConfig="stripe_api_invalid_config", errStripeDatabase="stripe_api_database_unavailable"
package main // cmd/payment-worker: loadConfig/run unchanged; workerConfig.stripe bool; errWorkerStripe="payment_worker_stripe_unavailable"
package main // cmd/stripe-admin
func run(ctx context.Context, args []string, getenv func(string) string, stdout io.Writer) error
```
`mockTransport`: exactly one non-nil → `stripe.NewWithMockTransport` (tests only), else `stripe.New`.

## Webhook handler (SQL: `payments.stripe_webhook_material`, `_prepare`, `_commit`)
Checks in order: method≠POST 405 `method_not_allowed`+`Allow: POST`; path not exactly
`/v1/stripe/webhook/<canonical lowercase uuid>` 404 `not_found`; any query incl. bare `?` 400
`invalid_request`; content-type/encoding 415 `unsupported_media_type`; >32 in flight (non-blocking)
503 `busy`+`Retry-After: 5`; body >256 KiB 413 `payload_too_large`. Request deadline 5 s, each DB
tx 2 s, `Cache-Control: no-store`, bodies exactly `{"received":true}` / `{"error":"<code>"}`.
Material: no row, or `execution_profile`≠configured profile → 404 `not_found`;
`OpenStripeWebhook(StripeWebhookScope{row, EndpointID, Profile, KeyVersion})` failure → 503
`signing_unavailable`; `NewWebhookVerifier{Secrets,AccountID,Environment}.Verify(raw, header,
time.Now())` failure → 400 `invalid_signature`. One tx: `prepare(endpoint,key_version,event…)`
(stale key_version SQLSTATE 40001 → 503 `unavailable`); only for `ACCEPT_PENDING`: `InsertTx
payment_signal_v1{operation_id:attempt_id,signal_id:<prepare's signal_id>,version:1}` on
`jobqueue.ForProfile(endpoint profile)`, then `commit(receipt,signal,job)`; COMMIT; then 200. DB
error/cancel → 503 `unavailable`; no early/goroutine ACK. Logs: fixed code + endpoint UUID only.

## Process wiring
- **API** (`cmd/api/stripe_webhook.go`; `main.go` loads after Meta and wraps `mountStripe` after
  `mountMeta`, reserving raw and `path.Clean` `/v1/stripe`): `COMMERCE_STRIPE_WEBHOOK_ENABLED`
  (""/0/1; disabled reads nothing else); enabled needs a private listener,
  `COMMERCE_STRIPE_INGRESS_DATABASE_URL`, `COMMERCE_PAYMENT_PROFILE`, and a signing keyring from
  `accounts.LoadKeyring` with names remapped `COMMERCE_ACCOUNT_*`→`COMMERCE_STRIPE_WEBHOOK_*`.
  `platform.ValidateSameDatabase` as Meta. The API reads no `STRIPE_*`.
- **Worker**: `COMMERCE_STRIPE_ENABLED` read only when the worker is enabled; with LIVE → config
  error; 1 → `payments.NewStripeRuntime(ctx,pool,keys,profile)` + `NewPaymentWorkerClient(…,
  WorkerConfig{…,Stripe:rt})`. Reads no `STRIPE_*`/whsec.
- **Registrar CLI** (all take `--tenant --store --principal`; stdout one JSON line of IDs/versions,
  never secrets; exit 1 + fixed code): `register`; `rotate --connection --expected-version`;
  `webhook --connection [--endpoint] --profile --expected-version --enabled`; `qualify --connection
  --expected-version --profile --currency --amount-minor --return-url`; `method` (one flag per
  `MethodInput` field). Env: `COMMERCE_STRIPE_REGISTRAR_DATABASE_URL`; `COMMERCE_ACCOUNT_*`
  (register/rotate); `COMMERCE_STRIPE_WEBHOOK_*` + `STRIPE_WEBHOOK_SECRET[_NEXT]` (webhook);
  `STRIPE_SECRET_KEY` + `STRIPE_ACCOUNT_ID` (register/rotate/SANDBOX qualify). LIVE refused.
- **Registrar SQL**: register = `VerifyAccount` → new connection+binding UUIDs → `SealStripeAPI`
  v1 → `integration.register_stripe_account`; rotate = verify → seal `expected+1` →
  `integration.rotate_stripe_key`; webhook = seal key_version `expected+1` →
  `payments.set_stripe_webhook_endpoint` (disable re-seals too); qualify SANDBOX = `ProbeCheckout`
  (lc_probe create at now+31m → expire → retrieve expired+unpaid+!livemode, key
  `lc:stripe:probe:v1:<qualification>`) → `payments.qualify_stripe_method(…,'stripe-probe:<id>',
  observed, observed+30d)`; qualify PROVIDER_MOCK = no network, evidence
  `provider-mock:<qualification>`; method = `payments.set_stripe_method`. SQLSTATE
  22023/PT409/42501 → `ErrRejected`; adapter errors → `ErrProvider`.

**Comments (PROCESS.md §5):** package docs owns / never / Depends on (incl. `api.stripe.com`) /
Used by; each SQL call names definer + why; each 503 says why Stripe retries; probe params carry
docs URL + retrieval date. No new module dependencies.

## Write paths
`internal/payments/{stripewebhook,stripeadmin}/**`, `internal/integrations/psp/stripe/probe{,_test}.go`,
`cmd/api/stripe_webhook{,_test}.go`, `cmd/api/main.go` (wiring lines), `cmd/payment-worker/main.go`,
`cmd/payment-worker/stripe_config_test.go`, `cmd/stripe-admin/**`, `output/stripe-b1-ingress-assembly/**`.
Never `cmd/*/stripe_sp*_test.go` (brief 3), `internal/platform/**` (pool-fix), SQL, checkout,
buyerhttp, `cmd/api/buyer*.go`, tests/foundation, go.mod.

## Verify
```sh
GOTOOLCHAIN=go1.27.1 go test -race -count=1 ./internal/payments/... ./internal/integrations/psp/stripe/... ./cmd/...
GOTOOLCHAIN=go1.27.1 go vet ./internal/payments/... ./cmd/... && gofmt -l internal cmd
bash scripts/dev/test-focused.sh '^Test(BuyerPaymentWorker|MetaRuntime|StripeSP(06|11|13)|StripeAuthority)'  # serialized
```

**Non-goals / Return:** no checkout/buyer routes, UI, refunds, LIVE, SQL changes, key caches, Connect, SP16/17 runs.
Return commit SHA, model/reasoning, base, paths, commands + exit codes + counts, evidence, risks,
NOT_RUN (SANDBOX probe). Any deviation from frozen signatures = stop and escalate.
