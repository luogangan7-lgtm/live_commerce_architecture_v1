# Unit ads-tests — independent gates MA01–MA09, MA11 (MOCK / REAL_PG / BROWSER), fake Graph + fake OAuth

Role: test_worker, independent (≠ every ads implementer). Base SHA `00c1d94`. Worktree
`.worktrees/ads-tests`, branch `unit/ads-tests`. Write from the contract and the FROZEN blocks of
`ads-a10.md`, `ads-core.md`, `ads-graph.md`, `ads-ui.md`, `ads-capi.md` only; do not read or copy those
units' branches. No owner secrets, no production DSNs, no network, no LIVE. SANDBOX gates are skip-gated.
Author at dispatch; compile/run after the merges listed per phase.

## Read (by section)
PROCESS.md; `contracts/meta-ads-v1.md` §0 (AD1–AD11), §1 (F9, F13–F16, F19, F20, F22), §2, §3, §3.1,
§4 (all), §5, §6, §7, §8, §9, §12, all disposition tables (they list the exact failure each gate must
catch); ads-core Defaults D1–D14 + Frozen HTTP/SQL/op-request blocks; ads-a10 A10-D1..D4; ads-graph G1–G6;
ads-capi C1–C7; ads-ui U1–U8. Reuse by symbol: foundation fixtures (`fixture(t)`, `mustExec`, owner/runtime/
worker pools, `create_initial_store`; grep `tests/foundation/{meta_claims_intake_*,stripe_refund_*,
external_operation_authority}_test.go`), `metareply` MOCK harness shape (loopback `GraphBaseURL`), browser
harness `tests/foundation/browser_refund_fulfilment_test.go` + `scripts/dev/test-local.sh` modes.

## Tier rules
REAL_PG = PG 18 via `test-focused.sh`. MOCK = real PG + real River + real `core.NewDispatcher` with
`metaads.Routes`/`capiroute.Routes` pointed at the fake Graph on loopback. Ops, approvals, facts come from
the real definers (captured facts via the real capture path, never fabricated rows); controlled faults and
aged timestamps via the owner pool are disclosed per test. Merchants: creator + a second member with
explicit `ads:*` grants per case (A-1: no auto grants). Billing RESTRICTED / consent fixtures only through
customers-billing's definers. SANDBOX: `t.Skip("NOT_RUN: …")` unless `META_ADS_SANDBOX=1`.

## Fake servers (`tests/ads/fakegraph/**`)
From Meta docs + contract §1/§3 only: campaigns/adsets/adcreatives/ads create + list (name tag, paging),
`POST /{campaign}` status, `GET /{campaign}?fields=status,effective_status`, `/act_{id}` preflight,
`/{campaign}/insights`, `/{pixel}/events`, OAuth code exchange + `/me`, `/me/permissions`, `/me/adaccounts`,
`/act_{id}/adspixels`; programmable faults (timeout after create, 5xx, Graph error codes 4/17/613/80004/100,
unparseable body), per-call counters, captured request bytes, a token → account isolation check. Package
comment says it is a MOCK and what it cannot prove (§9 last paragraph).

## Gates (top-level names exact; file prefix `meta_ads_`)
**Phase A** (needs F1 ads-a10, F2 ads-core + ads-graph merged and mounted):
- **MA01** `TestMetaAdsMA01Units` (`meta_ads_unit_test.go`, no PG): §9 row via `metaads.MetaBudget`,
  `SpendMinor`, `Encode/Parse{Preflight,Insights}` (≤200 chars, D2), `ads.CanonicalDraft` (D10 vectors),
  classification incl. activate/pause never FAILED_FINAL; phone/event_id parts in phase B.
- **MA02** `TestMetaAdsMA02Schema` + `TestMetaAdsMA02Allowance` (`meta_ads_schema_test.go`,
  `meta_ads_allowance_test.go`): §4.4 equality from ACLs/`pg_policies` (incl. D7/R-A and D8/R-B rows only if
  ruled), every §9 MA02 clause except the RESTRICTED ones; real two-transaction races with `pg_blocking_pids`
  witness; policy-removal mutations (ops read policy, events read policy) turn it red.
- **MA04** `TestMetaAdsMA04Chain`, **MA05** `TestMetaAdsMA05Unknown`, **MA06** `TestMetaAdsMA06Binding`,
  **MA07** `TestMetaAdsMA07Insights` (`meta_ads_flow_test.go`): §9 rows; zero-HTTP assertions by fake
  counters; BLOCKED_POLICY codes per D3; queue `ads` + priorities per D4; no activate ever after a pause (X7).
- **MA11** `TestMetaAdsMA11ReconcileSecret` (`meta_ads_a10_test.go`): §9 row; `go list -deps ./cmd/api`
  contains no `meta_ads/tokenopen` or `attribution/capiroute`; existing routes' outcomes unchanged
  (`Mode` ignored), coded denial vs bare denial (A10-D2).
- **Browser MA09a** `tests/admin/ads.spec.ts`, driver `tests/foundation/browser_meta_ads_test.go`
  (`//go:build browser`, `TestBrowserMetaAds`), new `test-local.sh --browser-meta-ads` mode: connect via
  fake OAuth (U1/U2 cookie + 303), `state_mismatch`, pick outside list refused, draft → approve → publish →
  pause → copy, allowance-off banner (O4), SANDBOX banner, report three blocks; 3 locales, 1586×992 + 390px.
**Phase B** (needs 0078/0079 + ads-capi merged):
- **MA02b** `TestMetaAdsMA02Billing`: RESTRICTED read as `commerce_ads_writer`; approve/publish 409, create/
  activate BLOCKED_POLICY zero HTTP, pause/insights/CAPI allowed.
- **MA03** `TestMetaAdsMA03Consent`, **MA08** `TestMetaAdsMA08CAPI` (`meta_ads_capi_test.go`): §9 rows +
  C2 (no user data outside the lease: stale generation/expired lease/non-CAPI op refused; `commerce_worker`
  has no SELECT on `storefront.destination_snapshots`), C5, DB-wide + log sentinel scan for raw/hashed phone.
- **Browser MA09b** `tests/storefront/ads-consent.mjs`: buyer grant → context row; withdraw → none (needs
  customers-billing buyer consent UI).
- **SANDBOX** `TestMetaAdsSandboxS1..S4`: skip-gated NOT_RUN unless owner env; never LIVE.

## PROCESS §5 (binding)
Each test file's header comment names the contract § and gate it proves and the tables/functions/hosts it
touches; the fake server's package comment names what it fakes and the docs URLs + retrieval dates of the
wire shapes it mirrors. Fake keys/tokens are split literals (PROCESS §6); no key-shaped literal.

## Red proof (PROCESS §2.4)
Per gate one targeted mutation of the merged candidate in a scratch copy (reverted) →
`output/ads-tests/red-<gate>.log`, then green. Compile failure is not red; zero matched tests or SKIP is never PASS.

## Write paths
`tests/foundation/{meta_ads_*,browser_meta_ads}_test.go`, `tests/ads/**`, `tests/admin/ads.spec.ts`,
`tests/storefront/ads-consent.mjs`, `scripts/dev/test-local.sh` (new mode only; integrator reviews),
`output/ads-tests/**`. Nothing else (not the implementers' unit tests, not `apps/**`, not contracts).

## Verify
```sh
GOTOOLCHAIN=go1.27.1 go vet ./tests/... && gofmt -l tests
LC_FOCUSED_TIMEOUT=2400s bash scripts/dev/test-focused.sh '^TestMetaAdsMA(01|02Schema|02Allowance|04|05|06|07|11)'
bash scripts/dev/test-local.sh --browser-meta-ads                                         # MA09a
LC_FOCUSED_TIMEOUT=2400s bash scripts/dev/test-focused.sh '^TestMetaAdsMA(02Billing|03|08)'   # phase B
META_ADS_SANDBOX=1 bash scripts/dev/test-focused.sh '^TestMetaAdsSandbox'                  # owner env only; else NOT_RUN
```
Logs with command, SHA, exit, PASS/FAIL/SKIP → `/Volumes/data/live_commerce_architecture_v1/output/ads-tests/`.

## NOT_RUN (expected)
MA-S1..S4 (owner sandbox + app 大梦 roles), MA-L1/L2 (owner chat approval, App Review), MA10 (integrator +
security_reviewer: token custody, OAuth callback, consent).

## Integrator hooks
Integrator adds the `--browser-meta-ads` mode to CI and the MA gates to `contracts/tasks.json` evidence.

## Return
Files; per gate the assertion list mapped to contract §/default IDs; red + green logs; NOT_RUN list; every
clause found untestable or ambiguous in the contract or the frozen briefs.
