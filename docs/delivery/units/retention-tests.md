# Unit retention-tests — U08 independent gates CRP01–CRP10 (claims retention purge + actor erasure)

Role: test_worker (mid tier), independent of `retention-core`. Write from `contracts/claims-retention-purge-v1.md`
(FROZEN 2026-09-30) and the FROZEN block + D1–D10 of `retention-core.md` only; never read or copy the
`unit/retention-core` branch. Base `00c1d94`. Worktree `.worktrees/retention-tests`, branch `unit/retention-tests`.
Author at dispatch against the frozen signatures; compile/run after F1 (0071 + platform block) / F2 (core merged +
exact-set test updates) / F3 (claims-worker hook). No network, no LIVE, no production DSN, no real buyer data: every
sender id, comment id, label, Page token and actor key is a synthetic sentinel (PROCESS §6; keys split-literal).

## Read (by section)
PROCESS.md §2.4, §4, §6; contract §0 (RD1–RD8), §1–§5, §7, §9, §10, §13 (F1–F6 each need a case); `retention-core.md`
FROZEN block, D4–D8, Integrator hooks. Reuse by symbol (read-only): foundation fixtures (`fixture(t)`, `mustExec`,
owner/runtime/buyer pools) and seeding helpers in `tests/foundation/{live_claims_test.go, live_claims_schema_test.go,
live_claims_ingest_test.go, meta_claims_intake_flow_test.go, meta_claims_intake_reply_test.go,
meta_claims_intake_schema_test.go}` (manual/Meta bundles, links, intake rows, reply operations), meta-consumer social
row seeding (grep `social.comment_events` in `tests/foundation`), process-test pattern `expiry_runtime_test.go`
(build + run a `cmd/*` binary with env), `migration` fresh/upgrade helpers used by MCI02.

## Tier rules
REAL_PG = PG 18 via `test-focused.sh`. Retention logins are created by the test as real `LOGIN` roles granted exactly
`commerce_retention_job` / `commerce_retention_operator`; definers are always reached through those logins, never the
owner pool. Aged timestamps, held row locks and `pg_stat_activity`-gated interleavings via the owner pool, disclosed
per test in evidence. No `time.Sleep` for ordering (CRP05: wait on `pg_locks`/`pg_stat_activity` states).

## Gates (top-level names exact: `TestClaimsRetentionCRPnn<Name>`; one per gate)
- **CRP01** UNIT, files `internal/retention/retention_gate_test.go` (package `retention_test`) +
  `cmd/retention-admin/admin_gate_test.go` (package `main`, via frozen `run`): contract §7 row — selector exactly-one,
  strict stdin (unknown key, both keys, trailing data, empty without `--bundle`), sender/comment id never accepted in
  argv, D7 tombstone file strictness, D8 limits; `%v/%+v/%#v/json` of Selector/Counts/config print no sentinel;
  `meta.SocialPeerKey` equals the peer key the consumer projection writes (drive `meta` public ingest/projection, not
  `tupleHash`), `ClaimActorKey` vectors unchanged; exit-code map; job-DSN-only → `status` works, every other
  subcommand exit 2. Job loop stop on `more=0`/`busy`/20 batches needs PG → assert in CRP09.
- **CRP02** REAL_PG `TestClaimsRetentionCRP02Schema`: §7 row verbatim — fresh + populated-0066 upgrade, migrate twice,
  55000 without 0064, role attributes, **§4 matrix equality** (column/table privileges, schema USAGE, EXECUTE, direct
  + inherited, from `information_schema`/`pg_*`), FORCE RLS + `pg_policies` quals, `prosecdef`/owner/`proconfig`
  (`search_path`, `lock_timeout=2s`), 42501 for every listed non-retention role, lock-only UPDATE cannot change a
  value, both pool validators reject mixed/SET ROLE/owner-reachable logins and every other validator rejects a login
  reaching a retention role, KC03 + MCI02 still pass (run them), string count + two selectors rejected,
  `social_days > intake_days` 23514 / `set_retention_policy` 22023, reserved label on an existing row → 55000; plus
  D4 once ruled: `customers.apply_erasure`'s `erased-<own id hex>` accepted, `erased-<other bundle id hex>` 23514.
- **CRP03** `…CRP03ReportOnly`: §7 row; checksum (`md5(string_agg(row::text order by pk))`) of every purge target.
- **CRP04** `…CRP04EnforcedPurge`: §7 row per class C1–C6 at threshold −1 s / +1 s; untouched set; token preview/
  redeem → not found; `issue_link` → PT404 **and** HTTP 404 through the merchant handler; F1 `purged-<8 hex>` case;
  C4 redaction shape (`redacted:true`, `mpr-purged:<id>`, `request_hash` unchanged); second run no-op; `more=1`.
- **CRP05** `…CRP05Concurrency`: §7 row; every interleaving listed; `pg_locks.objid` shows the one advisory key for both
  `run_retention` and `erase_actor`; F5 case (non-terminal inbox job → row kept, consumer retry `ALREADY`, not XX000).
- **CRP06** `…CRP06Erasure`: §7 row; selectors a/b/c-meta/c-manual; two stores/sessions; untouched actor/peer
  byte-identical; four hold cases incl. intake 7 d 23 h → `held`, `retry_after`, nothing written; replay/PT409/PT404;
  F1 labels.
- **CRP07** `…CRP07Privacy`: DB-wide scan of every text/jsonb/bytea column (incl. `river.river_job.args`) + captured
  stdout/stderr of `retention-admin` and the claims-worker process: no sentinel; erased key absent; its sha256 only in
  `retention_log.actor_digest`.
- **CRP08** `…CRP08RestoreReplay` (MODEL of restore until T20): isolated fresh DB; red = no replay keeps the key;
  green = replay from log rows and from a D7 tombstone file (missing inserted, existing not duplicated), `replay` row,
  hold ignored.
- **CRP09** `…CRP09Worker` in `tests/foundation/claims_retention_worker_test.go` (build + run `cmd/claims-worker`,
  `expiry_runtime_test.go` pattern): periodic job registered, RunOnStart inserts one `claims_retention_v1`, restart
  within the hour inserts none, work runs as `lc_retention_job` (assert `executed_by`), DB fault → River retry with
  earlier batches committed, stop on `more=0`/`busy`/20 batches, missing/invalid/mixed DSN → fixed code, smoke config
  carries only the job DSN. Evidence label MOCK (River) per contract.
- **CRP10** `…CRP10SourceGuards` (`claims_retention_guard_test.go`): §7 source guards (definer callers only in
  `internal/retention`; `COMMERCE_CLAIMS_ACTOR_KEY` loaded only by `cmd/meta-worker` + `cmd/retention-admin`;
  operator DSN name absent from `deploy/**`, `deploy/scripts/smoke.sh`, `.github/workflows/*`; no `net/http` or new
  import in retention code) + regression run list (KC01–15, MCI01–10, MC/MIso, CB05 when 0078 present, full
  `go test -race ./...`, `go vet`, `check_packet.py`). Completed by a separate security_reviewer verdict (not this unit).
- **CRP11** LIVE read-only: NOT_RUN (owner approval, app 4291253377792879 test user; contract §7).

## Write paths
`tests/foundation/claims_retention_test.go`, `tests/foundation/claims_retention_{worker,guard}_test.go`,
`internal/retention/retention_gate_test.go`, `cmd/retention-admin/admin_gate_test.go`,
`output/u08-claims-retention/**` (contract §10(2) evidence dir). Nothing else (not core's unit tests, not
`cmd/claims-worker/**`, not exact-set tests KC03/MCI02/MCI10, not `scripts/dev/*`, not contracts).

## Comments (PROCESS §5)
Each test function starts with a comment citing the contract row/decision it proves (`// CRP06 + RD5: 7 d 23 h
intake → held`); helpers name the table/definer they seed and why (`// claims.meta_intake: seeded as owner, aged
received_at, disclosed in evidence`). No hand-written dependency lists.

## Verify
```sh
GOTOOLCHAIN=go1.27.1 go vet ./tests/foundation ./internal/retention ./cmd/retention-admin
GOTOOLCHAIN=go1.27.1 go test -race -count=1 -run '^TestClaimsRetentionCRP01' ./internal/retention ./cmd/retention-admin
LC_FOCUSED_TIMEOUT=2400s bash scripts/dev/test-focused.sh '^TestClaimsRetentionCRP(0[2-9]|10)'
LC_FOCUSED_TIMEOUT=2400s bash scripts/dev/test-focused.sh '^Test(LiveClaims|MetaClaims|MetaConsumer|CustomersBillingCB05)'
python3 scripts/check_packet.py
```
Red proof per gate (PROCESS §2.4): one targeted mutation of the merged candidate in a scratch copy (reverted) →
`output/u08-claims-retention/red-<gate>.log`, then green. Suggested reds: CRP01 print `ActorKey` in `String()`;
CRP02 drop one policy / grant `commerce_worker` EXECUTE; CRP03 write in report mode; CRP04 drop the C2 window recheck;
CRP05 use `pg_advisory_xact_lock(hashtext(...))` in one definer; CRP06 remove the 8-day hold; CRP07 log the selector;
CRP08 skip replay insert; CRP09 omit `UniqueOpts.ByPeriod`; CRP10 load the actor key in claims-worker. Compile
failure, zero matched tests or SKIP is never PASS.

## Order
Author at dispatch (second writer slot). CRP02–CRP08 need F1; CRP01 + CLI parts of CRP07 need F2; CRP09 + worker
parts of CRP07 need F3; CRP10 after F3. Security review runs on the merged candidate after CRP02–CRP09 are green.

## Return
Files; per gate the assertion list mapped to contract text / brief default; red + green logs with command, SHA, exit
code, PASS/FAIL/SKIP counts; NOT_RUN (CRP11); every clause found untestable or ambiguous (escalate, do not guess),
in particular whether D4 (IR-U1) was ruled before CRP02 ran.
