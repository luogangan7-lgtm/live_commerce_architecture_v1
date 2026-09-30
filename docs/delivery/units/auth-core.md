# Unit auth-core — 0070 password auth SQL, identity password service, private HTTP, api config

Role: commerce_worker (mid tier). Base = `00c1d94` + integrator commit F0 (see `auth-mail.md`), SHA
recorded at dispatch. Worktree `.worktrees/auth-core`, branch `unit/auth-core`. No delegation, no
network (HIBP and SMTP only through loopback fakes in tests), no real mailbox, no secret.
Parallel with `auth-mail`, `auth-ui`, `auth-tests` (disjoint paths). Contract:
`contracts/merchant-password-auth-v1.md` (v1 FROZEN 2026-09-30) incl. §13 dispositions and §14
rulings (X3: signup/reset send detached after the 202, login synchronous). The "Defaults adopted"
below bind unless the integrator overrides.

**Goal:** a merchant signs up, logs in and resets with email + password + emailed 6-digit code;
sessions come only from the definer that consumes a correct code; no enumeration, bounded cost.

## Read (by section; grep headings, `sed -n` ranges)
PROCESS.md; contract §0 (PD1–PD15 + rejected), §2, §3 (consumer interface, mail content paragraph),
§4.1–§4.4, §5, §6, §7.1, §8, §9 PA01 + PA03–PA09 (what the tests will assert), §11. By symbol:
`migrations/0004_merchant_identity.sql` (`issue_merchant_session`, `revoke_merchant_session`, grants
lines 40–60, lock order), `0001_foundation.sql` (`identity.principals`, `memberships`),
`0065_owner_provisioning.sql` (`create_initial_store` body — unchanged);
`internal/identity/service.go` (`New`, `Policy`, `Session`, `randomToken`, `digest`, `translateError`,
`transaction`), `internal/identityhttp/handler.go` (`service`, `NewHandler`, `body`, `respond`,
`failure`, BFF-key check), `cmd/api/identity.go` (`loadIdentityConfig`, `flag`, `buildIdentityHandler`),
`cmd/api/main.go:71-110` (read only), `internal/platform/platform.go` (`OpenIdentityPool`),
`deploy/secrets.manifest.tsv` header (kinds).

## Defaults adopted (P2s the frozen text left open)
- A1 Binding = 32 random bytes, base64url-unpadded (43 chars) to the BFF; SQL gets `sha256(binding)`
  as `binding_hash`. Code HMAC = `HMAC-SHA256(pepper, binding_hash ‖ ascii(code))`. Session token =
  existing `randomToken()`; SQL `p_session = digest(token)`.
- A2 Throttle bucket key = `HMAC-SHA256(pepper, kind + ":" + value)` where value is the masked prefix
  text (`netip.Prefix.Masked().String()`, IPv4 /32, IPv6 /64, /48 for `ip48-*`; IPv4-mapped IPv6 is
  `Unmap`ped first), the normalized email, `email + "‖" + ipPrefix`, or the binding. Challenge
  `ip_hmac` and audit `ip_hmac` = the `ip:` bucket key; audit `email_hmac` = key of `email:<email>`.
- A3 Buckets are hit in the §6 table order for the route (§2 lists which); the first over-limit
  bucket stops the chain (later buckets are not consumed); a bucket with several windows
  (`email-mail-unauth`, `email-mail-login`) is one hit per window. `Retry-After` = ceil seconds to
  the window end.
- A4 Dummy PHC = `HashPassword` of 32 random bytes, computed once in `NewPasswords`; unknown email
  verifies against it (Argon2 always runs, PD6/PA05).
- A5 PD3 limiter: one process-wide `chan struct{}` of 4; acquire waits ≤ 2 s else `ErrBusy`; Argon2
  hash, Argon2 verify and every HIBP call acquire it. PD7 background sender: separate non-blocking
  8-slot semaphore; full ⇒ `record_challenge_mail(id,'FAILED')` without dialing.
- A6 Login send context = 10 s (so the request ends inside `http.Server.WriteTimeout` 15 s and the
  BFF's 14 s login timeout, auth-ui U3); background sends 25 s, detached (`context.WithoutCancel`).
- A7 HIBP: `GET {base}/range/{first5 SHA-1 hex upper}` with `Add-Padding: true`, 2 s timeout, stdlib
  `net/http`; suffix match case-insensitive, count > 0 ⇒ `breached`; any error/timeout/non-200 ⇒
  accept + audit `password.breach_check_unavailable` (fail-open, Q3). `BreachCheck="off"` only when
  `AllowLoopback`.
- A8 Without OIDC config (ruling R-4): `identity.New` accepts a nil `Provider` iff
  `Policy.PasswordLogin`; then `Start`/`Complete` return `ErrDisabled` (existing mapping 403) and
  `COMMERCE_IDENTITY_PROVIDER_KEY` is optional. With OIDC configured nothing changes.
- A9 `record_challenge_mail` outcome mapping: `nil` ⇒ SENT (reply stored), `errors.Is(err,
  mail.ErrUnknown)` ⇒ UNKNOWN, any other error ⇒ FAILED. Login: FAILED ⇒ 503 `mail_unavailable`,
  UNKNOWN ⇒ 202.
- A10 Private HTTP error body = existing `httperror.Write(w, status, code)`; `password_policy` 422
  carries `reason` (`too_short|too_long|breached|equals_email`) — the only extra key; 422 invalid email
  code = `invalid_email`; 429 code `throttled` + `Retry-After`; 503 codes `busy`/`mail_unavailable`.
- A11 Purge in `auth_throttle_hit` (≤ 100 rows each) also NULLs `pending_password_hash` of expired
  challenges it deletes (§4.1 last bullet) — deletion covers it; consume sets it NULL explicitly.
- A12 `Passwords.Close(ctx)` stops accepting background sends and waits for in-flight ones until
  `ctx` ends; `buildIdentityHandler`'s close func calls it with a 25 s context **before**
  `pool.Close()` (graceful shutdown per PD7).

## FROZEN Go interface (auth-ui's BFF, auth-tests and the integrator rely on exactly these)
```go
package identity // internal/identity — new files password.go, challenge.go, throttle.go, hibp.go, sender.go, mailcopy.go
type Mailer interface{ Send(ctx context.Context, m mail.Message) (reply string, err error) } // *mail.SMTP satisfies it
type PasswordPolicy struct {
    Pepper        []byte        // 32 bytes (COMMERCE_AUTH_PEPPER_FILE)
    SessionTTL    time.Duration // 5 min–24 h (existing COMMERCE_SESSION_TTL)
    MailDailyCap  int           // 20–100000, default 200 (§6 shares 40/15/45 %)
    BreachCheck   string        // "hibp" | "off" (off only with AllowLoopback)
    HIBPBaseURL   string        // "" = https://api.pwnedpasswords.com; override only with AllowLoopback
    AllowLoopback bool
}
type Passwords struct{ /* unexported */ }
func NewPasswords(pool *pgxpool.Pool, mailer Mailer, p PasswordPolicy) (*Passwords, error)
type Challenge struct{ Binding string `json:"binding"`; ExpiresAt time.Time `json:"expires_at"` }
func (p *Passwords) Signup(ctx context.Context, ip netip.Addr, email, password, locale string) (Challenge, error)
func (p *Passwords) Login(ctx context.Context, ip netip.Addr, email, password, locale string) (Challenge, error)
func (p *Passwords) Reset(ctx context.Context, ip netip.Addr, email, locale string) (Challenge, error)
func (p *Passwords) Complete(ctx context.Context, ip netip.Addr, binding, purpose, code, newPassword string) (Session, error) // existing Session type
func (p *Passwords) Close(ctx context.Context) error // A12
var ErrInvalidEmail, ErrInvalidCredentials, ErrInvalidCode, ErrAccountExists, ErrMailUnavailable, ErrBusy error
type PolicyError struct{ Reason string } // too_short|too_long|breached|equals_email; Error() never contains the password
type ThrottleError struct{ RetryAfter time.Duration }
// Pure helpers (PA01 UNIT):
func HashPassword(password string) (string, error)       // PD1 PHC string, under the PD3 limiter
func VerifyPassword(phc, password string) (bool, error)  // rejects non-argon2id/bad params; subtle.ConstantTimeCompare
func NormalizeEmail(raw string) (string, error)          // trim, ASCII-only, lower, one '@', ≤ 254 → ErrInvalidEmail
func CheckPasswordPolicy(password, email string) error   // NFC, 12–128 code points; email=="" skips equals_email (reset)
func NewCode() (string, error)                            // 6 digits, crypto/rand rejection sampling
func CodeHMAC(pepper, bindingHash []byte, code string) []byte
func ArgonCount() int64                                   // process-wide IDKey call counter (PA01/PA05 dummy-path proof)
// Policy gains PasswordLogin bool (A8).

package identityhttp // internal/identityhttp — new file password.go
type passwordService interface{ Signup(…); Login(…); Reset(…); Complete(…) } // same signatures as *identity.Passwords
func NewPasswordHandler(p passwordService, bffKey string) (http.Handler, error) // serves POST /v1/identity/password/{signup,login,reset,complete}; §7.1
func ClientIP(r *http.Request) (netip.Addr, error)                               // exactly one valid X-Commerce-Client-IP, read only after the BFF-key check (R-5)
```
Private wire (auth-ui codes against this): request/response bodies, status codes and error codes of
§7.1 exactly, plus A10. Unchanged: `identity.New` signature (A8 only relaxes validation),
`identityhttp.NewHandler`, all 0004/0065 functions, `/v1/identity/{login,logout,initial-store}`.

## Build
1. **SQL** `migrations/0070_merchant_password_auth.sql`: §4.1 tables (owned by the migration role —
   no `ALTER TABLE … OWNER`), §4.2 definers (owner `commerce_identity_writer`, `SET search_path =
   pg_catalog`, `REVOKE ALL … FROM PUBLIC`, `GRANT EXECUTE … TO commerce_identity`), §4.3 exactly
   (lock order email advisory → principal → credential → challenge → sessions), §4.4 grants, no RLS,
   `COMMENT ON` every table, column, function (owning package `internal/identity`, allowed role,
   non-goals). Fault seam for PA04: none added — tests inject faults via owner-pool triggers.
2. **Service** `internal/identity`: §2 flows + PD1–PD15 + A1–A12. Every rate-limit check before
   hashing, HIBP, mail or existence lookup (§6 heading). Signup/reset: write result, then detached
   send (X3); login: synchronous send (A6). Each no-retry branch comments why (PD7/I06).
   `mailcopy.go`: zh-CN/zh-TW/en subject (no code) + text/HTML per §3; no links/images.
3. **HTTP** `internal/identityhttp/password.go`: reuse `body` (strict JSON ≤ 64 KiB), BFF-key check,
   no Origin/Cookie, no query, `Cache-Control: no-store`; code not 6 ASCII digits ⇒ 401 `invalid_code`
   before any work; for reset `complete` the `binding` and `ip` throttles pass before HIBP/Argon2 (§7.1).
4. **Config** `cmd/api/identity.go`: §5 variables under `COMMERCE_IDENTITY_ENABLED=1` +
   `COMMERCE_PASSWORD_LOGIN_ENABLED`; `*_FILE` secrets read once, never logged; SMTP host DNS name
   (loopback only with the loopback flag); build `mail.NewSMTP` (after auth-mail M1) and
   `identity.NewPasswords`; inside `buildIdentityHandler` mount `/v1/identity/password/` →
   `NewPasswordHandler`, `/v1/identity/` → existing handler (so `main.go` is unchanged); close func =
   A12 then `pool.Close`. Invalid config fails startup (table-driven test in `identity_test.go`).
5. **Gate PA01** `internal/identity/password_pa01_test.go` `TestPasswordPA01Crypto` (contract §10 gives
   it to this unit; red = mutate `VerifyPassword` to `bytes.Equal` or the code sampler to `% 10^6`
   without rejection, then green). Other unit tests: names must not start with `TestPasswordPA0[3-9]`,
   `TestPasswordPA1`, `TestMailPA` (auth-tests' gates).

PROCESS §5: package comments of `identity`/`identityhttp` gain the password responsibility, "It never
…" (links OIDC and password principals; retries mail; persists a plaintext code) and the external host
`api.pwnedpasswords.com` (k-anonymity, why). Call sites into 0004 functions/other schemas carry the
one-line table/role/why comment; SQL gets `COMMENT ON`; the HIBP URL and `Add-Padding` carry the docs
URL (F6) + retrieval date; Argon2 params cite F1/F2.

## Write paths
`migrations/0070_merchant_password_auth.sql`,
`internal/identity/{service.go,password.go,challenge.go,throttle.go,hibp.go,sender.go,mailcopy.go}` +
their `*_test.go` incl. `password_pa01_test.go`,
`internal/identityhttp/{password.go,password_test.go}` (`handler.go` only for a shared helper export if
unavoidable — list it), `cmd/api/{identity.go,identity_test.go}`, `output/auth-core/**`.
Forbidden: `internal/mail/**`, `cmd/api/main.go`, other migrations, `deploy/**`, `apps/**`,
`tests/**`, contracts, `core-openapi.json`, go.mod/go.sum (F0 has x/crypto; if F0 is not yet in the
base, `go get golang.org/x/crypto@v0.57.0` locally and leave go.mod/go.sum out of the commit).

## Verify
```sh
GOTOOLCHAIN=go1.27.1 go vet ./... && gofmt -l internal cmd
GOTOOLCHAIN=go1.27.1 go test -race -count=1 ./internal/identity/... ./internal/identityhttp ./cmd/api
LC_FOCUSED_TIMEOUT=1200s bash scripts/dev/test-focused.sh '^Test(Identity|BrowserSessionStoreList|PrivateIdentityHTTP|AccountOnboarding|Pool)'   # regression: 0004/0065/OIDC unchanged
python3 scripts/check_packet.py
```
Logs → `/Volumes/data/live_commerce_architecture_v1/output/auth-core/` (command, SHA, exit, counts).
PA03–PA11 are auth-tests' gates; do not claim them.

## Integrator hooks (only the integrator edits these; the unit exposes the functions above)
- F0: go.mod/go.sum `golang.org/x/crypto v0.57.0`, `docs/engineering/dependencies.md` line (what:
  argon2id; why: PD1/PD2; importer `internal/identity`; rejected: pbkdf2/bcrypt/scrypt), frozen
  `internal/mail/message.go`.
- Merge `0070` (migration numbers are integrator-merged); regenerate `docs/engineering/dependency-map.md`
  (`scripts/dev/depmap.sh`).
- `deploy/secrets.manifest.tsv`: `commerce_auth_pepper b64url32 gen api <rotation: invalidates open
  challenges + throttle windows only>`, `commerce_smtp_password opaque owner api <rotation per §5>`;
  `deploy/compose.yml` api: `secrets:` both + `COMMERCE_AUTH_PEPPER_FILE`/`COMMERCE_SMTP_PASSWORD_FILE`,
  env `COMMERCE_PASSWORD_LOGIN_ENABLED: ${LC_PASSWORD_LOGIN_ENABLED:-0}`, `COMMERCE_SMTP_HOST`,
  `COMMERCE_SMTP_USERNAME`, `COMMERCE_MAIL_FROM`, `COMMERCE_MAIL_DAILY_CAP`, `COMMERCE_BREACH_CHECK`;
  admin env `COMMERCE_PASSWORD_LOGIN_ENABLED`; api `stop_grace_period: 20s` → `40s` (10 s HTTP drain +
  25 s mail drain, A12); `deploy/env/*.example` lines; `secrets-init.sh` generates the pepper only.
- `deploy/scripts/preflight.sh`: P08/P15 OIDC rules apply only when `LC_OIDC_ISSUER` is set (R-4); new
  PA15 rule (number allocated by the integrator across R2) — `dig +short $LC_ADMIN_HOST` resolves only
  to this host and to no Cloudflare range while `LC_PASSWORD_LOGIN_ENABLED=1`; Caddyfile unchanged
  (F9 already verified; any `trusted_proxies` change needs a proxy review, Q6).
- `docs/runbooks/mail.md` (PA13, owner-run) and `docs/runbooks/merchant-onboarding.md` sections with
  explicit anchors (contract §10/§11), both "owner approval of this specific incident in chat first":
  - `<a id="unlock-password"></a>`: identify the principal via `lc_psql` read-only
    (`SELECT principal_id, failed_count, disabled_at FROM identity.password_credentials WHERE email =
    lower(:'email')`, email typed at the prompt, never logged); then one transaction as the migration
    owner: `UPDATE identity.password_credentials SET disabled_at=NULL, failed_count=0 WHERE
    principal_id=$1 AND disabled_at IS NOT NULL` (expect 1 row, else ROLLBACK) + `INSERT INTO
    identity.auth_events(principal_id, action) VALUES ($1,'operator.unlocked')`; verify; note that
    unlock grants no access and cannot help if disabling continues (first incident ⇒ CAPTCHA contract).
  - `<a id="deactivate-principal"></a>`: trigger = `throttled` audit with bucket kind
    `global-mail-login`; list offending principals (store-holding, recent sign-ups); one transaction:
    `UPDATE identity.principals SET active=false WHERE id=ANY($1)` + revoke their unrevoked merchant
    sessions with `session.revoked` rows in `identity.session_events` (0004 pattern). The 0070 action
    CHECK has no deactivation action, so the audit is the `ops-admin.log` entry + session events
    (open question 1 below). Reactivation = same SQL with `true`, owner-approved.
  Also update §0 O2 / §2 step 1 of that runbook: OIDC is optional when password login is on.
- `contracts/tasks.json` evidence rows; `test-local.sh` mode review (auth-tests adds it).
`cmd/api/main.go`, OpenAPI, apps/admin nav: **no change needed** (identity routes mount inside
`buildIdentityHandler`; `contracts/core-openapi.json` has no `/v1/identity` path today — verified by grep).

## Order / Non-goals / Return
Dispatch after F0, parallel with auth-mail/auth-ui/auth-tests. Steps 1–3 + PA01 need no auth-mail;
step 4 compiles after auth-mail merges (M1) — rebase, then hand to the integrator (M2). auth-tests'
PG gates run after M2. No TOTP/passkeys, no email change, no staff invite, no CAPTCHA, no account
linking, no rehash-on-login, no bounce handling, no River job, no real mail.
Return commit SHA, model/reasoning, base, paths, commands + exit codes + counts, evidence, how A1–A12
were handled, risks, NOT_RUN. Any deviation from the frozen signatures = stop and escalate.

Open question for the integrator (default ships): (1) deactivation audit lives in `ops-admin.log` +
`session.revoked` events; adding `operator.deactivated` to the `auth_events` CHECK needs a contract
amendment — default: no amendment in v1.
