# Unit meta-intake-reply — dispatcher secret hook, Graph private reply, intake poller, claims-worker

Role: integration_worker. Base `8f491dc`. Worktree `.worktrees/meta-intake-reply`, branch
`unit/meta-intake-reply`. No delegation; no network except loopback `httptest`; no Meta token.
**Phase A** (parallel with `meta-intake-core`, no dependency on it): dispatcher hook, `system_link.go`,
`metareply`. **Phase B** (after the integrator merges `meta-intake-core`; rebase onto that SHA):
`claimsintake`, `cmd/claims-worker`, `cmd/meta-admin`. Consumes the frozen Go and SQL of
`docs/delivery/units/meta-intake-core.md` unchanged.

**Goal:** a leased intake that created a bundle on a `private_reply` source plans exactly one
`meta.private_reply` operation; the dispatcher sends it once to Graph with a per-store Page token
that only `LoadSecret` ever sees; every non-2xx or doubt is UNKNOWN, never a second POST.

## Read (by section)
`docs/delivery/PROCESS.md`; `meta-claims-intake-v1.md` §0, §5.3, §5.4, §6, §7, §9, §14, §15;
`external-dispatcher-v1.md` "Frozen Go surface" + "Amendment proposal"; `external-operation-v1.md`
UNKNOWN/Reconcile rules. Code by symbol: `internal/integrations/core/dispatcher.go`
(`DispatchRoute`, `compileDispatchRoutes`, `validDispatcherOptions`, `runOperation`,
`finalDispatchGate`, `invokeOutcome`), `service.go` (`externalOperationArgs`, `Plan` InsertTx);
`internal/claims/credentials.go` (`LinkToken`, `ParseLinkToken`); `internal/integrations/meta/env.go`
(`LoadPayloadKeyring` format); `internal/integrations/accounts/crypto.go` (AES-GCM seal/open);
`cmd/payment-worker/main.go`, `cmd/stripe-admin/main.go` (host + registrar CLI patterns);
`internal/jobqueue/run.go`; `tests/foundation/dispatcher_test.go` (T06 gates stay green).

## FROZEN Go interface (meta-intake-tests calls exactly these)
```go
package claims // internal/claims/system_link.go (pure; stdlib only)
type ReplyLinkKey struct{ /* unexported */ } // String/GoString/Format/MarshalJSON = "[redacted]"
func NewReplyLinkKey(raw []byte) (ReplyLinkKey, error) // exactly 32 bytes, not all-zero, copied; else command.ErrInvalid
func (k ReplyLinkKey) ID() string // hex(HMAC-SHA256(k, []byte("meta-claim-link-key-id/v1")))[:16]
func SystemLinkToken(k ReplyLinkKey, tenantID, storeID, bundleID, operationID string) (LinkToken, error)
//  base64url_raw(HMAC-SHA256(k, json.Marshal([]string{"meta-claim-link/v1",tenant,store,bundle,operation})))
//  passes ParseLinkToken; zero key or non-canonical UUID → command.ErrInvalid
package core // external-dispatcher-v1 amendment (IR-12)
type SecretClaim struct{ OperationID string; Generation int64; LeaseToken []byte } // redacted formatters
type Secret struct{ /* unexported */ } // String/GoString/Format/MarshalJSON/MarshalText = "[redacted]"
func NewSecret(b []byte) Secret // copies b
func (s Secret) Reveal() []byte // backing bytes; the dispatcher zeroes them after DispatchWithSecret returns
// DispatchRoute gains LoadSecret func(context.Context, pgx.Tx, SecretClaim) (Secret, error) and
// DispatchWithSecret func(context.Context, DispatchRequest, Secret) (Outcome, error): both set with
// Dispatch nil, or both nil with Dispatch set; anything else → NewDispatcher error.
func InsertOperationJob(ctx context.Context, jobs *river.Client[pgx.Tx], tx pgx.Tx, operationID string) (int64, error)
//  InsertTx external_operation_v1 {operation_id, version:1}, default queue, no opts (§5.4 guard shape)
package metareply // internal/integrations/metareply; Depends on graph.facebook.com
type PageTokenKeyring struct{ /* unexported */ } // redacted
func LoadPageTokenKeyring(getenv func(string) string) (*PageTokenKeyring, error)
//  COMMERCE_META_PAGE_TOKEN_ACTIVE_KEY_ID + COMMERCE_META_PAGE_TOKEN_KEYS_JSON, same format as the payload keyring
func NewPageTokenKeyring(activeID string, keys map[string][]byte) (*PageTokenKeyring, error)
type PageTokenScope struct{ TenantID, StoreID, BindingID, Provider, AssetID string; Version int64 }
func (k *PageTokenKeyring) Seal(s PageTokenScope, pageToken string) (keyID string, nonce, ciphertext []byte, err error)
func (k *PageTokenKeyring) Open(s PageTokenScope, keyID string, nonce, ciphertext []byte) (core.Secret, error)
type Registration struct{ TenantID, StoreID, PrincipalID, BindingID, Provider, AssetID string; ExpectedVersion int64; Scopes []string }
func RegisterPageToken(ctx context.Context, pool *pgxpool.Pool, keys *PageTokenKeyring, r Registration, pageToken string) (int64, error)
type Config struct{ GraphBaseURL, GraphVersion string; AuthorizationHeader bool; HTTPClient *http.Client }
func Routes(checkPool *pgxpool.Pool, linkKey claims.ReplyLinkKey, pageKeys *PageTokenKeyring, cfg Config) ([]core.DispatchRoute, error)
func RenderClaimLink(locale, origin string, token claims.LinkToken) (string, error)
package claimsintake // internal/claimsintake (phase B)
type Config struct{ Workers int; IdleSleep time.Duration } // 0 → 2 and 1s; Workers 1..8
func New(ctx context.Context, intakePool *pgxpool.Pool, linkKey claims.ReplyLinkKey, cfg Config) (*Poller, error)
func (p *Poller) ApplyOne(ctx context.Context) (leased bool, err error) // one lease+apply tx; tests drive it
func (p *Poller) Run(ctx context.Context) error // Workers goroutines of ApplyOne + IdleSleep until ctx done
```

## Behavior
- **Dispatcher**: in `dispatch` mode only, after `finalDispatchGate`, one DBTimeout-bounded tx →
  `LoadSecret` → end tx → `DispatchWithSecret` → zero secret. `ErrPolicyDenied` → BLOCKED_POLICY
  `credential_unavailable`; other error/panic → UNKNOWN `secret_load_failed`, zero calls. Reconcile
  and Check never see a Secret. Lease inequality `CallTimeout+3*DBTimeout+1s < lease` applies when
  any route sets `LoadSecret` (ruling (e) default; existing options unchanged otherwise).
- **Routes** `(facebook|instagram, meta.private_reply, service)`. Config: `GraphBaseURL` exactly
  `https://graph.facebook.com` or loopback `http://127.0.0.1:<port>` (MOCK); `GraphVersion`
  `^v[0-9]{1,3}\.[0-9]{1,2}$`, required, no default (U5 pin comes from MCI11); `checkPool` passes
  `platform.ValidateWorkerPool`. **Check**: request `link_key_id` ≠ `linkKey.ID()` → policy deny;
  re-derive token, one `SELECT claims.check_meta_reply($op, sha256(token))`; `OK` → nil; one of the
  five §6.3 codes → `fmt.Errorf("%s: %w", code, core.ErrPolicyDenied)`; anything else → plain error.
  **LoadSecret**: one `integration.load_meta_page_token` row → `Open` with scope from the row; zero
  rows or missing attested scope (FB `pages_messaging`; IG `instagram_manage_comments` +
  `pages_read_engagement`) → `ErrPolicyDenied`. **DispatchWithSecret**: re-derive token,
  `RenderClaimLink(locale, origin, token)` (request `origin`, core ruling (b)), `POST
  {base}/{version}/{asset_id}/messages` JSON `{"recipient":{"comment_id":…},"message":{"text":…}}`;
  token as `Authorization: Bearer` if `AuthorizationHeader`, else JSON field `access_token` (U6
  default false); never URL/query/log. 2xx with `message_id` (1..200 printable) → SUCCEEDED ref; every
  other result (4xx, 5xx, 429, timeout, garbled) → UNKNOWN `graph_unconfirmed` (FAILED_FINAL list
  empty until U3). Body read ≤64 KiB, never logged. **Reconcile** → UNKNOWN `reconcile_unproven`.
- **RenderClaimLink**: locale ∈ zh-TW|zh-CN|en; text = sentence + " " + `https://<origin>/<locale>/claim#t=<token>`;
  defaults (O6 owner may replace): zh-TW `感謝留言！點此確認你的喊單：`, zh-CN `感谢留言！点此确认你的下单：`,
  en `Thanks for your comment! Confirm your claim here:`. No merchant text, price or promise.
- **ApplyOne** (READ COMMITTED, statement 5 s, lock 1 s): `lease_meta_intake()`; none → commit,
  `(false,nil)`. Set `app.tenant_id/app.store_id` local; `claims.IngestMetaIntake`. `EventID==""` →
  DROPPED `window_closed`. ACCEPTED + `BundleCreated` + source `private_reply` →
  `claim_reply_plannable`; `OK` → new op UUID, `core.InsertOperationJob`, `SystemLinkToken`,
  `plan_claim_reply(intake, op, sha256(token), linkKey.ID(), job)`. Then APPLIED + `applied_event_id`,
  COMMIT. On error: rollback, then separate tx `fail_meta_intake(id, code, final)`: `invalid`
  (`command.ErrInvalid`/22023, final), `reply_key_conflict` (23505, final), `sqlstate_<lower>` final
  for 23514/23503/42501, non-final for everything else (40P01, 55P03, 57014, 08*, ctx timeout).
  Recorded failure → `(true,nil)`; unrecordable → `(true,err)`.
- **cmd/claims-worker**: `COMMERCE_CLAIMS_WORKER_ENABLED` ""/0/1; `COMMERCE_CLAIMS_INTAKE_DATABASE_URL`
  (`OpenClaimsIntakePool`), `COMMERCE_WORKER_DATABASE_URL` (`OpenWorkerPool`), same database;
  `COMMERCE_CLAIMS_REPLY_LINK_KEY` (std base64 32 B), page-token keyring, `COMMERCE_META_GRAPH_VERSION`,
  optional `COMMERCE_META_GRAPH_BASE_URL`, `COMMERCE_META_GRAPH_AUTH_HEADER`. River client on the worker
  pool: queue `default` only, one worker (`core.NewDispatcher` with `metareply.Routes`); poller alongside.
  One startup line listing the registered routes (IR-13). Never reads `COMMERCE_CLAIMS_ACTOR_KEY`,
  `COMMERCE_META_PAYLOAD_*`, `STRIPE_*`. Redacted config type as meta-worker.
- **cmd/meta-admin** `page-token`: flags tenant/store/principal/binding/provider/asset/expected-version/
  scopes; token from `META_PAGE_ACCESS_TOKEN`; DSN `COMMERCE_META_REGISTRAR_DATABASE_URL`; prints
  `{"version":N}` only. SQL EXECUTE grant is the authority check.
- Comments per PROCESS.md §5: Graph constants carry the §0 docs URL + 2026-09-28; each SQL call
  names its definer and why; UNKNOWN branches say why they never re-POST.

## Write paths
`internal/integrations/core/{dispatcher.go,secret.go,jobs.go,secret_test.go}`,
`internal/claims/{system_link.go,system_link_test.go}`, `internal/integrations/metareply/**`,
`internal/claimsintake/**`, `cmd/claims-worker/**`, `cmd/meta-admin/**`, `output/meta-intake-reply/**`.
Not: SQL, contracts, other `internal/claims` files, `internal/integrations/meta/**`, `tests/foundation/**`,
`apps/**`, go.mod (uuid/HTTP from stdlib or existing modules only).

## Verify
```sh
GOTOOLCHAIN=go1.27.1 go test -race -count=1 ./internal/integrations/core ./internal/integrations/metareply ./internal/claims ./internal/claimsintake ./cmd/claims-worker ./cmd/meta-admin
GOTOOLCHAIN=go1.27.1 go vet ./... && gofmt -l internal cmd
bash scripts/dev/test-focused.sh '^TestT06'   # dispatcher + external-operation regression (phase B)
```
Logs → main checkout `output/meta-intake-reply/`. MCI gates belong to `meta-intake-tests`.

## Non-goals / Return
No SQL, no Meta webhook/consumer change, no OAuth/refresh (T07), no LIVE send, no Reconcile read (U4).
Return per phase: commit SHA, model/reasoning, base, paths, commands + exit codes + PASS/FAIL/SKIP,
evidence, risks, NOT_RUN. Any deviation from a frozen signature = stop and escalate.
