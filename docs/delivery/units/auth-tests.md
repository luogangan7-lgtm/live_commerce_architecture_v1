# Unit auth-tests — independent gates PA03–PA11 (+ PA12 probe test file)

Role: test_worker, independent. Write from the contract and the FROZEN blocks + defaults of
`auth-mail.md` (M1–M7), `auth-core.md` (A1–A12) and `auth-ui.md` (U1–U7) only; do not read or copy
those units' branches. Worktree `.worktrees/auth-tests`, branch `unit/auth-tests`. Base = `00c1d94` +
F0. Author against frozen signatures at dispatch; compile/run after M2 (auth-mail + auth-core merged).
No real mailbox, no owner secret, no real email in any run; HIBP only through a loopback fake.

## Read (by section)
PROCESS.md; `contracts/merchant-password-auth-v1.md` §0 (PD3–PD14), §2, §3 classification, §4.1–§4.4,
§6 (whole table), §7, §8, §9 (every PA row), §11, §13 R2-1..R2-3/R3-1..R3-3 (the regressions these
gates exist for). Reuse by symbol: foundation fixtures (`fixture(t)`, `mustExec`, owner/runtime
pools, `sqlState`; grep `tests/foundation/{identity_integration,identity_independent,
browser_identity_integration,account_onboarding}_test.go`, `identityFixture`, `firstStoreRequest`),
browser harness `tests/foundation/browser_identity_chain_test.go` (`runBrowserIdentityChain`,
`browserEnvironment`, `browserLog`) + `scripts/dev/test-local.sh` modes, `mailtest` (frozen in auth-mail).

## Tier rules
REAL_PG/HTTP_PG = PG 18 via `test-focused.sh`; the service is built in-process with
`platform.OpenIdentityPool` on the fixture's identity login + `identity.NewPasswords(pool,
mailtest-backed *mail.SMTP, policy{AllowLoopback:true, HIBPBaseURL: fake})`; HTTP through
`identityhttp.NewPasswordHandler`. MOCK = the real SMTP adapter against `mailtest` (never a stub
Mailer). Aged timestamps / window boundaries / fault triggers via the owner pool only, disclosed per
test in evidence. Memberships for `has_membership` come from the real `/v1/identity/initial-store`
path (0065), never inserted rows. Codes are read from the `mailtest` mailbox, never from SQL.
Canary scans: unique sentinel email/password/code/SMTP secret per test, searched in captured logs,
errors, response bodies and every text column of `identity.*`.

## Gates (top-level names exact; file prefixes avoid helper collisions — helpers prefixed `pwa`)
- **PA03** `TestPasswordPA03Schema` (`password_schema_test.go`): every §9 PA03 clause; table owners ≠
  writer; column-level UPDATE denial (`code_hmac`, `auth_events.action`); TRUNCATE denied; definer
  owner/`proconfig`/ACL/PUBLIC revoked for all eight §4.2 functions; runtime/auth roles denied on the
  four tables; upgrade on a DB populated through 0066; `OpenIdentityPool` still admits the identity login.
- **PA04** `TestPasswordPA04Signup` (`password_flow_test.go`): §9 row; fault = owner-pool trigger
  raising after the credential insert ⇒ zero principal/credential/session/event rows; concurrent
  sign-ups two-tx witness (`pg_blocking_pids`).
- **PA05** `TestPasswordPA05Login` (`password_flow_test.go`): §9 row incl. 101st/150th wrong password
  on a disabled credential ⇒ identical 401 to unknown email, `failed_count` stays 100,
  `identity.ArgonCount()` increments on the disabled and unknown paths (A4); OIDC principal with the
  same email untouched (PD9).
- **PA06** `TestPasswordPA06Reset` (`password_reset_test.go`): §9 row incl. PD5 per-bucket (10) and
  per-principal (50) budgets, over-budget leaves `attempts` unchanged, concurrent completes two-tx
  witness, PD4 same-source supersede / cross-source keep / 4th-open supersedes oldest.
- **PA07** `TestPasswordPA07Throttle` (`password_throttle_test.go`): every §6 limit at N and N+1
  (incl. UTC+8 day alignment, A3 stop-at-first order), purge ≤ 100 per call, and each bold §9 PA07
  scenario as its own subtest (victim login after attacker resets; `email-pw` per source;
  `global-mail-unauth` isolation; disabled credential + attacker at every limit from the UTC+8 day
  boundary ⇒ victim reset from source B; one source leaves ≥ 1 `global-mail-unauth` hit, 11th unauth
  mail ⇒ 429; `global-mail-login` 503 consumes one `email-mail-login` hit; ≤ 5 login mails/24 h;
  store-less accounts exhausting `global-mail-login-new` leave a member's login working).
- **PA08** `TestPasswordPA08HTTP` (`password_http_test.go`): transport rules (BFF key, Origin/Cookie
  rejected, query, size, strict keys), client IP missing/duplicate/invalid ⇒ 400, the §9 identical-
  response comparisons (status, header names, JSON key set + types; `binding`/`expires_at` excluded),
  A10 error shapes, canary log scan.
- **PA08b** `TestPasswordPA08bTiming` (`password_timing_test.go`): `mailtest.SetDelay(400ms)`, n=50
  per arm, median difference < 50 ms for the three pairs, login ≥ 400 ms. Record raw latencies in
  evidence; a timing flake is re-run once and both runs are reported (never silently retried).
- **PA09** `TestPasswordPA09MockMail` (`password_mail_test.go`): §9 row through the real adapter;
  `DataCount()` == challenges; FAILED on login ⇒ 503; FaultDropAfterDot ⇒ UNKNOWN ⇒ 202 + resend
  supersedes (old binding `invalid_code`); background semaphore full ⇒ `mail_state='FAILED'`, response
  unchanged; per-locale content, no URL, no code in subject.
- **PA10** `tests/admin/password-bff.test.ts` (Node, imports only the auth-ui FROZEN TS): Origin/strict
  keys via parsers, cookie flags + Max-Age ≤ 600, `clearsChallenge` 200/409 only, XFF missing ⇒ 503 /
  multiple or invalid ⇒ 400, `readAuthConfig` valid without OIDC when the password flag is on, invalid
  without it; route-level "password never in response/logs" is asserted in PA11's harness.
- **PA11** `tests/admin/password-auth.spec.ts`, driven by `tests/foundation/browser_password_auth_test.go`
  (`//go:build browser`, `TestBrowserPasswordAuth`): in-process api + `mailtest` + an inspection
  endpoint that exists only in this test binary on loopback; §9 PA11 flow (sign-up → code →
  onboarding → logout → login → code → workspace; forgot → reset → signed in, old session revoked;
  wrong code, throttled, resend cooldown), 3 locales, desktop + 390px Chromium, autocomplete
  attributes, screenshots hashed; plus BFF-level checks: cookie cleared on 200/409 only, no password/
  code in admin server logs (canary). New mode `test-local.sh --browser-password-auth`.
- **PA12** `tests/foundation/mail_probe_test.go` `TestMailPA12SMTPProbe` (LIVE read-only, owner-run):
  `t.Skip("NOT_RUN: …")` unless `LC_MAIL_PROBE=1`; reads host/username/secret file paths from env,
  calls `(*mail.SMTP).Probe` twice (username; a different From ⇒ record the reply code); never DATA.
  Authored here, run only by the owner/integrator on the server (not CI).
PA01/PA02 belong to auth-core/auth-mail (contract §10); PA13/PA15 are integrator/owner LIVE; PA14 is
security_reviewer. Report here any PA01/PA02 clause you find unasserted in the merged code.

## Fakes
HIBP fake: `httptest` server in the test files serving `/range/{prefix}` with `Add-Padding`-shaped
rows, a breached-password table, 2.5 s delay mode (timeout ⇒ fail-open + audit), 500 mode. Written
from F6 docs, not from the implementation.

PROCESS §5: each new test file starts with a comment naming the gate, the contract § it proves and
the tables/functions/endpoints it touches; SQL faults via the owner pool carry a one-line why; the HIBP
fake cites F6 (URL + retrieval date). No new Go module or npm package.

## Write paths
`tests/foundation/{password_*,mail_probe,browser_password_auth}_test.go`,
`tests/admin/{password-bff.test.ts,password-auth.spec.ts}`, `scripts/dev/test-local.sh` (new mode
only; integrator reviews), `output/auth-tests/**`. Nothing else (not `internal/**`, not `apps/**`, not
contracts, not the cores' unit tests; a needed `mailtest` capability not in the frozen block ⇒ escalate).

## Verify
```sh
GOTOOLCHAIN=go1.27.1 go vet ./tests/foundation && GOTOOLCHAIN=go1.27.1 go vet -tags browser ./tests/foundation
node --test --experimental-strip-types tests/admin/password-bff.test.ts
LC_FOCUSED_TIMEOUT=2400s bash scripts/dev/test-focused.sh '^TestPasswordPA(0[3-9]|08b)'
bash scripts/dev/test-local.sh --browser-password-auth                    # PA11
LC_MAIL_PROBE=1 GOTOOLCHAIN=go1.27.1 go test -count=1 -run '^TestMailPA12SMTPProbe$' -v ./tests/foundation  # owner server only; else NOT_RUN
```
Red proof per gate (PROCESS §2.4): after M2, one targeted mutation of the merged candidate in a
scratch copy (reverted) per gate → `output/auth-tests/red-<gate>.log`, then the green log (e.g. PA05:
drop `LEAST(…,100)`; PA07: key `email-mail-unauth` by email only; PA08b: await SMTP on reset; PA06:
supersede across sources; PA10: clear cookie on 401). Compile failure is not a red run; zero matched
tests or SKIP is never PASS.

## Order / Return
Author at dispatch (writer slot permitting); runs need M2; PA10/PA11 need auth-ui merged.
Return files; per gate the assertion list mapped to contract §/default text; red + green logs with
command, SHA, exit code, PASS/FAIL/SKIP counts, all under
`/Volumes/data/live_commerce_architecture_v1/output/auth-tests/`; NOT_RUN (PA12/PA13/PA15 LIVE);
every clause found untestable or ambiguous in the contract or the frozen briefs.
