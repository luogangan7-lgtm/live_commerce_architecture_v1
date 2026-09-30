# Unit stripe-live-tests — independent gates SL02–SL06, SL08, SL09 and legal-pages LG01

Role: test_worker, independent (mid tier). Write from the contract and the FROZEN blocks of
`stripe-live-core.md`, `stripe-live-ops.md`, `legal-pages.md` only; do not read or copy those units'
branches. Base `00c1d94`. Worktree `.worktrees/stripe-live-tests`, branch `unit/stripe-live-tests`.
Author at dispatch against frozen signatures; compile/run after the integrator merges the units (F1/F2).
No LIVE, no live key, no production DSN; SANDBOX only with the owner's test keys (SL08).

## Read (by section)
PROCESS.md; contract §0 (LD1–LD8), §3 (all), §4, §5.1–§5.4, §6, §7, §8, §9, §11, §13; the three FROZEN
blocks + "Defaults adopted" (S1–S9, O1–O5, P1–P5) = the tested behaviour where the contract was silent.
Reuse by symbol: foundation fixtures (`fixture(t)`, `mustExec`, owner/registrar pools,
`stripeRegistrarSetup`, `stripeExpectCheck`, `stripeMustCatalog`, `newT06GoFixture` in
`tests/foundation/stripe_schema_test.go`, `stripe_registrar_test.go`), SP08 capture flow + SP13 webhook
harness (`stripe_flow_test.go`, `stripe_webhook_http_test.go`), RF refund fixtures
(`stripe_refund_{request,flow,env}_test.go`), process harness `stripe_process_test.go`, `stripetest`
(`New`, `Transport`, `SignWebhook`).

## Tier rules
REAL_PG/HTTP_PG = PG 18 via `test-focused.sh`. LIVE rows (accounts, endpoints, attempts, facts, receipts)
cannot come from a real LIVE flow: build them through the real definers where a definer exists and via
the owner pool only for rows no test path can create (a LIVE `merchant_accounts` credential, a LIVE
CAPTURED fact); disclose every owner-pool insert per test in the evidence file. Fake keys are split
literals (`"rk_" + "live_" + …`), never key-shaped. SANDBOX: `t.Skip("NOT_RUN: …")` unless `STRIPE_SANDBOX=1`.

## Gates (top-level names exact; file prefixes avoid helper collisions)
- **SL02** `TestStripeSL02Schema` (`stripe_live_schema_test.go`, `sls`): §11 SL02 list; 0077 + post_river/0016
  on fresh and populated-latest DB, repeat/checksum; SP19 fixture still rejected; revoke-only trigger on
  Stripe LIVE, SANDBOX and PAYUNi rows (`id` change, `revoked_at`→NULL, other column; supplied
  `revoked_at` replaced by `clock_timestamp()`); approval immutability + set-once triples; one active per
  connection; all FKs; `stripe_min_minor`↔`stripe_amount_ok` per currency; endpoint env/profile CHECK;
  refund env widened; FORCE RLS; column-privilege matrix from `information_schema`; definer
  owner/`proconfig`/ACL; PUBLIC revoked; no runtime/worker/ingress privilege on approvals; S5 definer
  owner `commerce_auth`, EXECUTE `commerce_runtime` only.
- **SL03** `TestStripeSL03Registrar` (`stripe_live_registrar_test.go`, `slr`): every §11 SL03 clause via the
  real registrar login calling the SQL definers (readiness jsonb supplied by the test; `LiveApprove`'s
  Stripe read is covered by SL01/SL08, and no mock transport ever admits a live key), plus
  `stripeadmin.Open(…).LiveRevoke` / `SetMethod(Enabled=false)` without the pair (S8). Concurrent qualify
  + revoke with two sessions and a `pg_blocking_pids` witness; `enabled=false` after revoke, expiry, rotate.
- **SL04** `TestStripeSL04LiveShape` (`stripe_live_shape_test.go`, `slp`, REAL_PG + HTTP) plus Go env rules
  in `internal/payments/stripe_live_sl04_test.go` (package-internal, name `TestStripeSL04LiveShape`):
  observation/apply definers `Livemode` exact per environment; `validStripeSnapshot`/
  `stripeSessionIdentity`/refund loader; merchant refund on a SANDBOX attempt through a LIVE-environment
  handler (and the reverse) → `not_refundable`, zero refund rows (S5).
- **SL05** `TestStripeSL05LiveIngress` (`stripe_live_ingress_test.go`, `sli`, HTTP_PG): §11 SL05 with
  `stripewebhook.NewInbox(…,"LIVE")`; SP13 log sentinels.
- **SL06** `TestStripeSL06LiveProcess` (`stripe_live_process_test.go`, `slx`): process half (worker/api refuse
  LIVE without the pair; `LC_STRIPE_CHECKOUT_ENABLED=0` removes Stripe from hosted while a MOCK in-flight
  attempt still closes; API never reads `STRIPE_SECRET_KEY`) and shell half against `deploy/scripts/*`
  with a stub `docker` on `PATH` recording argv (`-e` NAMES only): every §11 SL06 shell clause + O1–O3.
  The sentinel value never appears in stdout/stderr/stub log.
- **SL08** `TestStripeSL08RestrictedKey` (`stripe_live_rak_test.go`, SANDBOX): §11 SL08 with the owner's
  `STRIPE_RESTRICTED_TEST_KEY` from `~/.config/livecommerce/secrets.env` (default name; `rk_test_` only);
  writes `output/stripe-live/rak-permissions.txt` (permission names + which readiness fields were
  present, booleans only); SKIP = NOT_RUN.
- **SL09** `TestStripeSL09Watchdog` (`stripe_live_watchdog_test.go`, `slw`): each W11 signal red on a seeded
  fixture, green when cleared; output counts/ids only (no PII, no amounts).
- **LG01** `tests/storefront/legal-pages.mjs` (BROWSER, Playwright, spawns `next start` itself on a free
  port; no backend): every slug of `legal-pages.md` × 3 locales → 200 + one `h1` + footer with all 6 links;
  unknown slug → 404; `/{locale}/data-deletion` → 200 once customers-billing-ui has merged (else recorded
  NOT_RUN, never PASS); no third-party request; desktop + 390px no horizontal scroll; reports the count of
  `[data-owner-text]` markers; `LC_LEGAL_REQUIRE_FINAL=1` fails while any marker remains (the §9
  `policy_pages` attestation check). Screenshots hashed into the evidence.

## Write paths
`tests/foundation/stripe_live_*_test.go`, `internal/payments/stripe_live_sl04_test.go`,
`tests/storefront/legal-pages.mjs`, `output/stripe-live-tests/**`.
Nothing else (not the cores' unit tests, not `apps/**`, not `deploy/**`, not contracts).

## Verify
```sh
GOTOOLCHAIN=go1.27.1 go vet ./tests/foundation ./internal/payments/...
LC_FOCUSED_TIMEOUT=2400s bash scripts/dev/test-focused.sh '^TestStripeSL0[2-69]'
STRIPE_SANDBOX=1 bash scripts/dev/test-focused.sh '^TestStripeSL08'     # owner test RAK only; else NOT_RUN
pnpm build:storefront && node tests/storefront/legal-pages.mjs
```
Red proof per gate (PROCESS §2.4): after merge, one targeted mutation of the merged candidate in a scratch
copy (reverted) per gate → `output/stripe-live-tests/red-<gate>.log`, then the green log. Suggested
mutations: SL02 drop the revoke-only trigger; SL03 remove the canary cap branch; SL04 revert the S5 guard;
SL05 admit SANDBOX endpoint on LIVE; SL06 echo the `_FILE` value; SL09 invert one W11 threshold; LG01
drop a slug. Compile failure is not a red run; zero matched tests or SKIP is never PASS.

## Integrator hooks
- Add to `scripts/dev/test-focused.sh` and `test-local.sh` (contract §11 harness guard):
  `if env | grep -qE '=(sk|rk)_live_'; then echo 'refused: live key in test environment' >&2; exit 2; fi`
  (no value printed). The contract's `test-local.sh --stripe/--stripe-sandbox` modes do not exist at
  `00c1d94`; `test-focused.sh '^TestStripe(SP|SL)'` is the command.
- SL07 regression (SP01–SP21, RF01–RF12, SP16/RF10/SP21 SANDBOX, SP18 browser) is the integrator's run.

## Order / Return
Author at dispatch; SL02/SL03 after F1; the rest after F2; LG01 after `legal-pages` merges + the layout hook.
Return files; per gate the assertion list mapped to contract/default text; red + green logs with command,
SHA, exit, PASS/FAIL/SKIP; every owner-pool insert disclosed; NOT_RUN (SL08 without the RAK; SL-LIVE01/02
owner); every clause found untestable or ambiguous.
