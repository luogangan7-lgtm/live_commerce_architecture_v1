# Unit retention-core — U08: 0071 claims retention SQL, internal/retention job + operator API, cmd/retention-admin

Role: commerce_worker (mid tier). Base `00c1d94`. Worktree `.worktrees/retention-core`, branch `unit/retention-core`.
No delegation, no network, no Meta call, no production DSN. Parallel with every other R2 unit (depends on 0060/0064
only; contract §8) and with `retention-tests` (writes against the FROZEN block below, never against this branch).
Contract: `contracts/claims-retention-purge-v1.md` (FROZEN 2026-09-30) incl. §12 IR-1..IR-7 (accepted as written);
the "Defaults adopted" below bind unless the integrator overrides. Rulings: `r2-design-rulings.md` X4 (W1 lapses when
§10 holds), X6 (frozen; owner questions OQ1–OQ3 keep their defaults), R-6 (0071 released to U08).

**Goal:** close the claims production-mount blocker: hourly purge of expired claims identity/intake/reply-ledger/social
rows (report-only until the policy is enforced) and synchronous operator erasure of one actor, with an audit log that
holds counts and a replayable digest only.

## Read (by section; grep headings, `sed -n` ranges)
PROCESS.md §2–§6; contract §0 (facts, RD1–RD8, rejected), §1–§5, §8, §9, §13. By symbol only:
`0060_live_claims.sql` (`claims.bundles` :90-115 label CHECK + `claims_bundle_label`, `claims.links`, `claims.lines`,
`live.claim_windows`, `issue_link` PT404, `commerce_claims_writer` definer pattern), `0064_meta_claims_intake.sql`
(`claims.meta_intake` :150-200, `plan_claim_reply` semantic_key, §4.3 lock-only UPDATE + pool pattern),
`0028_meta_inbox.sql:372-392` (`purgeable`, `lock_purgeable`), `0029` (`social.*`, `social_terminal`),
`0066` (operator-CLI grants), `migrations/migrate.go:125-140`; `internal/integrations/meta/{claim_intake.go:29-95
(LoadClaimsActorKey, ClaimActorKey), protocol.go:294 (tupleHash), projection.go:79}`, `internal/platform/
{claims_intake.go, platform.go:115 (openPool), :197-345 (validatePoolAuthority)}`, `cmd/meta-admin/main.go` (stdin,
exit codes, fixed stderr), `cmd/claims-worker/main.go:109-160` (read only: pools, River client).

## Defaults adopted (P2s the frozen text left open)
- D1 **`meta.SocialPeerKey` is built here** (contract §8 names integration_worker): a new file only,
  `internal/integrations/meta/social_peer.go`, body `return tupleHash("meta-social-peer/v1", app, object, asset, sender)`.
  Reason: `cmd/retention-admin` needs it to compile; a separate unit would be an unfrozen upstream. `projection.go`
  is not edited (CRP01 proves equality).
- D2 **`cmd/claims-worker` wiring is an integrator hook** (contract §8 names integration_worker): cvs-ecpay also
  edits that `main.go`. This unit exposes `NewWorker` + `PeriodicJob`; the integrator adds ~15 lines (hooks below).
- D3 `internal/platform/retention.go` (new file) is assigned here; the two authority keys + reachability probe in
  `platform.go` `validatePoolAuthority` are assigned here as an F1 hand-off (refund-core step-2 precedent), limited
  to one block modelled on the `claims_intake` probe and its `roleValid` case. No other `platform.go` line.
- D4 **Cross-contract conflict, escalated (IR-U1, blocks F1 merge only):** customers-billing-v1 CD7
  `customers.apply_erasure` (0078) sets `label='erased-'||replace(id::text,'-','')` with `purged_at` NULL; 0071
  `bundle_label_reserved` rejects that with 23514, so CB05 fails once both land. Default fix (integrator to rule):
  the CHECK gains the disjunct `OR label = 'erased-'||replace(id::text,'-','')` (a bundle may carry only its **own**
  id; a merchant cannot pre-occupy another bundle's label, so §13 F1 stays closed; U08 labels stay random). CRP06's
  23514 cases (random 32 hex) are unchanged. Do not implement any other workaround.
- D5 Count keys (all integers, booleans as 0/1). Run: `enforced, links, bundles, intake, operations, comment_events,
  messages, conversations, more` (contract §3 exactly; C6 deletions uncounted) (+ `busy` alone when the lock is taken). Erasure: `bundles, lines, links,
  intake, comment_events, operations, messages, conversations` (+ `replayed`). Replay row: same erasure keys summed +
  `tombstones, inserted, social_deferred`. Held: `held, retry_after`.
- D6 CLI stdout: erase → first line `request=<uuid>`; then sorted `key=value` lines, numeric values only; held →
  `retry_after=<RFC 3339 UTC>`. `status` → `enforced= version= link_days= intake_days= claims_days= social_days=
  last_run_unix= last_run_more=` (smoke parses `enforced=1`, `last_run_unix=`).
- D7 `--tombstones-file` = JSON array (≤ 10 000) of objects with exactly `request_id` (uuid), `selector_digest`
  (64 lowercase hex), `actor_digest` (64 hex | null), `bundle_tenant|bundle_store|bundle_ref` (uuid | null);
  unknown/missing keys, trailing data → usage (exit 2). Read with a 4 MiB cap.
- D8 Selector input limits: `--object` ∈ {`page`,`instagram`} (0064 CHECK), `--asset` `^[0-9]{1,32}$`, `--app`
  repeatable 0..8 `^[0-9]{1,32}$` (only with a `sender_id`), stdin ≤ 4 KiB, `sender_id` `^[0-9]{1,32}$`, `comment_ref`
  `^[0-9_]{1,80}$`. Object→platform mapping for the bundle lookup = the one `plan`/ingest already uses (read 0064,
  do not invent).
- D9 Worker: `Timeout()` 5 min; default River retry; `Kind()` `claims_retention_v1`; args `{}`; queue `default`
  (claims-worker's only queue); each `SELECT claims.run_retention(500)` in its own tx on the retention-job pool.
- D10 Exact-set regressions (KC03, MCI02, MC/MIso social grants, meta-inbox function grants, MCI10 actor-key guard,
  T06 if it enumerates definers) are **not** edited here: list test + assertion + before/after (§6 clauses 1, 2, 4, 5)
  for the integrator.

## FROZEN Go interface (retention-tests and the integrator call exactly these)
```go
package retention // internal/retention
const JobKind = "claims_retention_v1"
type JobArgs struct{}
func (JobArgs) Kind() string                   // JobKind
func (JobArgs) InsertOpts() river.InsertOpts   // UniqueOpts{ByPeriod: time.Hour}
type Worker struct{ river.WorkerDefaults[JobArgs] /* unexported pool */ }
func NewWorker(jobPool *pgxpool.Pool) (*Worker, error)  // nil pool → error; caller validated the pool
func (w *Worker) Work(ctx context.Context, job *river.Job[JobArgs]) error // ≤20 batches, stop on more=0|busy=1
func (w *Worker) Timeout(*river.Job[JobArgs]) time.Duration
func PeriodicJob() *river.PeriodicJob          // PeriodicInterval(time.Hour), RunOnStart: true

type Counts map[string]int64                   // D5 keys only; String/GoString/Format/MarshalJSON print numbers only
type Held struct{ RetryAfter time.Time }       // zero value = not held
type Selector struct {                         // redacted String/GoString/Format/MarshalJSON: never a key, ref or id
    Request             string   // uuid
    Object, Asset       string   // selectors a, b
    ActorKey            string   // a: 64 hex, derived by the caller (meta.ClaimActorKey)
    PeerKeys            []string // a: 0..8 × 64 hex (meta.SocialPeerKey per --app)
    CommentRef          string   // b
    Tenant, Store, Bundle string // c
}
type Policy struct{ Enforced bool; LinkDays, IntakeDays, ClaimsDays, SocialDays int }
type Status struct{ Policy; Version, LastRunUnix int64; LastRunMore bool }
type Tombstone struct{ RequestID string; SelectorDigest, ActorDigest []byte; BundleTenant, BundleStore, BundleRef string } // "" ≡ NULL
func RunOnce(ctx context.Context, pool *pgxpool.Pool, limit int) (Counts, error)
func Erase(ctx context.Context, pool *pgxpool.Pool, s Selector) (Counts, Held, error)
func SetPolicy(ctx context.Context, pool *pgxpool.Pool, expectedVersion int64, p Policy) (int64, error)
func GetStatus(ctx context.Context, pool *pgxpool.Pool) (Status, error)
func Replay(ctx context.Context, pool *pgxpool.Pool, tombstones []Tombstone) (int64, error) // nil → from log
var ErrUsage, ErrNotFound, ErrConflict, ErrBusy error  // 22023, PT404, PT409, 55P03; anything else → fixed generic error

package meta // internal/integrations/meta/social_peer.go (D1)
func SocialPeerKey(app, object, asset, sender string) string

package platform // internal/platform/retention.go (D3)
func OpenRetentionJobPool(ctx context.Context, dsn string) (*pgxpool.Pool, error)
func ValidateRetentionJobPool(ctx context.Context, pool *pgxpool.Pool) error
func OpenRetentionOperatorPool(ctx context.Context, dsn string) (*pgxpool.Pool, error)
func ValidateRetentionOperatorPool(ctx context.Context, pool *pgxpool.Pool) error

package main // cmd/retention-admin — test seam for CRP01 (retention-tests)
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int
```
Exit codes (contract §5): 0 done/replayed, 2 usage (incl. job DSN on any subcommand but `status`), 3 held, 4
not_found, 5 conflict/busy, 1 other with one fixed stderr code. `status` uses the operator DSN when set, else the job
DSN via `ValidateRetentionJobPool`.

## Build
1. **SQL** `0071_claims_retention.sql`: §2 exactly (preconditions 55000, roles, columns, CHECKs incl. D4 once ruled,
   NOT VALID + DO-block VALIDATE, indexes, both tables, FORCE RLS), RD8 trigger, §3 definers exactly (single advisory
   key `hashtextextended('claims-retention',0)`, SKIP LOCKED in `run_retention`, C2 window recheck under `FOR SHARE`,
   RD5 hold before any write, `lock_timeout='2s'`), §4 grants/policies exactly (CRP02 compares for equality), no
   post-River file (IR-4). `COMMENT ON` every table, column, function, role, policy.
2. **Platform** (D3): `retention.go` + the `platform.go` block; every other validator rejects a login reaching any of
   the three retention roles. **Hand steps 1–2 to the integrator first = F1.**
3. **`internal/retention`**: package doc; job/worker (D9); operator functions mapping SQLSTATEs; redacted types.
4. **`meta.SocialPeerKey`** (D1) + one vector test against `tupleHash` output.
5. **`cmd/retention-admin`**: `doc.go` (env list, subcommands, exit codes), strict stdin JSON
   (`json.Decoder.DisallowUnknownFields` + trailing-token check), `meta.LoadClaimsActorKey` only for selector (a),
   selectors/keys never in argv, logs or errors; every Open/Validate error → fixed code.

## Comments (PROCESS §5, reviewer checks)
Package comments: `retention` "owns the claims retention job and actor erasure calls; It never chooses rows (the
0071 definers do), never calls the network, never logs an id, key or comment ref"; `retention-admin` "operator-only
CLI, no service starts it". Every definer call: `// claims.erase_actor: RD4 one tx, RD5 hold inside the lock`.
Every retry/UNKNOWN branch says why (`// busy: another run holds hashtextextended('claims-retention',0); next hour
retries`). SQL: `COMMENT ON` names `internal/retention`, the allowed roles and non-goals. No hand-written
depends-on lists (integrator regenerates `dependency-map.md`). No new module (stdlib + pgx + River already in go.mod).

## Write paths
`migrations/0071_claims_retention.sql`, `internal/retention/**` (except `*_gate_test.go`),
`cmd/retention-admin/**` (except `*_gate_test.go`), `internal/integrations/meta/{social_peer.go,social_peer_test.go}`,
`internal/platform/{retention.go,retention_test.go}`, `internal/platform/platform.go` (D3 block only),
`output/retention-core/**`. Forbidden: `cmd/claims-worker/**`, `cmd/meta-worker/**`, other `internal/integrations/
meta/*`, `internal/claims/**`, `tests/**`, `deploy/**`, `.github/**`, `docs/runbooks/**`, contracts, OpenAPI,
`go.mod/go.sum`, `apps/**`.

## Verify (unit tests only; names must not start with `TestClaimsRetentionCRP`)
```sh
GOTOOLCHAIN=go1.27.1 go vet ./... && gofmt -l internal cmd && bash scripts/dev/check-pkgdocs.sh
GOTOOLCHAIN=go1.27.1 go test -race -count=1 ./internal/retention/... ./cmd/retention-admin/... ./internal/integrations/meta/... ./internal/platform/...
LC_FOCUSED_TIMEOUT=1800s bash scripts/dev/test-focused.sh '^Test(LiveClaims|MetaClaims|MetaConsumer|MetaInbox|T06|Pool)'
python3 scripts/check_packet.py
```
Unit tests: flag/stdin parsing table, SQLSTATE → error → exit code, Counts/Selector formatting, `SocialPeerKey`
vector. Expected-red regressions from exact-set tests (D10) are listed, not fixed. Logs → `output/retention-core/`.

## Integrator hooks (only the integrator edits these; the unit exposes the functions above)
- `cmd/claims-worker/{main.go,doc.go,main_test.go}` (F3): env `COMMERCE_RETENTION_JOB_DATABASE_URL` required when
  enabled; `platform.OpenRetentionJobPool` + `sameDatabase` with the worker pool; `river.AddWorker(workers,
  must(retention.NewWorker(jobPool)))`; `river.Config.PeriodicJobs: []*river.PeriodicJob{retention.PeriodicJob()}`;
  fixed error codes; env-sentinel test list. Merge with cvs-ecpay's edit of the same file.
- `deploy/postgres/logins.tsv` + `deploy/secrets.manifest.tsv` + `deploy/compose.yml` (claims-worker
  `COMMERCE_RETENTION_JOB_DATABASE_URL_FILE`): `lc_retention_job` only. `lc_retention_operator` is created by the
  runbook on owner approval and its DSN never reaches the deploy host or CI (§10(5), F2).
- `deploy/scripts/smoke.sh`: `retention-admin status` on the job DSN, assert `enforced=1` and
  `now - last_run_unix < 26 h` once claims are mounted; build `retention-admin` into the image (Dockerfile/Makefile).
- §6 clauses 1–7 recorded in the named contracts; exact-set tests from D10 updated to frozen values only;
  `docs/runbooks/claims-data-deletion.md`; `contracts/tasks.json` U08 evidence; `scripts/dev/depmap.sh` regen;
  IR-U1 (D4) ruling recorded in `r2-design-rulings.md`.

## Gates
Implementer runs unit tests only. CRP01–CRP10 belong to `retention-tests` (independent, red-then-green); CRP10 also
needs a security_reviewer verdict on definers, grants, stdin/log handling. CRP11 LIVE NOT_RUN (owner approval).

## Order / Non-goals / Return
F1 = steps 1–2 merged (unblocks CRP02–CRP08 runs); F2 = unit merged + D10 test updates (CRP01, CLI); F3 = claims-
worker hook (CRP09). Non-goals: Meta deletion callback, request queue, K_actor rotation, whole-bundle DELETE,
`meta_private`/order retention, UI, production `policy-set`/`erase` (owner approval, §10(3)). Return commit SHA,
model/reasoning, base, paths, commands + exit codes + PASS/FAIL/SKIP counts, evidence, D10 regression list, how
D1–D10 were handled, risks, NOT_RUN. Any deviation from the frozen signatures = stop and escalate.
