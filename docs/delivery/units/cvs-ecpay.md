# Unit cvs-ecpay — ECPay wire adapter, logistics keyring, directory cache, `ecpay.cvs_create` dispatcher route

Role: integration_worker (mid tier). Base SHA `00c1d94` (+ ads-a10 F1 and integrator F0 below before step 3),
SHA recorded at dispatch. Worktree `.worktrees/cvs-ecpay`, branch `unit/cvs-ecpay`. No delegation, no
network (ECPay only through `httptest` in unit tests), no ECPay key (stage keys are cvs-tests' TCV07 env).
**Wave 1**, parallel with cvs-core and cvs-ui against the FROZEN blocks here and in `cvs-core.md`.
Contract: `contracts/taiwan-cvs-logistics-v1.md` (v1 FROZEN 2026-09-30) incl. §14–§15 and
`r2-design-rulings.md` X1/X5/X6/X8/X9. Release write-set: cvs owns 0072–0073, `post_river/0017`,
`internal/fulfillment` cvs files, `internal/integrations/shipping/ecpay/**`, storefront/admin CVS pages; auth
(0070, identity password/mail, `internal/mail`), ads (0074/0075/0080, `post_river/0015`, `internal/ads`,
`internal/integrations/{core,meta_ads}`, `cmd/ads-worker`), stripe-live (0077, `post_river/0016`),
customers-billing (0078–0079, `internal/{customers,billing}`), U08 purge (0071) are other units' — never touch.

Path note: the task sheet said `internal/integrations/logistics/ecpay`; the FROZEN contract §7 says
`internal/integrations/shipping/ecpay`. The contract wins; the integrator may rename before dispatch.

**Goal:** stdlib-only ECPay 物流整合 v1 client (MAC, map form, GetStoreList cache, connect probe, Create,
Query/V5, print form, status parse) plus the lease-fenced dispatcher route that buys a label exactly once
per operation and never redispatches an UNKNOWN.

## Read (by section; `grep -n` headings, `sed -n` ranges)
PROCESS.md; contract §0 TD1/TD3/TD7/TD8, §0.2 R-1 + R-7, §1 F1–F12 + F17–F21, §4.2 credential paragraph,
§4.3 rows `load_cvs_create`, `finish_cvs_create`, API-side key loaders, `ecpay_recipient_ok`, `request_cvs_shipment`
(trade-no formula only), §6, §7 (all), §9, §10 TCV01/05/06/07/10 (what the tests assert), §12, §16.3.
Code by symbol: `internal/integrations/core/{dispatcher.go (DispatchRoute, runOperation, loadSecret,
completeOperation), secret.go (SecretClaim, Secret, zero), service.go (Outcome, Operation)}`,
`internal/integrations/metareply/{routes.go (Routes, loader error → ErrPolicyDenied mapping), keyring.go
(LoadPageTokenKeyring, Seal, Open — shape to copy)}`, `internal/integrations/psp/stripe/client.go` (`call`,
`classify`, redacted `String()`, body cap), `cmd/claims-worker/main.go:120-165` (route list; read only).

## F0 (integrator, lands before step 3; unit must not edit `internal/integrations/core`)
After ads-a10 (F1 of ads; delivers `ReconcileWithSecret`, `DispatchRequest.Mode`, R-7b) merges, the
integrator lands R-7a (contract §0.2, §11 "integrator") with one dispatcher test each:
```go
package core
// Outcome gains:        Detail any `json:"-"`            // route-private; ignored by Complete
// SecretClaim gains:    Mode string                      // "dispatch"|"reconcile" = claim.Mode (E3)
// DispatchRoute gains:  Finish func(context.Context, pgx.Tx, SecretClaim, Outcome) error
//   runs inside completeOperation's tx BEFORE Service.Complete; error ⇒ rollback + errCompletionUncertain.
```
E1 (recorded deviation, integrator confirms): the contract text types Finish's third argument `Operation`;
`Operation` carries no lease token, and `finish_cvs_create` is lease-fenced, so Finish takes `SecretClaim`
(operation id + generation + lease token + mode).

## Defaults adopted (P2s the frozen text left open; integrator may override)
- E2 Trade no: `"LC" + base32.StdEncoding(NoPadding)(sha256([]byte(lowercaseUUIDText)))[:18]` — the Go twin of
  0073's `'LC'||…` (cvs-core C2 uses the identical input `convert_to(p_operation::text,'UTF8')`).
- E3 `load_cvs_create(..., mode)` gets `SecretClaim.Mode`; SQL also requires it to equal `operations.lease_mode`.
- E4 Loader SQLSTATEs: `PT409` (policy refusal: not REQUESTED, profile disabled, credential moved, not payable)
  → `ErrPolicyDenied`; `40001`/`22023`/`P0002` → plain error (dispatcher: uncertain / secret_load_failed).
  LIVE without `CVS_ECPAY_LIVE_CREATE=1` and a recipient failing `RecipientOK` → `ErrPolicyDenied`.
- E5 Keyring env `ECPAY_LOGISTICS_KEYRING` = JSON `{"active":"<id>","keys":[{"id":"…","key_base64":"…"}]}`
  (≤8 KiB, 1..16 keys, 32-byte keys), AES-256-GCM, AAD = canonical JSON of `{purpose:"ecpay-logistics-v1",
  tenant_id, store_id, connection_id, provider:"ecpay_logistics", environment, merchant_id, version}`
  (metareply keyring shape; errors never echo key material or env values).
- E6 `Device` in the map form: `"0"` (desktop map) unless the caller passes `mobile=true`; cvs-core passes
  `false` in v1 (TCV08 records WebKit mobile behaviour; switching is a one-line change).
- E7 Create `0|msg` → FAILED_FINAL `ecpay.rejected`; HTTP 403 → UNKNOWN `ecpay.rate_limited`; bad MAC /
  malformed / timeout / 5xx → UNKNOWN `ecpay.uncertain`. Codes match `^[a-z0-9_.]{1,80}$`.
- E8 Directory cache clock: next 20:00 `Asia/Taipei` via `time.LoadLocation` with a fixed UTC+8 fallback
  (no tzdata dependency); single-flight = `sync.Mutex` + per-key `chan struct{}`.

## FROZEN Go interface (cvs-core, cvs-tests and the integrator call exactly these)
```go
package ecpay // internal/integrations/shipping/ecpay — stdlib only, no SQL, no River
type Environment string // "SANDBOX" | "LIVE"
type Credentials struct{ MerchantID, HashKey, HashIV string }                  // String() redacted
type Payload struct{ HashKey, HashIV, SenderName, SenderCellPhone string }       // ciphertext JSON {hash_key,hash_iv,sender_name,sender_cell_phone}; redacted
type Scope struct{ TenantID, StoreID, ConnectionID, MerchantID string; Environment Environment; Version int64 }
type Store struct{ ID, Name, Address, Phone string }
type MapReturn struct{ MerchantID, MerchantTradeNo, SubType, StoreID, Outside string }
type CreateRequest struct{ SubType, MerchantTradeNo, MerchantTradeDate, ReceiverStoreID, ReceiverName, ReceiverPhone, GoodsName, ServerReplyURL string
    GoodsAmount, CollectionAmount int } // CollectionAmount 0 ⇒ IsCollection=N
type Result struct{ Outcome, Code, LogisticsID, PaymentNo, ValidationNo, ShipmentNo, StatusCode string } // Outcome SUCCEEDED|FAILED_FINAL|UNKNOWN
type StatusReport struct{ MerchantID, MerchantTradeNo, LogisticsID, RtnCode, RtnMsg, UpdateDate, PaymentNo, ValidationNo string; BodySHA256 [32]byte }
var ErrInvalid, ErrMAC, ErrNoDirectory, ErrUnavailable error
func CheckMac(params url.Values, hashKey, hashIV string) string                  // F6
func VerifyMac(params url.Values, hashKey, hashIV string) bool                   // constant-time, over every field except CheckMacValue
func RecipientOK(name, phone string) (normalizedPhone string, ok bool)           // twin of fulfillment.ecpay_recipient_ok
func MerchantTradeNo(operationID string) string                                  // E2
func CVSType(subType string) (string, error)                                     // UNIMART*→UNIMART, FAMI*→FAMI, HILIFE*→HILIFE, OKMARTC2C→ErrNoDirectory
func MapForm(env Environment, merchantID, tradeNo, subType, serverReplyURL string, mobile bool) (action string, fields map[string]string)
func ParseMapReturn(body []byte) (MapReturn, error)                              // ≤8 KiB, F3 subset, dup keys → ErrInvalid
func ParseStatus(body []byte, c Credentials) (StatusReport, error)               // ≤16 KiB, dup keys → ErrInvalid, MAC → ErrMAC, MerchantID must equal c
type Client struct{ /* unexported */ }
func NewClient(env Environment, rt http.RoundTripper) (*Client, error)           // rt nil = TLS≥1.2 default; tests pass a fake's transport
func (c *Client) Probe(ctx context.Context, cr Credentials) error                // GetStoreList UNIMART, 10 s; RtnCode≠1 → ErrUnavailable
func (c *Client) StoreDirectory(ctx context.Context, cvsType string, cr Credentials) (map[string]Store, error) // §7.2 cache; cached failure → ErrUnavailable
func (c *Client) CachedStore(cvsType, storeID string) (Store, bool, bool)        // (store, hit, cacheFresh) — inline map-return path, never fetches
func (c *Client) Create(ctx context.Context, cr Credentials, p Payload, r CreateRequest) Result
func (c *Client) Query(ctx context.Context, cr Credentials, merchantTradeNo string) Result  // Query/V5
func PrintForm(env Environment, cr Credentials, subType, logisticsID, paymentNo, validationNo string, thermal bool) (action string, fields map[string]string, err error) // OKMARTC2C → ErrInvalid
type Keyring struct{ /* unexported */ }
func LoadKeyring(getenv func(string) string) (*Keyring, error)                   // E5
func (k *Keyring) Seal(s Scope, p Payload) (keyID string, nonce, ciphertext []byte, err error)
func (k *Keyring) Open(s Scope, keyID string, nonce, ciphertext []byte) (Payload, error)
type Config struct{ Enabled, LiveCreate bool; HooksOrigin string }               // CVS_ECPAY_ENABLED, CVS_ECPAY_LIVE_CREATE, COMMERCE_CVS_HOOKS_ORIGIN
func LoadConfig(getenv func(string) string) (Config, error)                      // Enabled ∧ HooksOrigin not https origin → error

package ecpayroute // internal/integrations/shipping/ecpay/ecpayroute — the only SQL in this unit
func Routes(workerPool *pgxpool.Pool, keys *ecpay.Keyring, client *ecpay.Client, cfg ecpay.Config) ([]core.DispatchRoute, error)
// one route: provider ecpay_logistics, action ecpay.cvs_create, purpose transactional;
// Check / LoadSecret(load_cvs_create) / DispatchWithSecret(Create) / ReconcileWithSecret(Query) / Finish(finish_cvs_create)
```

## Build
1. **Wire** (`ecpay/*.go`): §7.1–§7.3 exactly; endpoint constants per environment with docs URL + retrieval date
   (PROCESS §5); .NET encode table; `GoodsName` fixed `"商品"`; F5 field validation before send; response cap
   256 KiB (GetStoreList 32 MiB / 30 s, streamed decode into the map); no redirects; no body/key/PII/trade-no logs.
   `doc.go`: owns / never / hosts (`logistics(-stage).ecpay.com.tw`, why). Merge alone first = **F1e** (cvs-core
   compiles against it). Unit tests `*_test.go` names must not start with `TestEcpayMac`/`TestCvs` (TCV gates are
   cvs-tests').
2. **Keyring** (`ecpay/keyring.go`): E5; `Open` with the wrong scope fails.
3. **Route** (`ecpayroute`, after F0 + cvs-core F1 SQL merged): Check = request shape `{order_id,attempt}` ∧
   `cfg.Enabled` (no tx, no Secret); LoadSecret = `load_cvs_create` (E3/E4) → decrypt with the **frozen** version;
   dispatch refuses LIVE without `LiveCreate` and recipients failing `RecipientOK`; DispatchWithSecret = Create
   with frozen trade no/date, `ServerReplyURL = HooksOrigin + "/v1/cvs/ecpay/status/" + endpoint_id`, IsCollection
   per `collection_amount`; ReconcileWithSecret = Query/V5 by frozen trade no; Finish = `finish_cvs_create` with
   `Outcome.Detail` codes (total over provider data: never returns an error for a provider value). Secret zeroed
   after use. Every UNKNOWN branch comments why it must not re-create (I06).

## Write paths
`internal/integrations/shipping/ecpay/**` (incl. `ecpayroute/**`, own `*_test.go`, `doc.go`), `output/cvs-ecpay/**`.
Forbidden: `internal/integrations/core/**`, `ecpay/ecpaytest/**` (cvs-tests' fake), migrations, `cmd/**`,
`internal/{fulfillment,checkout,httpapi,buyerhttp}/**`, `tests/**`, contracts, go.mod/go.sum, `apps/**`.

## PROCESS §5 (every file you touch)
Package comments (owns / never / external host + why); wire constants carry docs URL + 2026-09-29/30 date;
every retry/UNKNOWN branch says why same trade no / why no retry; SQL call sites name the definer and why
(`// integration.load_cvs_create: lease-fenced, frozen credential version (§4.3)`). No hand-written dependency
lists; run `scripts/dev/depmap.sh` and include the diff.

## Verify
```sh
GOTOOLCHAIN=go1.27.1 go vet ./internal/integrations/... && gofmt -l internal
GOTOOLCHAIN=go1.27.1 go test -race -count=1 ./internal/integrations/shipping/... ./internal/integrations/core/...
LC_FOCUSED_TIMEOUT=1800s bash scripts/dev/test-focused.sh '^Test(T06|ExternalOperation|MetaClaimsIntakeMCI)'  # after F0/F1; regression
bash scripts/dev/depmap.sh && python3 scripts/check_packet.py
```
Logs → `/Volumes/data/live_commerce_architecture_v1/output/cvs-ecpay/` (command, SHA, exit, PASS/FAIL/SKIP).

## Gates
Implementer: unit tests (MOCK via `httptest`), incl. one red run per non-trivial function. Independent (do not
claim): TCV01, TCV05 (route legs), TCV06 (parse legs), TCV07, TCV10 — cvs-tests. TCV12/TCV13 owner LIVE.

## Integrator hooks (integrator only; unit ships `output/cvs-ecpay/integrator-hooks.patch`)
- F0 above (core).
- `cmd/claims-worker/main.go`: `ecpay.LoadKeyring`, `ecpay.LoadConfig`, `ecpay.NewClient(env, nil)`, append
  `ecpayroute.Routes(workerPool, keys, client, cfg)` to the route list iff `cfg.Enabled`.
- `deploy/compose.yml` (api + claims-worker env), `deploy/env/api.env.example`, `deploy/secrets.manifest.tsv`
  (`ECPAY_LOGISTICS_KEYRING` file row), §12; `docs/engineering/dependency-map.md` regen.

## NOT_RUN / Return
NOT_RUN: every SANDBOX/LIVE leg (no keys), status notifications in SANDBOX (F7). Return commit SHAs (F1e, full),
model/reasoning, base, paths, commands + exits + counts, evidence paths, E1–E8 as applied, risks, NOT_RUN. Any
deviation from the frozen signatures = stop and escalate.
