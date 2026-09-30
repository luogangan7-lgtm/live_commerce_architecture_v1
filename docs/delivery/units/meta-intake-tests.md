# Unit meta-intake-tests — MCI01–MCI12 gates and KC01–KC16 regression

Role: test_worker, independent (must not be the author of `meta-intake-core` or `meta-intake-reply`).
Worktree `.worktrees/meta-intake-tests`, branch `unit/meta-intake-tests`. No delegation. Write from
the contract and the frozen signatures, **not** from the implementation. May start writing at base
`8f491dc` in parallel with both implementers; runs against the integrator's merge SHA that
contains both (record it). No Meta token, no send, no non-loopback network.

## Read
`docs/delivery/PROCESS.md` (§2.4 red-then-green, §4 evidence); `contracts/meta-claims-intake-v1.md`
in full (§12 is the gate list; §10 says what each class can prove); frozen Go + SQL blocks of
`docs/delivery/units/meta-intake-core.md` and `meta-intake-reply.md`; `live-keyword-claims-v1.md`
§11 (KC gates). Reuse helpers by symbol, do not copy: `miSetup`, `miRoute`, `miPost`,
`miSignature`, `miInstagramRoute` (signed webhook through the real handler), `mcConsumer`,
`mcAwait`, `mcKeys` (consumer), `lcSetup`, `lcPrincipal` (claims), `t06StartDispatcher`,
`t06DispatchOptions`, `t06Await` (dispatcher).

## MOCK harness (one, shared by MCI04–MCI09)
Signed synthetic FB `feed` and IG `comments`/`live_comments` bodies → real ingress handler → inbox
→ `meta.NewConsumerWorkerWithClaims` (actor key from `crypto/rand`) → `claimsintake.Poller.ApplyOne`
(no sleeps; drive it) → `core.NewDispatcher` with `metareply.Routes` whose `GraphBaseURL` is an
`httptest` server on 127.0.0.1. The fake Graph counts POSTs per `comment_id`, records headers and
body, and can return 2xx/`message_id`, 4xx, 5xx, 429, garbled, hang. Page token registered via
`metareply.RegisterPageToken` with a synthetic sentinel token; it may appear **only** in the fake
Graph's received request. Link and actor keys: distinct random 32 B per test. All text, names,
ids synthetic sentinels (grep-able). Concurrency is proven with `pg_stat_activity` waits, never sleeps.

## Gates (contract §12 is the requirement text; one `TestMetaClaimsMCIxx…` per gate)
| Gate | Tier | Where | Notes beyond §12 |
| --- | --- | --- | --- |
| MCI01 | MOCK (UNIT) | `internal/integrations/meta/claim_intake_gate_test.go` (package meta: `qualifyClaim`), `internal/claims/system_link_gate_test.go`, `internal/integrations/core/secret_gate_test.go` | Equal key bytes across `ClaimActorKey`, social `tupleHash` peer key, `labelMAC`, `SystemLinkToken` → pairwise unrelated; `ReplyLinkKey.ID` vector; every new type redacted under `%v %+v %#v %s` and JSON |
| MCI02 | MOCK (REAL_PG) | `tests/foundation/meta_claims_intake_schema_test.go` | §4.3 delta computed as catalog diff vs a pre-0064 snapshot, compared for equality (direct + inherited, schema USAGE, EXECUTE); fresh + populated-0063 upgrade, migrate twice |
| MCI03 | MOCK (REAL_PG) | `…_schema_test.go` | `live.put_claim_source` from a `commerce_runtime` merchant tx |
| MCI04 | MOCK (REAL_PG + River) | `…_flow_test.go` | child-process kill: build `cmd/claims-worker` into `t.TempDir()`, SIGKILL while its apply waits on a lock held by the test; §5.4 guard probes from the intake login |
| MCI05 | MOCK (REAL_PG) | `…_flow_test.go` | ×20 concurrent consumer runs; two apps routed to one asset; IG kind pair; DB-wide keyword sentinel scan |
| MCI06 | MOCK (REAL_PG) | `…_flow_test.go` | IG delivery-time case uses `entry.time` only; rate bound with 10 concurrent applies |
| MCI07 | MOCK (REAL_PG + fake Graph) | `…_reply_test.go` | every Check deny → BLOCKED_POLICY and fake Graph POST count 0; Secret instrumentation: wrap Check/Reconcile to assert no argument or ctx value holds the token; child kill after the fake received the POST → exactly 1 POST ever |
| MCI08 | MOCK (REAL_PG) | `…_reply_test.go` | sentinel scan of every text/bytea/jsonb column in claims, live, integration, ops, river, meta_inbox + captured process logs; AAD field swap/tamper via `PageTokenKeyring.Open`; `cmd/meta-worker`, `cmd/api` sources never read `COMMERCE_CLAIMS_REPLY_LINK_KEY` / `COMMERCE_META_PAGE_TOKEN_*` |
| MCI09 | MOCK (REAL_PG) | `…_flow_test.go` | lock-order workload; zero 40P01 |
| MCI10 | REVIEW + regression | `tests/foundation/meta_claims_intake_guards_test.go` (`go/parser`) | callers: `Ingest/IngestParsed` only `RecordManualClaim`, `IngestMetaIntake` only `internal/claimsintake`; no `net/http`/`net` import in `meta/consumer.go`, `meta/claim_intake.go` or package `claimsintake`; `SecretClaim` referenced only by `LoadSecret` code; `internal/claims` imports per claims §4; main-`river` default-queue inserts are `external_operation_v1` only |
| MCI11 | LIVE read-only | `…_live_test.go` | written; `t.Skip` unless `META_LIVE_PAGE_ID` + `META_LIVE_PAGE_TOKEN` set; GET only (never POST); **NOT_RUN in R1** (harness has no Page token) |
| MCI12 | LIVE send | — | not written; NOT_RUN (needs O1–O5 and per-send owner approval in chat) |

Each MOCK gate records one red run (a named mutation of the implementation in a throwaway branch,
or the pre-fix commit) before its green run; list mutation + red log per gate. A SKIP, zero
matched tests or a missing log is never PASS.

## KC01–KC16 regression (with the §4.4 amendment in force)
```sh
GOTOOLCHAIN=go1.27.1 go test -count=1 ./internal/claims/grammar -run 'TestGrammarKC01|TestGrammarBoundaries'
GOTOOLCHAIN=go1.27.1 go test ./internal/claims/grammar -run '^$' -fuzz FuzzParse -fuzztime 60s   # KC01 fuzz
GOTOOLCHAIN=go1.27.1 go test -race -count=1 ./internal/claims/... ./internal/httpapi ./internal/buyerhttp ./cmd/api   # KC02, KC13, KC14 units
bash scripts/dev/test-focused.sh '^TestLiveClaims'              # KC02–KC15 real PG (KC03 as amended by clause 4/10)
bash scripts/dev/test-focused.sh '^TestBrowserLiveClaimsRealChain'   # KC16 browser
```
KC03 must pass with exactly the core unit's authorized edits; diff `live_claims_schema_test.go`
against `8f491dc` and fail the review if any assertion beyond §4.4 clauses 4/10 changed.

## Commands
```sh
GOTOOLCHAIN=go1.27.1 go test -race -count=1 ./internal/integrations/meta ./internal/integrations/core ./internal/integrations/metareply ./internal/claims ./internal/claimsintake
bash scripts/dev/test-focused.sh '^TestMetaClaimsMCI'
bash scripts/dev/test-focused.sh '^Test(MetaConsumer|MetaInbox|MetaIsolation|MetaRuntime|T06)'   # MC01–07, MI01–07, MIso, dispatcher
GOTOOLCHAIN=go1.27.1 go test -race ./... && GOTOOLCHAIN=go1.27.1 go vet ./... && python3 scripts/check_packet.py
```
Logs → main checkout `output/meta-intake-tests/` (command, SHA, exit code, PASS/FAIL/SKIP counts).

## Write paths
`tests/foundation/meta_claims_intake_{schema,flow,reply,guards,live}_test.go`,
`internal/integrations/meta/claim_intake_gate_test.go`, `internal/claims/system_link_gate_test.go`,
`internal/integrations/core/secret_gate_test.go`, `output/meta-intake-tests/**`. No non-test file;
no edit of any existing test (KC03/T06 edits belong to `meta-intake-core`).

## Disputes / Return
A red gate against the merged code is reported as a dispute (gate, contract clause, observed vs
required) — never fixed by editing the implementation or weakening the test. Return SHA tested,
model/reasoning, paths, per-gate tier + red/green evidence, commands + exit codes + counts,
disputes, NOT_RUN (MCI11, MCI12, anything skipped). Security review is a separate unit.
