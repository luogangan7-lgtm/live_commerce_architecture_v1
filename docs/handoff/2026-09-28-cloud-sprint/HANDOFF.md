# Cloud sprint handoff — 2026-09-28 (paused for budget)

Branch `claude/gallant-bohr-9rs3yo`, draft PR luogangan7-lgtm/live_commerce_architecture_v1#1.
Paused by the owner (cloud credits). A new session continues from here. Read this file,
AGENTS.md, then the memory canvas element `agent:claude-ccr-live-commerce`.

## Landed on the branch (CI green)

GitHub CI `foundation` on `ce147fa` passed: 725 top-level tests, 0 FAIL, real PG18, race,
vet and govulncheck. Commits:

| Commit | What |
| --- | --- |
| c3878a4 | Replays return the same result regardless of host TZ (`command.InLocalTime`); test PG memory raised to 1g (Linux memcg OOM) |
| 2720f97 / ce147fa | CI timeout 60 min; govulncheck v1.8.0 (v1.1.4 panicked on Go 1.27) |
| 3d772e5 | Failure evidence written to repo `output/`, not the Mac-only `/Volumes/data` |
| 917cf46 (merged 465e08f) | Tests wait (fixed 5 s bound) for async PG backend teardown instead of snapshotting immediately; checkout `fetch-depth: 0` |
| 3b48d80 | Initial-store grant test compares the exact permission set |
| 5e273f7 | **T10 contract FROZEN**: `contracts/live-keyword-claims-v1.md` (see §0.1 integrator rulings) |
| 53f1c59 | Stripe PSP contract **DRAFT**: `contracts/stripe-psp-v1.md` (adversarial review was interrupted, not frozen) |

## Work in progress (NOT on the branch; saved as patches here)

Apply with `git am docs/handoff/2026-09-28-cloud-sprint/patches/<dir>/*.patch`, in this
order. Each was written by an isolated agent. None of it has been independently tested or
reviewed yet.

1. `t10-core/` — migration `0060_live_claims.sql`, `internal/claims` (+grammar kw-v1), `storefront.LockCartOwner`, vectors, and the author's own PG smoke `TestLiveClaimsCoreSmoke`. The author reported it green; that claim still needs verification.
2. `t10-ui/` (needs t10-core) — HTTP M1–M7 / B1–B2, the Studio Claims panel, storefront claim page, and the KC16 browser chain (`tests/foundation/browser_live_claims_test.go`, `tests/admin/claims-ui.spec.ts`). Committed by the ui worker before the pause. Run status is unknown.
3. `t10-tests/` (needs t10-core) — WIP independent PG gate tests KC02–KC15. **Interrupted, never run.**
4. `deploy-packaging-v1/` — `deploy/**` (Dockerfiles, compose, Caddy TLS, migration job, PG backup/PITR scripts, smoke) plus `docs/runbooks/{deploy,backup-restore,incident}.md`. Patches 1–3 were independently verified with a real docker build + compose smoke (verify-1 found failures F1–F3, which are now fixed, and the local smoke re-run passed). Patch 4 contains review-round edits that were **not re-verified**.

## Next steps (in order)

1. Apply t10-core. Then have an independent test_worker finish and run t10-tests: `bash scripts/dev/test-local.sh --live-claims`, or the focused `go test -run '^TestLiveClaims'` with a disposable PG. Fix product bugs at the root.
2. Apply t10-ui. Run `pnpm install --frozen-lockfile`, typecheck/build both apps, then `bash scripts/dev/test-local.sh --browser-live-claims` (Chromium at /opt/pw-browsers). Close the contract §0.1 P2 list (a)–(g), each with a test.
3. Independent security + correctness review of T10. Then a full suite and CI.
4. Apply deploy patches. Re-run the compose smoke (patch 4). Do a security/operability review of `deploy/**`.
5. Stripe: finish the adversarial review of `contracts/stripe-psp-v1.md` and freeze it with integrator rulings. The Stripe account's default currency is **HKD** (country HK); the contract must state store vs settlement currency. Then implement. Test tiers: MOCK + real sandbox + browser with card 4242.
6. T12 end-to-end loop, then minimal fulfilment/refund record, T20 security/fault injection, T21 review, T22 release.

## Credentials (values never in git or memory)

The owner uploaded Stripe sk_test and the Meta/Instagram/Threads app id and secret. In the
paused container they were written to `/root/.config/livecommerce/secrets.env` (chmod 600,
outside the repo). **A new container does not have that file.** Ask the owner to upload the
same file again, then regenerate it with the same variable names:
`STRIPE_SECRET_KEY, META_APP_ID/SECRET, INSTAGRAM_APP_ID/SECRET, THREADS_APP_ID/SECRET,
COMMERCE_META_APPS_JSON (page=Meta app, instagram=IG app, random verify_token),
COMMERCE_META_PAYLOAD_ACTIVE_KEY_ID + COMMERCE_META_PAYLOAD_KEYS_JSON (random 32-byte key)`.

Read-only verification results:
- Stripe: acct_1UJDb0RusP6Wwj7e, livemode=false.
- Meta app 「大梦」: client_credentials OK.
- Threads: OK.
- Instagram Login app: cannot be checked with client_credentials; still unverified.

The owner should reset the Meta, IG and Threads app secrets after launch, because they
appeared in chat.

## Known risks / notes

- The full local suite on this 4-vCPU container takes about 25 min. The 90 s real-clock gates (MRR/LMR) are load-sensitive, so don't run heavy docker builds at the same time.
- `migrations.Apply` releases its lock only by closing the connection. A back-to-back Apply could, in theory, return busy. We could not reproduce this, so there is no product change.
- The pinned pre-LMR source 395b10d is reachable only through `origin/commerce/*` branches. Keep those branches.
- Migration numbers 0044–0059 are left for the local Codex lanes. This lane uses 0060 (T10) and 0061 (Stripe).

## Update — continued session (2026-09-28, later)

All WIP patches below are now **merged on the branch**. The patch files are kept only for history.

| Commit | Result |
| --- | --- |
| 3b206d9 / 92d7f76 / 14e7e4e + 4ce041a | T10 core, HTTP/UI and PG gate tests merged; test compile fix |
| 09587d8 | T10 real-PG gates **15 PASS / 0 FAIL** (`go test -run '^TestLiveClaims'`). Product bug fixed: the claim-window CHECK had a NULL hole; migration 0060 and the contract are updated. Three test bugs fixed |
| (same tree) | **KC16 browser gate PASS**: `bash scripts/dev/test-local.sh --browser-live-claims`, admin + storefront Next, Go, PG, Chromium, MOCK ingress |
| ea0bf59 | deploy/packaging-v1 merged |
| dc9086c | `cmd/migrate` added (deploy I1). Real PG: 2 runs, both exit 0, 55 migrations |
| d87a672 | `build-images.sh` optional `GO_IMAGE/NODE_IMAGE/RUNTIME_IMAGE`. Needed only behind a TLS-intercepting proxy: in the cloud container use `GO_IMAGE=lc-verify-golang:ca NODE_IMAGE=lc-verify-node:ca` (local images that add the proxy CA) |
| (independent integrator run) | `deploy/scripts/smoke.sh static` PASS. `smoke.sh full` **44 PASS / 0 FAIL / 1 BLOCKED**; the block is S29m = I8 (after a logical restore, `live.media_plan_ready()` changes t->f because constraint md5 fingerprints are re-parsed; this needs a media-migration fix owned by the media lane). Evidence: `deploy/.evidence/20260928T135453Z-13b6/` (gitignored, not pushed) |
| 7561bcc | KC03 upgrade PG memory raised to 1g |

GitGuardian flags `tests/foundation/live_claims_schema_test.go` (`url.UserPassword("postgres", password)`) in commits 14e7e4e and 8a3c27a. It is a **false positive**: the value is `hex(randomBytes(24))`, generated per run. The line is now `ggignore`. The owner must mark incidents 37686711 as false positive in the GitGuardian dashboard, because history still contains the older commits.

The CI result for 7561bcc was not awaited, to save budget. Check it first.

### Remaining next steps (updated)
1. Check CI on the branch head; fix anything red.
2. Independent security + correctness review of T10. The §0.1 P2 tests (a)–(g) exist in the gate tests (`TestLiveClaimsP2*`); the review should confirm the tests are not vacuous.
3. Stripe: freeze `contracts/stripe-psp-v1.md` (the adversarial review was interrupted), then implement MOCK + SANDBOX + browser 4242.
4. T10c Meta comment intake (amendment `meta-claims-intake-v1`), T12 E2E, fulfilment/refund record, T20, T21, T22, I8 media restore gate.

## Final state at pause (credits exhausted)

- Stripe stage A merged and independently verified (27c2f5b): adapter `internal/integrations/psp/stripe`,
  unit SP01–SP05/SP19 PASS, Node webhook vectors 43/43, real sandbox SP16 PASS (no charge).
  Owner answers + rulings in `contracts/stripe-psp-v1.md` §0.1 (per-store account, TWD/HKD/SGD/MYR/USD,
  40-min hold, card only, manual refund work after closure, Connect deferred).
- Stripe stage B1 (migration 0061, workers, webhook, MOCK SP06–SP15) was started and **stopped
  immediately — nothing usable**; restart it from the §0.1 staging rule.
- CI on the head was not awaited; check it first.
- Next order: CI green → Stripe B1 → B2 (buyer routes/UI + SP18 browser 4242) → T10c Meta intake →
  T12 E2E → fulfilment/refund record → T20/T21/T22 → I8 media restore gate.
