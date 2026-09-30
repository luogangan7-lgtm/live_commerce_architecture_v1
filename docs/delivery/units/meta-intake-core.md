# Unit meta-intake-core — 0064 SQL, IngestMetaIntake, consumer staging

Role: commerce_worker. Base `8f491dc`. Worktree `.worktrees/meta-intake-core`, branch
`unit/meta-intake-core`. No delegation, network or Meta token. Runs **in parallel** with
`meta-intake-reply` phase A (disjoint paths; its phase B builds on this merge). Merge first.

**Goal:** a signed comment on a bound object stages one text-free `claims.meta_intake` row in the
consumer tx; `IngestMetaIntake` applies it; all reply-path SQL exists and is privilege-exact.

## Read (by section; grep headings, sed ranges)
`docs/delivery/PROCESS.md`; `contracts/meta-claims-intake-v1.md` in full (FROZEN; §4.3, §4.4, §15
binding); `live-keyword-claims-v1.md` §3.2, §3.3, §4.3 + its "Amendment by meta-claims-intake-v1".
Code by symbol: `internal/claims/ingest.go` (`IngestParsed`, `readSource`, `record`,
`lockBundle`), `claims.go` (`Reason`, `persistedReasons`, `mapError`), `credentials.go`
(`LinkToken`, `labelMAC`); `internal/integrations/meta/consumer.go` (`Work`), `projection.go`,
`protocol.go` (`canonical`, `tupleHash`), `runtime.go`; `internal/platform/platform.go`
(`ValidateMetaConsumerPool`, `validatePoolAuthority`); migrations 0028, 0029
(`finish_social_event`), 0060, 0061 (`load_stripe_credential`, `register_stripe_account`), post-River 0003/0012.

## FROZEN Go interface (meta-intake-reply and meta-intake-tests call exactly these)
```go
package claims
const ReasonRateLimited Reason = "RATE_LIMITED" // persisted, meta only; add to persistedReasons
// IngestResult gains: BundleCreated bool `json:"bundle_created"` (true only if this call inserted the bundle)
func IngestMetaIntake(ctx context.Context, tx pgx.Tx, intakeID string) (IngestResult, error)
// ReplyLinkKey/SystemLinkToken live in internal/claims/system_link.go, owned by meta-intake-reply.
package meta
type ClaimsActorKey struct{ /* unexported */ } // String/GoString/Format/MarshalJSON = "[redacted]"
func NewClaimsActorKey(raw []byte) (ClaimsActorKey, error) // 32 bytes, not all-zero
func LoadClaimsActorKey(getenv func(string) string) (ClaimsActorKey, bool, error)
//  COMMERCE_CLAIMS_ACTOR_KEY std base64; "" → (zero,false,nil) = staging off; malformed → ErrRuntimeConfig
func ClaimActorKey(k ClaimsActorKey, object, assetID, fromID string) string
//  hex(HMAC-SHA256(k, json.Marshal([]string{"meta-claim-actor/v1",object,asset,from}))); zero key → ""
func NewConsumerWorkerWithClaims(ctx context.Context, pool *pgxpool.Pool, keys *PayloadKeyring, actor ClaimsActorKey) (*ConsumerWorker, error)
func NewConsumerClientWithClaims(ctx context.Context, workerPool, consumerPool *pgxpool.Pool,
    keys *PayloadKeyring, actor ClaimsActorKey, concurrency int) (*river.Client[pgx.Tx], error)
// unexported, frozen for the in-package MCI01 test (§3 table + fail-closed list, text >256 B, parent_id):
//  func qualifyClaim(object, assetID, kind string, unit []byte) (claimCandidate, bool)
//  type claimCandidate struct{ ObjectID, CommentRef, FromID string; Parsed grammar.Result }
// NewConsumerWorker/NewConsumerClient unchanged == WithClaims(zero key): never stage (MC01–07 bytes).
package platform
func OpenClaimsIntakePool(ctx context.Context, dsn string) (*pgxpool.Pool, error)
func ValidateClaimsIntakePool(ctx context.Context, pool *pgxpool.Pool) error // as ValidateMetaConsumerPool
```
`IngestMetaIntake` preconditions (else `command.ErrInvalid`, nothing written): READ COMMITTED tx of
the `commerce_claims_intake` login; `intakeID` leased in this tx by `claims.lease_meta_intake()`;
`app.tenant_id/app.store_id` set local to its scope; `app.principal_id/app.buyer_id` unset. It
**never changes intake state** (the poller does). No interval / source inactive / future >120 s →
`{Outcome:REJECTED, Reason:WINDOW_CLOSED, EventID:""}`, nil. DB errors stay unwrappable
(`errors.As(err, **pgconn.PgError)`; the poller classifies SQLSTATE per §5.3). Manual path
(`IngestParsed`, `RecordManualClaim`) keeps bytes and behaviour; both share one unexported core.

## Frozen SQL surface the reply unit calls
`claims.lease_meta_intake() RETURNS SETOF claims.meta_intake`; `claims.fail_meta_intake(p_intake
uuid, p_code text, p_final boolean) RETURNS void` (no lease; PENDING only);
`integration.claim_reply_plannable(p_intake uuid) RETURNS text`; `integration.plan_claim_reply(
p_intake uuid, p_operation uuid, p_link_hash bytea, p_link_key_id text, p_job bigint) RETURNS uuid`;
`claims.check_meta_reply(p_operation uuid, p_link_hash bytea) RETURNS text`;
`integration.load_meta_page_token(p_operation uuid, p_generation bigint, p_lease_token bytea)
RETURNS TABLE(tenant_id uuid, store_id uuid, binding_id uuid, provider text, asset_id text, version
bigint, key_id text, nonce bytea, ciphertext bytea, scopes_attested text[])`;
`integration.register_meta_page_token(p_tenant uuid, p_store uuid, p_principal uuid, p_binding
uuid, p_provider text, p_asset_id text, p_expected_version bigint, p_key_id text, p_nonce bytea,
p_ciphertext bytea, p_scopes text[]) RETURNS bigint` (new version = expected+1, expected 0 creates
the head; provider/asset ≠ binding → 22023). Poller's final write: plain `UPDATE
claims.meta_intake SET state, drop_reason ('window_closed' only), applied_event_id,
lease_xid=NULL, updated_at` under the LEASED policy. Page-token AAD (§7) =
`json.Marshal([]string{"livecommerce/meta-page-token/v1",tenant,store,binding,provider,asset_id,version,key_id})`.

**Contract gaps — implement these defaults unless the integrator rules otherwise:**
(a) `live_media`: the intake holds no Meta kind → column `claims.meta_intake.live_media boolean NOT
NULL`, from a trailing `p_live_media boolean` of `insert_meta_intake` (`instagram_live_comment`).
(b) `origin`: `commerce_worker` cannot read `control.storefront_domains` → the frozen request gains
`origin` (ACTIVE domain https origin) next to `origin_ref`, read by `plan_claim_reply`.
(c) `p_occurred`: IG units carry no time (U7) → `stage_claim_intake` uses the locked inbox event's
`occurred_at`; `p_occurred` must be NULL or equal (else 22023); NULL event time → not staged.
(d) gate allowlist edits below exceed §4.4 clause 10 (clause 4 implies them) → allowed as listed.

## SQL work (`0064_meta_claims_intake.sql`, `post_river/0014_meta_claims_intake_river.sql`)
Contract §2, §4 (tables, CHECK widening, interval trigger + backfill, FORCE RLS, §4.1 roles, §4.2
bounds, **§4.3 exactly, no extra grant**), §5.1 stage functions, §6.1 partial unique index, §6.2,
§6.3 `check_meta_reply`, §7 custody + loader + registrar, `identity.principal_holds`,
`live.put_claim_source`, INSERT-only `claims.issue_system_link`, `intake_scope()`. 0014: §5.4
guard + DEFERRABLE constraint trigger on `river.river_job`, River grants of §4.3. Definers:
`search_path=pg_catalog`, `REVOKE ALL FROM PUBLIC`, `COMMENT ON` owner package + only caller;
IR-16 comment on each cross-domain column grant. Round-3 P2s (column lists, `principal_holds`,
`origin_ref`, `issue_system_link` return type, "generation 0" wording): fix, list in return.

## Go work
- `internal/claims`: unexported core parameterised by source (meta: `FOR SHARE` window then
  interval + 60 s grace, §4.2 advisories right after `claim-source`, RATE_LIMITED before the bundle
  step, `BundleCreated`). Header gains Used by `internal/claimsintake`. No new import (claims §4).
- `internal/integrations/meta/claim_intake.go`: §3 qualification + fail-closed list on the already
  projected unit, `grammar.Parse` in memory, `ClaimActorKey`, one `meta_inbox.stage_claim_intake`
  after `finish_social_event`, before COMMIT; stage error rolls back the whole tx. No network/River.
- `cmd/meta-worker`: `LoadClaimsActorKey` + `WithClaims`. `internal/platform`: intake pool
  open/validate; `validatePoolAuthority` rejects runtime logins that can reach the intake role.

## Gate-test edits authorized (only these; ruling (d))
KC03 `foreign-roles-denied`: drop `commerce_integration_writer` × SELECT × `claims.events` only.
KC03 `privilege-matrix` want-sets, `denied` exclusions, schema-`claims` ACL, writer EXECUTE set:
add exactly the §4.3 rows. `TestT06WorkerAuthorityAndFunctionACL`: add each new `integration.*`
function with owner/worker flags and the new count. A MIso/MC ACL enumeration that fails: add the
exact §4.3 row. List every edited line in the return; never loosen an assertion.

## Write paths
`migrations/0064_meta_claims_intake.sql`, `migrations/post_river/0014_meta_claims_intake_river.sql`,
`internal/claims/{ingest.go,claims.go,meta_intake.go,meta_intake_test.go}`, `internal/
integrations/meta/{claim_intake.go,claim_intake_test.go,consumer.go,runtime.go}`,
`cmd/meta-worker/{main.go,main_test.go}`, `internal/platform/{claims_intake.go,platform.go}`,
`tests/foundation/{live_claims_schema_test.go,external_operation_authority_test.go}` (edits above
only), `output/meta-intake-core/**`. Not: contracts, `internal/integrations/core/**`,
`internal/claimsintake/**`, `internal/integrations/metareply/**`, `cmd/claims-worker`, `apps/**`, go.mod.

## Verify
```sh
GOTOOLCHAIN=go1.27.1 go test -race -count=1 ./internal/claims/... ./internal/integrations/meta ./internal/platform ./cmd/meta-worker
GOTOOLCHAIN=go1.27.1 go vet ./... && gofmt -l internal cmd && python3 scripts/check_packet.py
bash scripts/dev/test-focused.sh '^Test(LiveClaims|MetaConsumer|MetaInbox|MetaIsolation|MetaRuntime|T06)'
```
Logs → main checkout `output/meta-intake-core/`. MCI gates belong to `meta-intake-tests`.

## Non-goals / Return
No dispatcher/Graph/poller/claims-worker code, no merchant HTTP/UI for `put_claim_source` (T12), no
LIVE, no contract edits. Return commit SHA, model/reasoning, base, paths, commands + exit codes +
PASS/FAIL/SKIP counts, evidence, round-3 P2 handling, edited gate lines, risks, NOT_RUN. Any
deviation from a frozen signature = stop and escalate.
