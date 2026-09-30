# Delivery process (Release 1 onward)

Status: binding for every agent that writes code in this repo, from 2026-09-28.
Owner goal: ship a stable, deployable live-commerce SaaS as fast as possible, with automated
tests for every feature, real-browser tests for every page, and code that stays diagnosable.
Read this file first; then read only the contract sections your brief names (`AGENTS.md` rule:
load by section, never the whole `架构.md`).

## 1. Release plan

Release 1 (R1) = the smallest loop a merchant can actually sell with. Nothing below is cut from
the product; R2/R3 are sequencing, not scope reduction.

| Order | Unit | Contract | Evidence target |
| --- | --- | --- | --- |
| R1-1 | Stripe stage B1: `0061` + post-River SQL, fake Stripe server, payments/checkout/webhook inbox, HTTP/process/registrar (SP06–SP15, SP19–SP21) | `contracts/stripe-psp-v1.md` (FROZEN) | MOCK + REAL_PG, SP16 SANDBOX |
| R1-2 | Stripe stage B2: buyer payment UI, BFF, browser card 4242/decline/3DS (SP18) | same + UI amendment | BROWSER (SANDBOX) |
| R1-3 | Refund lifecycle on Stripe (merchant-initiated, REQUESTED→SUBMITTING→SUCCEEDED/FAILED/UNKNOWN, 架构 §12.3) | new amendment `stripe-refund-v1` | MOCK + SANDBOX + BROWSER |
| R1-4 | Manual fulfilment: merchant records carrier + tracking (架构 §13.4 manual path, audited), buyer sees it | new `manual-fulfilment-v1` | REAL_PG + BROWSER |
| R1-5 | T10c Meta comment intake: signed Meta comment webhook → claims ingest → first private reply with cart link | amendment `meta-claims-intake-v1` | MOCK (signed) + LIVE read-only probes |
| R1-6 | T12 end-to-end: comment → claim → cart → pay (4242) → order → ship → refund, admin + storefront | `tasks.json` T12 | BROWSER |
| R1-7 | Maintainability: package docs for every package, generated dependency map, CI staleness check | this file §5 | CI |
| R1-8 | T20/T21-lite security + fault review of R1 surface; deploy blockers (I8, runbook drift); `smoke.sh full` | `deploy/`, runbooks | smoke + review verdicts |

R2: T13 carriers / Taiwan CVS labels, T14 customer graph + consent, T17 billing, T18 ops UIs.
R3: T15 ads, T16 CAPI/catalog/audiences, T19 benchmarks, T22 pilot + rollback drill.

Owner-only items (engineering cannot close them): OIDC IdP choice (deploy B2), Stripe live
activation, Meta App Review / Access Tier, carrier contracts, production host + ADR O1.

## 2. Per-unit software-engineering flow

Every unit goes through these stages in order. A stage that fails twice on the same blocker
escalates to the integrator instead of looping (`AGENTS.md`).

1. **Brief** (integrator): `docs/delivery/units/<unit>.md` — goal, contract sections, write paths,
   gates, test commands, non-goals. Agents read the brief, not the chat.
2. **Contract** (integrator, only if the brief says so): amendment drafted, one adversarial
   review, then FROZEN. No implementation of a downstream piece before its upstream interface is
   frozen.
3. **Implement** (author agent, own git worktree under `.worktrees/<unit>` on branch `unit/<unit>`,
   only the brief's write paths). Unit tests for pure logic. `go vet`, `gofmt`, typecheck clean.
4. **Independent tests** (test author ≠ implementer): gate tests written from the contract, not
   from the implementation. Real PG via `bash scripts/dev/test-focused.sh '<regex>'`.
   Browser pages via Playwright. A test that cannot fail is not a test: each gate records one
   red run (mutation or pre-fix) before its green run.
5. **Review** (reviewer ≠ author): correctness + security on the unit diff, adversarial
   ("try to break it"). P0/P1 block merge.
6. **Integrate** (integrator): merge into the release branch, run the affected focused suites,
   push, let CI run the full suite. Record evidence (§4). Update `contracts/tasks.json`.
7. **Record**: Humaux memory (`fix`/`decision`/`rejected`), canvas step update.

## 3. Agents, models and token economy

| Role | Model tier | Why |
| --- | --- | --- |
| integrator (main loop) | top tier | contract rulings, merges, final verdicts |
| contract/security reviewer | top tier | money, tenancy and secrets paths |
| implementer, test author, UI author | mid tier (Sonnet) | well-specified code against a frozen contract |
| mechanical: run suites, summarize logs, grep inventories | small tier (Haiku) | no judgment needed |

Token rules for every agent:
- Start every task by reading this file, then the brief. The identical prefix is what makes
  prompt caching hit across agents; do not paraphrase it into your own notes.
- Read code by symbol (`grep -n`, `sed -n 'a,bp'`), not whole files. Never read `架构.md` or a
  contract end to end when the brief names sections.
- Return a short structured result (what changed, commands, exit codes, evidence paths,
  NOT_RUN list). Put long output in files under `output/` (gitignored), never in the reply.
- Don't re-run a green suite without a code change in between. Don't re-verify what the brief
  marks as already verified; cite the evidence file instead.
- One PG test run at a time machine-wide: `scripts/dev/test-focused.sh` serializes itself.

## 4. Evidence labels (`AGENTS.md`)

Every claim is one of DESIGN, MODEL_ONLY, MOCK, SANDBOX, LIVE, NOT_RUN. Zero tests matched, a
SKIP, a cancelled run or a missing log is never PASS. Evidence file = log under
`/Volumes/data/live_commerce_architecture_v1/output/<unit>/` — the **main checkout's** absolute
path, never the worktree's own `output/` (worktrees are deleted after merge; untracked files in
them are lost) — with command, commit SHA, exit code and top-level PASS/FAIL/SKIP counts.

## 5. Code comments and dependency annotations (anti-rot standard)

Goal: when something breaks at 3am, the code itself tells you who calls what, which table or
external service it touches and why, without archaeology. These rules generalize
`contracts/stripe-psp-v1.md` §15 (D9) to the whole repo.

**Package level** — every Go package has a package comment (in `doc.go` or the main file):
1. `// Package x owns …` — the single responsibility.
2. `// It never …` — the non-goals (what callers must not expect from it).
3. External services it calls name the host and why (`api.stripe.com`, `graph.facebook.com`).
   Internal "depends on / used by" is NOT hand-written: `docs/engineering/dependency-map.md` is
   generated from `go list` and CI fails when it is stale, so it is the single source of truth
   (hand-written lists rot; amended 2026-09-29).

**Call sites** — a call that crosses a package boundary into another domain, or any SQL on
another domain's table, carries a one-line comment naming the table/role/function and why:
`// payments.apply_capture: single stock writer; the worker never writes the ledger`.

**Money, stock, retries** — monetary comparisons carry `// I05:` + rule; stock transitions carry
`// §11.5:` + required evidence; every UNKNOWN/retry branch says why it retries the same key or
why it must not retry.

**External constants** — every third-party wire constant/parameter has its docs URL and the
retrieval date.

**SQL** — every new table, column, function and role gets `COMMENT ON` naming its owning
package, allowed roles and non-goals.

**Frontend** — every `apps/*` route/component file starts with a comment naming the BFF route(s)
it calls and the Go endpoint behind them.

**Dependencies** — adding a Go module or npm package requires a line in `docs/engineering/
dependencies.md` (what, why, who imports it, alternatives rejected). The generated
`docs/engineering/dependency-map.md` (package → imports → importers) is regenerated by
`scripts/dev/depmap.sh`; CI fails when it is stale.

Comments explain *why* and *what it touches*, not what the next line does.

## 6. Hard boundaries (restated from AGENTS.md; do not relax)

- No production deploy, real refund, live payment, message send to real users, or destructive
  migration without explicit owner approval in chat.
- Secrets only from env / `~/.config/livecommerce/secrets.env` (outside the repo); never in code,
  tests, logs, commits or agent replies. Stripe keys must be `sk_test_`/`rk_test_`.
  Fake keys in tests are written split (`"sk_" + "test_..."`) so no key-shaped literal exists;
  CI step "No key-shaped secret literals" enforces it.
  Synthetic DSNs are assembled with `net/url` (`url.URL{Scheme:"postgres", User:
  url.UserPassword(u, dsnSentinel1), Host:..., Path:...}`), with the sentinel in a neutrally named
  constant; leak assertions search for the constant. GitGuardian flagged every literal form
  (whole literal, split literal, `"postgres://u:" + const + "@host"`, `*Password*` constants) in
  2026-09-29.
- Tenant/store scope comes from server-side auth only.
- Migration numbers: this release branch owns 0060–0079. Only the integrator merges migrations,
  OpenAPI, shared JSON schema, go.mod/go.sum and pnpm-lock.
- Never delete or weaken a failing test to go green; fix the root cause.
