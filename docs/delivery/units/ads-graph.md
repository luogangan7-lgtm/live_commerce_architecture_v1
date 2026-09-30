# Unit ads-graph — `internal/integrations/meta_ads` Graph adapter, HPKE token custody, OAuth connect, `cmd/ads-worker`

Role: integration_worker (mid tier). Base SHA `00c1d94`. Worktree `.worktrees/ads-graph`, branch
`unit/ads-graph`. No delegation. **No network** (fake Graph via `httptest` only; SANDBOX gates MA-S1..S4
are owner-scheduled, not this unit's). **Wave 1**, parallel with ads-core/ads-ui against the FROZEN
blocks of `ads-a10.md` and `ads-core.md` (rebase on their F1/F0 merges; do not copy their branches).
Contract: `contracts/meta-ads-v1.md` (FROZEN). ads-core's Defaults D1–D14 bind here too.

**Goal:** every Meta wire call the contract names, classified exactly per §3, with the BISU token
reaching code only through the lease-fenced loader (AD11) and only `cmd/ads-worker` able to open it (A-4).

## Read (by section)
PROCESS.md; contract §0 AD1–AD4, AD9, AD11, §0.1 A-4/A-9/A-10, §1 (F3, F8–F13, F18–F22, U1–U10),
§2 steps 1–3, §3 (all), §3.1, §4.1 (credential widening, HPKE `info`, loader), §6 header + §6.1, §8,
§9 MA01/MA04/MA05/MA11, §11. Code by symbol: `internal/integrations/metareply/{routes.go (Config,
GraphHost, versionPattern, loopbackPattern, Routes, the LoadSecret/DispatchWithSecret wiring),
keyring*.go}` (copy shape, not code paths that touch page tokens), `internal/integrations/core`
(`DispatchRoute`, `Secret`, `SecretClaim`, `Outcome`, ads-a10 additions), `cmd/claims-worker/main.go`
(config redaction, pools, `core.NewDispatcher`, River client), `cmd/api/accounts.go` (insert-only River
client on `river`), `internal/platform` (`OpenWorkerPool`, `ValidateWorkerPool`). `go doc crypto/hpke`.

## Defaults adopted (in addition to ads-core D1–D14)
- G1 Package `metaads` in directory `internal/integrations/meta_ads` (contract path); HPKE **open** lives
  only in subpackage `internal/integrations/meta_ads/tokenopen`, imported only by `cmd/ads-worker`
  (MA11 static check: `go list -deps ./cmd/api` must not contain it). Seal (public key) in `metaads`.
- G2 HPKE suite X25519-HKDF-SHA256 / HKDF-SHA256 / AES-256-GCM (`crypto/hpke`, stdlib, no dependency);
  `enc` 32 bytes stored in `nonce`/`pending_enc`; `info` = JSON array per §4.1.
- G3 Env (names frozen for deploy): api — `COMMERCE_META_ADS_APP_ID`, `COMMERCE_META_ADS_APP_SECRET_FILE`,
  `COMMERCE_META_ADS_CONFIG_ID`, `COMMERCE_META_ADS_REDIRECT_URI`, `COMMERCE_META_ADS_GRAPH_VERSION`
  (required, `v26.0`, A-9), `COMMERCE_META_ADS_TOKEN_HPKE_PUBLIC_KEYS_JSON` + `…_ACTIVE_KEY_ID`;
  worker — `COMMERCE_ADS_WORKER_DATABASE_URL` (commerce_worker login), `COMMERCE_META_ADS_GRAPH_VERSION`,
  `COMMERCE_META_ADS_TOKEN_HPKE_PRIVATE_KEYS_FILE`, `COMMERCE_META_ADS_PARTNER_AGENT`. Secrets only from
  files (O-D); config types redact in every formatter. Any missing/invalid value → process refuses to start
  with one fixed code; the api constructor returns `nil, nil` when `COMMERCE_META_ADS_APP_ID` is unset (surface off).
- G4 `optimization_goal`/`billing_event` pairs (U2, closed by MA-S1): ENGAGEMENT → `POST_ENGAGEMENT`/
  `IMPRESSIONS`; TRAFFIC → `LINK_CLICKS`/`IMPRESSIONS`; `special_ad_categories=[]` (U1, MA-S1). Each a named
  constant with docs URL + retrieval date + `// UNKNOWN until MA-S1`.
- G5 Reconcile for `preflight_account`/`read_insights` = repeat the read (§3); for creates, name tag match
  over ≤10 pages of `limit=100`; `>1` → UNKNOWN `duplicate_remote_objects`.
- G6 OAuth `Connect` performs, in order: code exchange (with `redirect_uri`, U10), `/me?fields=client_business_id`,
  `/me/permissions` (granted only), `/me/adaccounts?fields=account_id,name,currency,timezone_name,account_status`
  (≤5 pages), `/act_{id}/adspixels?fields=id,name` per account (≤20 accounts), then seals and zeroes the
  plaintext. Any failure → `ads.ErrConnectFailed` (no partial result, no token returned).

## FROZEN Go interface
```go
package metaads // internal/integrations/meta_ads
const GraphHost = "https://graph.facebook.com"
type Config struct{ GraphBaseURL, GraphVersion, PartnerAgent string; HTTPClient *http.Client } // host = GraphHost or loopback (MOCK)
var ErrNotWholeUnit, ErrUnsupportedCurrency, ErrBadSpend error
func MetaBudget(currency string, amountMinor int64) (int64, error)       // §3 I05; TWD /100 only if %100==0
func SpendMinor(currency, spend string) (int64, error)                   // §3 exact decimal ×100
type Preflight struct{ Status int; Currency, Timezone string; Funded bool }
type InsightsDay struct{ EffectiveStatus, Currency, Timezone string; SpendMinor, Impressions, Clicks int64; Purchases, PurchaseValueMinor *int64 }
func EncodePreflight(p Preflight) (string, error)                        // grammar §3, ≤200 (ads-core D2)
func ParsePreflight(ref string) (Preflight, error)
func EncodeInsights(d InsightsDay) (string, error)
func ParseInsights(ref string) (InsightsDay, error)
type SealKeys struct{ /* public keys + active id */ }
func LoadSealKeys(getenv func(string) string) (*SealKeys, error)
type AppConfig struct{ AppID, RedirectURI string; AppSecret []byte }     // redacted formatters
type OAuth struct{ /* … */ }
func NewOAuth(cfg Config, app AppConfig, keys *SealKeys) (*OAuth, error)
func (o *OAuth) Connect(ctx context.Context, code string, seal ads.SealInfo) (ads.ConnectResult, error) // = ads.ConnectFunc
type Client struct{ /* … */ }
func NewClient(cfg Config) (*Client, error)
func (c *Client) PostEvent(ctx context.Context, token []byte, pixelID string, body []byte) (core.Outcome, error) // §3 CAPI classification; ads-capi calls it
func Routes(pool *pgxpool.Pool, cfg Config, keys *tokenopen.Keyring, check func(context.Context, core.DispatchRequest) error) ([]core.DispatchRoute, error)
// 8 routes (meta_ads × create_campaign|create_adset|create_creative|create_ad|preflight_account|activate|pause|read_insights),
// purpose "marketing"; LoadSecret = integration.load_meta_ads_token + tokenopen; DispatchWithSecret; ReconcileWithSecret.

package tokenopen // internal/integrations/meta_ads/tokenopen — cmd/ads-worker only
type Keyring struct{ /* private keys */ }
func LoadKeyring(getenv func(string) string) (*Keyring, error)
func (k *Keyring) Open(tenantID, storeID, keyID string, enc, ciphertext []byte) ([]byte, error)

package main // cmd/api/merchant_ads.go (new file)
func newMerchantAds(pool *pgxpool.Pool, getenv func(string) string) (*ads.Service, error) // nil,nil when surface off (G3)
```
Classification tables are §3 verbatim (creates; activate/pause never FAILED_FINAL; reads; CAPI). Every
Graph error code constant (4, 17, 613, 80004, 100) carries the F# URL + retrieval date.

## Build
1. `metaads`: config/host guard, money, grammar, classification, 8 routes with the frozen request JSON of
   ads-core D11 (strict decode; unknown key → FAILED_FINAL `bad_request` before any call), reconcile by tag,
   status GET, redacted formatters, bounded bodies (≤1 MiB), `CallTimeout` context only.
2. `tokenopen` + `SealKeys` (G1/G2); zero every plaintext slice after use.
3. `OAuth.Connect` (G6) with an `httptest` fake Graph in unit tests.
4. `cmd/ads-worker`: config (G3), `platform.OpenWorkerPool` + validator, `core.NewDispatcher(pool,
   metaads.Routes(…, ads.NewChecker(pool).Check))`, River client Schema `river`, **Queues `{"ads": {MaxWorkers: 4}}` only**,
   `ads.AddWorkers` + `ads.PeriodicJobs()` + the dispatcher worker; graceful stop; `doc.go`.
5. `cmd/api/merchant_ads.go`: insert-only River client (`river`), `metaads.NewOAuth`, `ads.NewService`.

## PROCESS §5 comment/dependency rules (binding)
`// Package metaads owns …` / `// It never …` (plans ops, reads PG outside the loader, stores tokens) /
external host `graph.facebook.com` + why; `tokenopen` package comment states it must only be imported by
`cmd/ads-worker`. Each wire constant: docs URL + retrieval date. Each UNKNOWN branch: why never re-POST
(no idempotency key, F9). `cmd/*` doc.go lists env names (never values). No hand-written dependency lists; run `scripts/dev/depmap.sh`.

## Write paths
`internal/integrations/meta_ads/**`, `cmd/ads-worker/**`, `cmd/api/{merchant_ads.go,merchant_ads_test.go}`,
`docs/engineering/dependency-map.md` (generated), `output/ads-graph/**`.
Forbidden: `cmd/api/main.go`, `internal/ads/**`, `internal/integrations/core/**`, migrations, `tests/**`,
`deploy/**`, `Caddyfile`, contracts, go.mod/go.sum (stdlib only — a needed module = stop and escalate).

## Gates
Implementer (unit tests, names not `TestMetaAdsMA*`): money + grammar vectors, classification tables,
tag reconcile, host guard, redaction, OAuth fake. Independent (ads-tests): MA01, MA04, MA05, MA06, MA11.

## Verify
```sh
GOTOOLCHAIN=go1.27.1 go vet ./... && gofmt -l internal cmd
GOTOOLCHAIN=go1.27.1 go test -race -count=1 ./internal/integrations/meta_ads/... ./cmd/ads-worker ./cmd/api
GOTOOLCHAIN=go1.27.1 go list -deps ./cmd/api | grep -c meta_ads/tokenopen   # must print 0
bash scripts/dev/depmap.sh && python3 scripts/check_packet.py
```
Logs → `/Volumes/data/live_commerce_architecture_v1/output/ads-graph/`.

## NOT_RUN (expected)
MA-S1..S4 (owner sandbox account + app 大梦 roles), MA-L1/L2; U1–U10 stay UNKNOWN; PG-backed route runs until F2.

## Integrator hooks (integrator only)
`cmd/api/main.go`: `ads, err := newMerchantAds(runtimePool, os.Getenv)` → `httpapi.Options{Ads: ads}`;
`deploy/compose.yml`: `ads-worker` service (image, commerce_worker DSN, secret files, no published port) +
api env; `deploy/secrets.manifest.tsv` rows for the G3 files; Caddy access-log exclusion for
`/api/ads/meta/callback` (§2 step 2); later `attribution.Routes` + CAPI periodic job appended in
`cmd/ads-worker` (ads-capi).

## Order / Return
Author at dispatch; compile after F0/F1; PG runs after F2 (ads-core merged). Return SHA, model/reasoning,
base, paths, commands + exits + counts, evidence, G1–G6 handling, UNKNOWNs touched, risks, NOT_RUN.
