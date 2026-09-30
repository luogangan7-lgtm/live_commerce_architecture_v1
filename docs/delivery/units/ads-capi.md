# Unit ads-capi — 0080 CAPI + billing/consent grants, `internal/attribution` (CAPI sweeper, CAPI route, feed, consent hook)

Role: integration_worker (mid tier). Base = integrator-recorded SHA at dispatch (≥ `00c1d94` + the
merges below). Worktree `.worktrees/ads-capi`, branch `unit/ads-capi`. No delegation, no network.
**Wave 2:** dispatch only after customers-billing's `0078_customers_privacy.sql` + `0079_platform_billing.sql`
(with `customers.consent_allows(uuid,uuid,uuid,text,text)` and `billing.store_standing(uuid,uuid)` as
SECURITY DEFINER) and ads-core F1c (0074/0075, role `commerce_ads_writer`) are merged. May author against
the frozen signatures earlier; PG runs need those merges. Contract: `contracts/meta-ads-v1.md` (FROZEN);
ads-core D1–D14 and ads-graph G1–G3 bind. `contracts/customers-billing-v1.md` CD5/CD7, BD5 (by section).

**Goal:** one CAPI Purchase per consented CAPTURED attempt, never resent after UNKNOWN, user data read
only through the lease-fenced definer and hashed in memory; a public product feed per verified storefront
host; and the grants that let ads definers read billing standing (0080 unblocks ads-core's approve/publish paths).

## Read (by section)
PROCESS.md; contract §0 AD8–AD10, §0.1 A-3/A-7/A-11, §1 F14–F17, §3 (CAPI bullet), §4.3 (all), §4.4
(0080 rows), §6.1 (CAPI Check), §6.4, §7 (feed + consent rows), §8, §9 MA03/MA08, §12. By symbol:
`0018_payment_capture.sql` (`payments.facts` :76), `0062` (`payments.refund_facts`), `0020`
(`buyer.resolve_published_store` :85), `0012:103` (`storefront.destination_snapshots`), `buyer.owners`,
0078/0079 function headers only; Go: `internal/buyerhttp/handler.go` (:377 `X-Commerce-Storefront-Origin`),
`internal/integrations/meta_ads` frozen API (`Client.PostEvent`, `Config`, `tokenopen`), `internal/ads`
frozen API, `core.InsertOperationJobOn`, `core.DenyPolicy`.

## Defaults adopted
- C1 **Package split for MA11:** `internal/attribution` (sweeper, feed, consent hook — importable by
  `cmd/api`) and `internal/attribution/capiroute` (dispatch route; imports `tokenopen`; `cmd/ads-worker` only).
- C2 **CAPI LoadSecret** runs `integration.load_meta_ads_token` and `ads.capi_user_data` in the **same**
  lease-fenced transaction, opens the token, hashes `ph` (F16 E.164 digits) and `external_id` in memory and
  packs `{token, hashed user data, ua, contents, value, currency, source_url}` into the `core.Secret`; raw
  phone/owner id zeroed before return (`DispatchWithSecret` never receives the claim, so this is the only
  lease-fenced point to read user data). Reconcile = plain `Reconcile` returning UNKNOWN unchanged.
- C3 `external_id` = hex(SHA-256(hex(HMAC-SHA256(K, "capi-external-id/v1|<tenant>|<store>|<owner>"))));
  K from `COMMERCE_CAPI_EXTERNAL_ID_KEY_FILE` (worker only, O-D).
- C4 Frozen op request (provider `meta_dataset`, action `meta.capi.purchase`, queue `ads` p=3, semantic key
  `ads:capi:<attempt>`): `{"v":1,"attempt_id","event_id":"lc-purchase-<attempt>","event_time":<unix>,"test_event_code"?}`;
  pixel id = `ExternalAssetID`. Body `event_source_url` = storefront origin + `/orders`, `action_source=website`,
  `partner_agent` = `COMMERCE_META_ADS_PARTNER_AGENT`, `value` exact decimal of amount_minor/100.
- C5 **Consent hook (A-3)** matches customers-core's frozen seam in `consentPut`: same transaction, iff
  `Purpose==ads_personalization && Granted`. Go pre-validates so the SQL call cannot fail on input: UA
  truncated to 512 runes, empty UA → no call. The definer upserts only when `consent_allows` is true.
- C6 **Feed** Go route `GET /v1/buyer/feeds/meta.csv` (buyer runtime pool, origin from the BFF-set
  `X-Commerce-Storefront-Origin` only, never a query/store param); columns per F17 re-verified at
  implementation (`id`=variant public id, title, description, availability, condition, price "<amount> <ISO>",
  link, image_link, brand); `Cache-Control: public, max-age=900`; storefront Next route proxies it.
- C7 CAPI Check codes: `consent_withdrawn`, `capi_disabled`, `dataset_binding_changed`,
  `capi_principal_revoked`, `environment_mismatch`, `event_too_old` (via `core.DenyPolicy`); nil in reconcile mode.

## FROZEN Go interface
```go
package attribution // internal/attribution
func AddWorkers(w *river.Workers, pool *pgxpool.Pool) error          // capi_purchase_sweep_v1 (also deletes stale capi_contexts)
func PeriodicJobs() []*river.PeriodicJob                              // 5 min, queue "ads"
func PutCAPIContext(ctx context.Context, tx pgx.Tx, sessionHash []byte, storeID, userAgent string) error // C5; ads.put_capi_context, caller's tx
func FeedHandler(pool *pgxpool.Pool) http.Handler                    // C6; ads.feed_rows(origin)
func EventID(attemptID string) string                                // "lc-purchase-"+uuid (AD8)
func HashPhone(e164 string) (string, bool)                           // F16: digits incl. country code → hex SHA-256; false = omit
func ExternalID(key []byte, tenantID, storeID, ownerID string) string // C3

package capiroute // internal/attribution/capiroute — cmd/ads-worker only
func Routes(pool *pgxpool.Pool, cfg metaads.Config, keys *tokenopen.Keyring, externalIDKey []byte) ([]core.DispatchRoute, error) // 1 route (meta_dataset, meta.capi.purchase)
```
SQL (0080, exactly §4.3/§4.4): grants; `ads.capi_contexts`, `ads.capi_events`; `ads.capi_user_data(uuid,bigint,bytea)`;
`ads.put_capi_context(bytea,uuid,text)`; `ads.feed_rows(text)`; `ads.plan_capi_*` (names yours, EXECUTE `commerce_worker`).

## Build
1. `migrations/0080_meta_capi.sql` (§4.3 + §4.4 0080 rows; column-level SELECT + read policies only on the
   listed columns; `COMMENT ON` all). MA02's RESTRICTED-as-`commerce_ads_writer` check depends on it.
2. `internal/attribution`: sweeper §6.4 (≤200/run, consent checked in the planning transaction, environment +
   `PROVIDER_MOCK` filter, `received_at ≥ now−6 d`), feed, hook. `capiroute`: C2/C4/C7, `Client.PostEvent`.
3. `apps/storefront/app/feeds/meta.csv/route.ts` (new file): server route, forwards only the verified host
   via the existing buyer-server helper; streams CSV; no query params accepted.

## PROCESS §5 (binding)
Package comments (`owns` / `never` — never resends, never stores hashes, never reads user data outside the
definer; host `graph.facebook.com`); consent call sites `// customers.consent_allows: CD5 …`; money
`// I05:`; the UNKNOWN branch says why never resent (F15); F14/F16/F17 constants with URL + date; SQL
`COMMENT ON`; storefront route header comment (BFF → Go). Run `scripts/dev/depmap.sh`.

## Write paths
`migrations/0080_meta_capi.sql`, `internal/attribution/**`, `apps/storefront/app/feeds/meta.csv/route.ts`,
`docs/engineering/dependency-map.md` (generated), `output/ads-capi/**`.
Forbidden: `internal/customers/**`, `internal/billing/**`, 0078/0079, `internal/ads/**`, `internal/integrations/**`,
`cmd/**`, `internal/buyerhttp/**`, contracts, other `apps/**`, `tests/**`, go.mod.

## Gates
Implementer unit tests (not `TestMetaAdsMA*`): hashing vectors (F16), event body exact keys, C3 vector,
feed CSV escaping. Independent (ads-tests phase B): **MA03, MA08**, MA02 billing clauses, MA09 consent part.

## Verify
```sh
GOTOOLCHAIN=go1.27.1 go vet ./... && gofmt -l internal
GOTOOLCHAIN=go1.27.1 go test -race -count=1 ./internal/attribution/...
LC_FOCUSED_TIMEOUT=1800s bash scripts/dev/test-focused.sh '^Test(T06|Customers|Billing)'   # regression of the merged 0078/0079 suites
pnpm typecheck:storefront && bash scripts/dev/depmap.sh && python3 scripts/check_packet.py
```
Logs → `/Volumes/data/live_commerce_architecture_v1/output/ads-capi/`.

## NOT_RUN
MA-S4 (owner test dataset in 香港大碗, test_event_code), MA-L*; U5/U6.

## Integrator hooks (integrator only)
`cmd/ads-worker/main.go`: append `capiroute.Routes(…)` to the dispatcher routes, `attribution.AddWorkers` +
`attribution.PeriodicJobs()`, env `COMMERCE_CAPI_EXTERNAL_ID_KEY_FILE`; `internal/buyerhttp` (or `cmd/api`)
mount `GET /v1/buyer/feeds/meta.csv` → `attribution.FeedHandler`; customers-core `consentPut`: replace the A-3 seam comment with
`attribution.PutCAPIContext(ctx, tx, hash, storeID, r.UserAgent())` (same tx, grant only);
Caddy: storefront hosts route `/feeds/meta.csv` to the storefront app; retention registration of
`ads.insights_daily`/`ads.capi_events`/`ads.capi_contexts` with customers-billing CD7 (§12; blocks production mount);
`deploy/secrets.manifest.tsv` row for C3; OpenAPI buyer feed path.

## Return
SHA, model/reasoning, base, paths, commands + exits + counts, evidence, C1–C7 handling, risks, NOT_RUN.
