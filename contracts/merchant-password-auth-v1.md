# Merchant password auth v1 — email + password, emailed one-time code

Status: **v1 FROZEN 2026-09-30 (DESIGN; gates NOT_RUN)** — round-1 review `output/contract-review/r2-design-wave.json`
key `auth` and the three round-2 P1s addressed in §13; round-3 findings (`output/contract-review/r2-round3.json`
key `auth`) applied per ruling X1 (§13 Round 3), frozen per ruling X6. Evidence label for this file: DESIGN.
Every PA gate below is NOT_RUN. Nothing here authorizes sending email to a real person; the only
real sends are the owner-approved PA12/PA13 (§9).

Owner decision O-A (2026-09-29, `docs/delivery/units/r2-design-rulings.md`): merchant login =
email + password, with an emailed one-time code as the security step — at sign-up (verify email),
at every login (second step) and at password reset. This replaces OIDC as the **primary** path.
OIDC (`internal/oidclogin`, `/api/auth/login` + `/api/auth/callback`) stays as an optional second
path. Owner decision O-B: outbound mail = generic SMTP adapter (Go stdlib `net/smtp`, implicit TLS
465, no new dependency); first configuration = a QQ mailbox; Tencent Exmail (`@xgdwm.com`) is the
recommended upgrade; Resend is not used.

Extends [merchant-identity-v1](merchant-identity-v1.md) and
[merchant-browser-auth-v1](merchant-browser-auth-v1.md). Reused unchanged: `identity.principals`,
`identity.sessions` (audience `merchant`), `identity.session_events`, `identity.resolve_access`,
`identity.list_session_stores`, `identity.create_initial_store` (0065 body), the admin BFF session
and CSRF cookies, `/api/auth/logout`, `/v1/identity/logout`, `/v1/identity/initial-store`.
Supersedes one sentence of merchant-identity-v1 ("do not write … a password store") and the
"Email … never create … merchant identity" line **for password principals only**: a password
principal is created by a verified email; OIDC principals are still mapped only by exact
`(issuer, subject)`, and the two are **never merged by email** (§0 PD9).
Invariants I01, I02, I04, I06, I11, I16, I18, I23 are authoritative; where they conflict, this file
is wrong.

## 0. Owner inputs, decisions and rulings

Owner inputs recorded: O-A, O-B, O-D (production operated by Claude on the owner's server; the
owner supplies secrets as files, never through chat); domain `xgdwm.com` on Cloudflare DNS.

| # | Decision | Why / risk closed |
| --- | --- | --- |
| PD1 | **Password hash = Argon2id** via `golang.org/x/crypto/argon2.IDKey`, params m=19456 KiB, t=2, p=1, 16-byte salt from `crypto/rand`, 32-byte key, stored as a PHC string `$argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>` (unpadded std base64). Params are code constants, not config. No pepper on passwords (rotation would lock everyone out). | OWASP first choice (F1). Stdlib has only PBKDF2 (F3), which is not memory-hard. |
| PD2 | **New dependency `golang.org/x/crypto`** (integrator ruling R-1: accepted, `argon2` only). Go-team maintained, same trust domain as the `golang.org/x/oauth2` and `x/text` already in `go.mod`. Rejected: stdlib `crypto/pbkdf2` at 600k iterations (GPU-cheap), bcrypt (72-byte limit, F1), scrypt, hand-written Argon2. | PROCESS §5: one line in `docs/engineering/dependencies.md`. |
| PD3 | **Hashing and breach checks are bounded:** one process-wide semaphore of 4 slots guards every Argon2 computation and every HIBP call (4 × 19 MiB ≈ 76 MiB); a request that waits > 2 s gets 503 `busy`. Every rate-limit check (§6) runs **before** hashing or HIBP. | Argon2 memory and outbound calls are DoS levers (I23). |
| PD4 | **Emailed code:** 6 decimal digits from `crypto/rand` (uniform, rejection sampling), valid 10 minutes, at most 5 wrong entries per challenge, single use, bound to the purpose (`signup`/`login`/`reset`) and to a browser binding (32-byte random, HttpOnly cookie). DB stores only `HMAC-SHA256(auth_pepper, binding_hash ‖ code)` and the creating source's `ip_hmac` (PD14 bucket). Supersede rule: a `login` challenge supersedes all older open `login` challenges of the credential (the caller proved the password). A `reset` or `signup` challenge supersedes older open challenges of the same email + purpose **only when they were created from the same ip bucket**; otherwise both stay open, with at most 3 open per email + purpose (the oldest is superseded). | NIST ≥ 6 digits, ≤ 10 min (F5). Binding stops a code phished from the mail being used in another browser without the cookie. Same-source supersede stops an attacker from another source killing the victim's open reset/sign-up code (round-2 P1). |
| PD5 | **Reset-code budget per (principal, ip bucket):** applies to `reset` challenges only — `SUM(attempts)` over the principal's `reset` challenges created in the last 24 h **with the same `ip_hmac` as the challenge being completed** ≥ 10, **or** the same sum over all the principal's `reset` challenges ≥ 50 ⇒ `complete` for purpose `reset` returns `INVALID` without comparing the code or incrementing `attempts`. (The binding cookie is held by the creating browser, so a challenge's `ip_hmac` is the attacker's own source.) `login` challenges (which already required the correct password) are limited only by their own 5 attempts; `signup` challenges have no principal. | Bounds a reset-code takeover to 50/10^6 per day; one attacking source can no longer block the victim's reset (round-2 P1), nor the victim's login (round-1 P1). |
| PD6 | **No user enumeration:** sign-up and reset-start always answer 202 with the same shape; login answers the same 401 `invalid_credentials` for unknown email, wrong password and disabled credential, and runs Argon2 against a fixed dummy PHC when the email is unknown. Every rate-limit bucket, including the mail buckets for sign-up/reset, is hit **before** the existence lookup, whether or not the email exists. Sign-up of an existing email sends an "account already exists — sign in or reset" notice instead of a code. Response time does not depend on existence because sign-up/reset never wait for SMTP (PD7) and do the same hashing work. | OWASP auth guidance; verified by PA08b (timing). |
| PD7 | **Mail is sent once after commit, no queue, no retry.** The challenge row commits (tx 1) with `mail_state='PENDING'`; then one SMTP send; the outcome `SENT`/`FAILED`/`UNKNOWN` is recorded (tx 2). SMTP has no idempotency key, so nothing is ever re-sent automatically: an uncertain result stays `UNKNOWN` (I06: no blind retry). Recovery from a lost mail = the user presses "resend", a new step-1 call creating a **new** challenge (new code) that supersedes the old one. **`login` waits for the send** (the caller already proved the password; FAILED → 503 `mail_unavailable`). **`signup` and `reset` write the 202 response first**; the send then runs in a detached, bounded goroutine (non-blocking acquire of an 8-slot semaphore — when full, the send is skipped and recorded `FAILED`; context detached from the request, 25 s cap; graceful shutdown waits for in-flight sends up to 25 s; a crash leaves `PENDING`). The mail outcome never changes a sign-up/reset response. The plaintext code exists only in process memory until the send ends. | I06; I04 does not apply (the code is not a must-execute business task; a lost mail loses nothing). River would need a new isolated schema + worker for a 10-minute secret that must not be persisted in plaintext (rejected). Background send for signup/reset closes the timing oracle (round-1 P1). |
| PD8 | **Sessions reuse `identity.sessions`** (audience `merchant`, token hash only, TTL from existing `COMMERCE_SESSION_TTL` 5 min–24 h) and `identity.session_events('session.issued')`. A session is issued **only** inside the definer that consumes a correct code (§4.3), never from a function that takes a principal id. This is **not** a barrier against the `commerce_identity` login itself: it chooses the code HMAC, so a leaked identity DSN (`dsn_lc_api_identity`, already its own manifest row) means takeover of every password account — the same trust already placed in that role by `identity.issue_merchant_session(p_issuer,p_subject,…)` in 0004. | merchant-identity-v1 rule: no function takes an arbitrary principal target; trust boundary stated honestly. |
| PD9 | **Separate principals per method.** A password principal has one row in `identity.password_credentials`; an OIDC principal has `identity.external_identities`. Same email in both ⇒ two principals; no linking in v1. | No merge by email; linking is an account-takeover path. |
| PD10 | **Reset = code + new password in one step,** then: password version +1, consecutive-failure counter reset, **all** merchant sessions of the principal revoked (`session.revoked` events), one new session issued. | Ruling Q5. Revoking kills an attacker's session. |
| PD11 | **Password policy:** NFC-normalized, 12–128 Unicode code points (ruling Q2; see §11 for the NIST gap), no composition rules; not equal to the email **at sign-up** (at reset Go holds only the binding, not the email, so `equals_email` is NOT_IMPLEMENTED there — documented limit); not in Have I Been Pwned (k-anonymity range API, `Add-Padding: true`, F6) at sign-up and reset. HIBP unreachable (2 s timeout) ⇒ accept and log `password.breach_check_unavailable` (fail-open, ruling Q3; logged, not an audit row, §15 A3). | NIST: no composition rules, blocklist required (F4). |
| PD12 | **Hard disable after 100 consecutive wrong passwords** on one credential (`disabled_at`); the counter saturates at 100 and never raises (§4.2). Only a successful reset, or the owner-approved operator unlock (§11), re-enables. A disabled credential answers exactly like an unknown email (§2 Login). Temporary throttles (§6) fire long before this. | NIST §3.2.2 cap (F4). Lockout abuse bounds: §11. |
| PD13 | **Tenant scope is unknown before authentication** and must not come from the client (I01), so there is no per-tenant login bucket. The ceiling on cost/abuse is a deployment-wide mail budget split into three shares (§6): unauthenticated, login of principals **without** an active membership, and login of principals **with** one — so self-registered accounts without a store cannot spend store members' login mail. | Honest equivalent of per-tenant limits (round-2 P1). |
| PD14 | **Client IP** for rate limits: Caddy is the edge and replaces incoming `X-Forwarded-*` (F9, verified in `deploy/caddy/Caddyfile` header: no `trusted_proxies`). The admin BFF takes the single `X-Forwarded-For` value, requires an IP literal, and forwards it as `X-Commerce-Client-IP` on the BFF-key-authenticated private call (ruling R-5). Go buckets IPv4 by /32 and IPv6 by /64 (plus /48 for sign-up) and stores only `HMAC(auth_pepper, bucket)`. The admin host stays DNS-only (no Cloudflare proxy) until a `trusted_proxies` review (ruling Q6), checked by PA15. | No raw IP at rest; no trust in a browser header. |
| PD15 | **Mail transport: generic SMTP** (O-B) — Go stdlib `crypto/tls` + `net/smtp`, implicit TLS on port 465 only, `PLAIN` auth, one real adapter behind a consumer-side interface; tests use a loopback TLS fake SMTP server, so the real adapter is what MOCK exercises (§3). First configuration: a dedicated QQ mailbox (`smtp.qq.com:465`, SMTP authorization code as the secret, F8). Upgrade: Tencent Exmail (`smtp.exmail.qq.com:465`, sender `@xgdwm.com` with SPF/DKIM/DMARC on Cloudflare, F10) — a config + secret change, no code change. | O-B; no new dependency (stdlib only). |

Rejected alternatives:
- Magic links instead of codes: a secret in a URL (I11; Caddy logs query strings), opened on another
  device than the one signing in.
- Storing the plaintext code, or a plain SHA-256 of it (a 6-digit code falls to 10^6 hashes).
- A River job / outbox worker for auth mail (PD7).
- Redis or in-memory rate limiting: a second store / lost on restart; PG counters suffice (AGENTS.md).
- Account lockout that answers differently for known emails (enumeration).
- Creating the principal before the email is verified (lets anyone squat an email address).
- Auto-linking an OIDC identity and a password credential with the same email (PD9).
- SMS codes: cost, SIM-swap, and no owner request.
- Resend / Cloudflare Email Sending / Amazon SES / Postmark as the v1 transport: owner chose SMTP (O-B);
  a provider HTTP API can be added later behind the same `Mailer` interface.
- Automatic SMTP retry on an uncertain result: SMTP has no idempotency key, so a retry can deliver
  two different-looking mails for one code and hides provider throttling (O-B).
- A per-principal code budget across login challenges (round-0 PD5): an attacker knowing only the
  email could exhaust it with reset challenges and block the victim's login (round-1 P1).
- Round-1 reset/sign-up lockout primitives: an email-only `email-mail-unauth` bucket, supersede of
  any open reset/sign-up challenge by any new one, and a per-principal-only reset budget — together
  they let one source keep a disabled credential locked forever (round-2 P1).
- An unbounded `failed_count+1` against a `CHECK (… ≤ 100)`: the 101st wrong password raised (5xx),
  an existence oracle (round-2 P1).

## 1. External facts relied on (retrieved 2026-09-29)

| # | Fact | Source |
| --- | --- | --- |
| F1 | OWASP Argon2id minimum configs incl. m=19 MiB, t=2, p=1; PBKDF2-HMAC-SHA256 600,000 iterations; bcrypt 72-byte input limit. | https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html |
| F2 | `argon2.IDKey(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte` implements Argon2id; module `golang.org/x/crypto` v0.57.0 published 2026-09-08. | https://pkg.go.dev/golang.org/x/crypto/argon2 |
| F3 | Go 1.27.1 stdlib `crypto/` has `pbkdf2`, `hkdf`, `sha3`, no argon2/scrypt/bcrypt. `go.mod` declares `go 1.27.1`. | local `ls $(go env GOROOT)/src/crypto`; `go.mod` line 3 |
| F4 | NIST SP 800-63B-4: passwords ≥ 15 chars single-factor, ≥ 8 as part of MFA; no composition rules; compare against breached/dictionary values; limit consecutive failures to ≤ 100; "Email SHALL NOT be used for out-of-band authentication". | https://pages.nist.gov/800-63-4/sp800-63b.html (§3.1.1.2, §3.1.3.1, §3.2.2) |
| F5 | Same: OOB/OTP secrets ≥ 6 decimal digits; invalid unless completed within 10 minutes. | same, §3.1.3.2 |
| F6 | Pwned Passwords `GET https://api.pwnedpasswords.com/range/{5 hex of SHA-1}`, no API key, `Add-Padding: true` pads to 800–1,000 rows, no rate limit. | https://haveibeenpwned.com/API/v3 |
| F7 | Go `net/smtp` is frozen (no new features) but supported; `smtp.NewClient(conn net.Conn, host)` wraps an existing connection (so a `crypto/tls` connection gives implicit TLS on 465); `smtp.PlainAuth` sends credentials only over TLS or to localhost, else fails without sending them. | local `go doc net/smtp`, `go doc net/smtp.PlainAuth`, `go doc net/smtp.NewClient` (go1.26.2); https://pkg.go.dev/net/smtp |
| F8 | QQ mailbox third-party sending: SMTP server `smtp.qq.com`, port 465 with SSL; username = full mailbox address; password = a 16-character **authorization code** generated after enabling POP3/SMTP in mailbox settings (SMS-verified), not the web login password. Sending limits are **not documented**; QQ's help site lists the bounce "550 Sender frequency limited", and community reports mention throttling after ~10 rapid sends (unofficial). Daily cap: **UNKNOWN**. | https://help.mail.qq.com/detail/0/1087 (authorization code); https://cloud.tencent.com/developer/article/2177098 (host/port); https://blog.csdn.net/qq_44776721/article/details/113782738 (unofficial throttle report) |
| F9 | Caddy `reverse_proxy` ignores incoming `X-Forwarded-*` unless `trusted_proxies` is set. | https://caddyserver.com/docs/caddyfile/directives/reverse_proxy |
| F10 | Tencent Exmail SMTP `smtp.exmail.qq.com:465`; sender on the owner's domain with SPF/DKIM/DMARC records on Cloudflare. Exact record values: **UNKNOWN** until read from the Exmail admin console at setup, never typed from this file. | owner decision O-B, `docs/delivery/units/r2-design-rulings.md` |

## 2. Flows

Step-1 routes return a fresh challenge binding that the BFF puts in `__Host-commerce_challenge`.
Step 2 is always `complete` (§7).

**Sign-up.** `POST signup {email,password,locale}` → throttles (§6, all before any lookup) →
normalize email, check password policy (PD11) → HIBP + Argon2 hash under PD3 (always, also when the
email exists) → `start_signup_challenge`:
- email free → challenge row (purpose `signup`, `pending_password_hash`) → 202 → background code mail.
- email taken → no row → 202 → background "account exists" notice.
Both → 202 `{binding,expires_at}` before any send. `complete{code}` → principal + credential
(`email_verified_at`) + session in one transaction → cookies set → existing onboarding
(`/v1/identity/initial-store`, still behind `COMMERCE_ONBOARDING_ENABLED`) unchanged.

**Login.** `POST login {email,password,locale}` → throttles `ip`, `email-pw` → `password_login_material(email)` →
Argon2 verify (dummy PHC when no row). **Argon2 verify always runs against the stored PHC, also when
`disabled` is true; a disabled credential answers 401 `invalid_credentials` whether or not the
password matches, after calling `record_password_failure` when it does not match.** Wrong (any
credential state, or unknown email): `record_password_failure(email)` → 401. Right and not disabled →
throttle `email-mail-login` → `start_login_challenge(email, password_version, ip)` (returns
`has_membership`) → throttle `global-mail-login` (member) or `global-mail-login-new` (no active
membership); over → `record_challenge_mail(id,'FAILED')`, 503 `mail_unavailable`, no SMTP →
synchronous code mail → 202; FAILED → 503 `mail_unavailable`; UNKNOWN → 202 (user can resend).
`complete{code}` → session. On a `global-mail-login*` 503 the `email-mail-login` hit is not refunded;
the UI tells the user to wait instead of retrying (round-3 P2; PA07 asserts the consumption).

**Reset.** `POST reset {email,locale}` → throttles (incl. `ip-mail-unauth`/`ip48-mail-unauth`, `email-mail-unauth`, `email-mail-unauth-total`, `global-mail-unauth`,
all before the lookup) → `start_reset_challenge` → 202 always → background code mail if a credential
exists. `complete{code,new_password}` → `code` must be 6 digits → `binding` and `ip` throttles →
policy + HIBP + Argon2 under PD3 → SQL → PD10.

**Resend** = repeat step 1 with the values still held in the page (no extra endpoint).

## 3. Mail adapter

`internal/mail` (new package): `type Message struct{To, Subject, Text, HTML string}`,
`func (c *SMTP) Send(ctx, Message) (reply string, err error)`.
- Dial: `tls.DialWithDialer` to `COMMERCE_SMTP_HOST:465` (10 s dial timeout, `ServerName` = host,
  `MinVersion` TLS 1.2, system roots; a test root CA and a loopback host only with
  `COMMERCE_IDENTITY_ALLOW_LOOPBACK_TESTS=1`); `smtp.NewClient(conn, host)`; `conn.SetDeadline`
  = min(ctx deadline, 25 s); `Auth(smtp.PlainAuth("", user, secret, host))`; `Mail(from)`;
  `Rcpt(to)`; `Data()` write; `Close()` of the data writer (reads the final reply); `Quit()`.
- Message built with stdlib only (`mime`, `mime/multipart`, `mime/quotedprintable` or base64):
  headers `From`, `To`, `Subject` (RFC 2047 `mime.BEncoding` for zh), `Date`, `Message-ID`
  (random, `@` + From domain), `MIME-Version`, `Auto-Submitted: auto-generated`,
  `Content-Type: multipart/alternative` (text + minimal HTML, UTF-8). No tracking, no links.
- Classification (**no retry in any branch**, PD7):
  - any failure before the end-of-data `.` is written (dial, TLS, AUTH, MAIL, RCPT, DATA 354) → **FAILED** (the server cannot have accepted the message);
  - final reply 2xx → **SENT** (`reply` = reply text, ≤ 128 bytes, stored as `provider_message_id`);
  - final reply 4xx/5xx (incl. `550 Sender frequency limited`, F8) → **FAILED**, log kind `smtp_rejected` + reply code only;
  - no final reply (timeout, reset) after the `.` was written → **UNKNOWN**.
- Errors and logs never contain the recipient, the code, the body, the username or the secret (I11).
- The consumer interface lives in `internal/identity` (`type Mailer interface{ Send(...) }`), the
  same testing-seam pattern as `Provider`; there is one real implementation.
- Loopback fake: `internal/mail/mailtest` (TLS SMTP server + in-memory mailbox), same pattern as
  `internal/integrations/psp/stripe/stripetest`; imported only by tests; `cmd/api` must not
  depend on it (PA14 check).

Mail content (`internal/identity/mailcopy.go`, zh-CN/zh-TW/en, chosen by the step-1 `locale`):
subject without the code; text + minimal HTML with the code, purpose, "valid 10 minutes", "if this
was not you, ignore this mail / reset your password"; no links, no images, no tracking pixels.
From: `COMMERCE_MAIL_FROM` = display name + the SMTP username address (config check: the address
must equal `COMMERCE_SMTP_USERNAME`, because QQ/Exmail send only as the authenticated mailbox —
verified in PA12; default display name `xgdwm`).

DNS: none for the QQ mailbox (sender `@qq.com`; SPF/DKIM are QQ's). Exmail upgrade: MX/SPF/DKIM
records from the Exmail admin console on `xgdwm.com` (DNS only), `_dmarc.xgdwm.com`
`v=DMARC1; p=none; rua=mailto:<owner mailbox>` for 2–4 weeks, then `p=quarantine` (F10).

## 4. Persistence: `migrations/0070_merchant_password_auth.sql`

### 4.1 Tables (owned by the migration owner, as in 0004; `commerce_identity_writer` gets only the §4.4 grants; COMMENT ON each table/column per PROCESS §5)

Verified: 0004 creates `identity.login_flows`, `external_identities`, `initial_stores`,
`session_events` without `ALTER TABLE … OWNER`, so they belong to the migration role and the
writer holds only explicit grants; only the definers are `OWNER TO commerce_identity_writer`.

`identity.password_credentials`
- `principal_id uuid PK REFERENCES identity.principals(id)`
- `email text NOT NULL UNIQUE CHECK (length(email) BETWEEN 3 AND 254 AND email ~ '^[!-~]+$' AND email = lower(email) AND email ~ '^[^@]+@[^@]+$')` — ASCII only in v1 (`[!-~]` = 0x21–0x7E, which also excludes space and control characters), so `lower()` is collation-independent; Go rejects non-ASCII emails with 422 invalid email.
- `password_hash text NOT NULL CHECK (password_hash ~ '^\$argon2id\$v=19\$m=[0-9]{4,7},t=[0-9]{1,2},p=[0-9]{1,2}\$[A-Za-z0-9+/]{22}\$[A-Za-z0-9+/]{43}$')`
- `password_version bigint NOT NULL DEFAULT 1 CHECK (password_version > 0)`
- `failed_count int NOT NULL DEFAULT 0 CHECK (failed_count BETWEEN 0 AND 100)`, `disabled_at timestamptz`
- `email_verified_at timestamptz NOT NULL`, `created_at`, `password_changed_at timestamptz NOT NULL DEFAULT clock_timestamp()`

`identity.email_challenges`
- `id uuid PK` (Go-generated)
- `purpose text NOT NULL CHECK (purpose IN ('signup','login','reset'))`
- `email text NOT NULL` (same CHECK as above; the recipient), `locale text NOT NULL CHECK (locale IN ('zh-CN','zh-TW','en'))`
- `principal_id uuid REFERENCES identity.principals(id)`, `password_version bigint`, `pending_password_hash text` (PHC CHECK)
- CHECK: `signup` ⇔ principal NULL, pending hash NOT NULL; `login` ⇔ principal + version NOT NULL, pending NULL; `reset` ⇔ principal NOT NULL, version and pending NULL
- `binding_hash bytea NOT NULL UNIQUE CHECK (octet_length=32)`, `code_hmac bytea NOT NULL CHECK (octet_length=32)`,
  `ip_hmac bytea NOT NULL CHECK (octet_length=32)` (creating source's PD14 bucket; PD4 supersede, PD5 budget)
- `attempts smallint NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 5)`
- `created_at timestamptz NOT NULL DEFAULT clock_timestamp()`, `expires_at timestamptz NOT NULL` (= created + 10 min, set in SQL), `consumed_at timestamptz`, `consumed_reason text CHECK (consumed_reason IN ('verified','superseded','exhausted'))`
- `mail_state text NOT NULL DEFAULT 'PENDING' CHECK (IN ('PENDING','SENT','FAILED','UNKNOWN'))`, `provider_message_id text CHECK (length ≤ 128)` (SMTP 2xx reply text)
- index `(email, purpose) WHERE consumed_at IS NULL`; index `(expires_at)`; index `(principal_id, created_at) WHERE purpose = 'reset'` (PD5 count).
- On consume or expiry purge, `pending_password_hash` is set NULL.

`identity.auth_throttle` — `bucket bytea` (32 bytes: `sha256(p_bucket || int4send(window seconds) || int4send(offset))`,
derived inside `auth_throttle_hit` so every window of a bucket has its own key, §15 A1), `window_start timestamptz`, `hits int`,
PK `(bucket, window_start)`; index `(window_start)`.

`identity.auth_events` (append-only audit; no tenant exists yet so `ops.audit_events` cannot hold
it) — `id uuid PK DEFAULT gen_random_uuid()`, `principal_id uuid NULL REFERENCES identity.principals(id)`,
`email_hmac bytea NULL` (32), `ip_hmac bytea NULL` (32), `challenge_id uuid NULL`,
`action text NOT NULL CHECK (action IN ('signup.requested','signup.exists_notified','signup.verified',
'login.password_failed','login.password_ok','login.disabled','login.verified','reset.requested',
'reset.completed','code.failed','code.exhausted','throttled','mail.sent','mail.failed','mail.unknown',
'password.breach_check_unavailable','operator.unlocked'))`, `created_at`. `operator.unlocked` is
written only by the migration-owner runbook SQL (§11), never by a definer. No raw email, IP, code or
password. Retention 180 days (ruling Q8).

### 4.2 SQL entry points (all `SECURITY DEFINER`, owner `commerce_identity_writer`,
`SET search_path=pg_catalog`, `REVOKE ALL … FROM PUBLIC`, `GRANT EXECUTE … TO commerce_identity`)

| Function | Does |
| --- | --- |
| `identity.auth_throttle_hit(p_bucket bytea, p_window_seconds int, p_offset_seconds int) RETURNS int` | Upsert +1 in the window aligned to `p_offset_seconds` (0, or 28800 for the UTC+8 day) under the derived key `sha256(p_bucket || int4send(p_window_seconds) || int4send(p_offset_seconds))` (§15 A1), return hits; deletes ≤ 100 throttle rows older than 2 days, ≤ 100 challenges expired > 1 day, ≤ 100 `auth_events` older than 180 days (I23). |
| `identity.start_signup_challenge(p_id uuid, p_email text, p_hash text, p_binding bytea, p_code bytea, p_locale text, p_ip bytea) RETURNS TABLE(outcome text, expires_at timestamptz)` | Advisory xact lock on the email; `EXISTS` if a credential has the email (no row); else supersede open signup challenges for the email **with the same `ip_hmac`**, then supersede the oldest while ≥ 3 remain open (PD4), insert, `CHALLENGE`. |
| `identity.password_login_material(p_email text) RETURNS TABLE(password_hash text, password_version bigint, disabled boolean)` | Zero rows when unknown or principal inactive. Returns no principal id. |
| `identity.record_password_failure(p_email text) RETURNS void` | `failed_count = LEAST(failed_count+1, 100)`; sets `disabled_at` when it reaches 100 and `disabled_at` IS NULL; audit. Never raises for a disabled or unknown email (same statement count; no-op for unknown). |
| `identity.start_login_challenge(p_id uuid, p_email text, p_version bigint, p_binding bytea, p_code bytea, p_locale text, p_ip bytea) RETURNS TABLE(expires_at timestamptz, has_membership boolean)` | Lock credential; require version match, not disabled, principal active (else PT401); supersede open login challenges; insert; `has_membership` = an active `identity.memberships` row of the principal in an active `control.tenants` row (the writer already has SELECT on both from 0004; neither has RLS). |
| `identity.start_reset_challenge(p_id uuid, p_email text, p_binding bytea, p_code bytea, p_locale text, p_ip bytea) RETURNS TABLE(outcome text, expires_at timestamptz)` | `NONE` when unknown/inactive; else supersede open reset challenges **with the same `ip_hmac`**, then the oldest while ≥ 3 remain open (PD4), insert, `CHALLENGE`. |
| `identity.record_challenge_mail(p_id uuid, p_state text, p_message_id text) RETURNS void` | Set once from `PENDING`; audit `mail.*`. |
| `identity.complete_email_challenge(p_binding bytea, p_purpose text, p_code bytea, p_new_hash text, p_session bytea, p_ttl bigint) RETURNS TABLE(outcome text, expires_at timestamptz)` | §4.3. |

Lock order everywhere: email advisory lock → principal → credential → challenge → sessions
(extends 0004's "principal then session").

### 4.3 `complete_email_challenge`

1. Validate args (`p_ttl` 300–86400, byte lengths, purpose). Look up the challenge by
   `binding_hash` and purpose; missing / consumed / expired → `INVALID` (returned, not raised, so
   the attempt counter commits).
2. Reset budget (PD5, purpose = `reset` only): lock the principal row `FOR UPDATE` (0004's
   `UPDATE (id)` grant exists for exactly this), then over the principal's `reset` challenges with
   `created_at` in the last 24 h: `SUM(attempts) FILTER (WHERE ip_hmac = <this challenge's ip_hmac>)`
   ≥ 10, or `SUM(attempts)` ≥ 50 → `INVALID` with no compare and no `attempts` change. The lock
   serializes concurrent `complete` calls on different challenges of the same principal.
3. Constant-time compare is not available in SQL; the comparison is between two HMACs of
   server-side secrets, so plain `=` leaks nothing useful. Mismatch → `attempts+1`
   (5 ⇒ `consumed_reason='exhausted'`), audit `code.failed` → `INVALID`.
4. Match → consume (`verified`) and branch:
   - `signup`: lock email; credential already exists (race) → `EXISTS`; else insert principal,
     credential (`pending_password_hash`, `email_verified_at=now`), session, `session.issued`,
     `signup.verified` → `SESSION`.
   - `login`: principal active, credential not disabled, version = bound version (else `INVALID`);
     `failed_count=0`; session + events → `SESSION`.
   - `reset`: `p_new_hash` required; hash, version+1, `failed_count=0`, `disabled_at=NULL`,
     `password_changed_at`; revoke every unrevoked merchant session of the principal with
     `session.revoked` events; supersede open `login` challenges; new session → `SESSION`.
Go maps `INVALID` → 401 `invalid_code`, `EXISTS` → 409 `account_exists`, `SESSION` → token.
Go never distinguishes an exhausted challenge, a wrong code, an over-budget principal or an unknown
binding.

### 4.4 Grants (0070)

- `commerce_identity_writer` (not owner of any new table): SELECT, INSERT on the four new tables;
  UPDATE only on
  `password_credentials(password_hash,password_version,failed_count,disabled_at,password_changed_at)`,
  `email_challenges(attempts,consumed_at,consumed_reason,mail_state,provider_message_id,pending_password_hash)`,
  `auth_throttle(hits)`; DELETE on `auth_throttle`, `email_challenges`, `auth_events` (purge only);
  no TRUNCATE, no UPDATE on `auth_events`; the 0004 grants (SELECT, INSERT on principals/sessions/
  session_events, `UPDATE (id)` on principals, `UPDATE (revoked_at)` on sessions) cover session
  issue, principal lock and revoke-all.
- `commerce_identity`: EXECUTE on §4.2 only; no table privilege.
- `commerce_runtime`, `commerce_auth` and every other role: nothing on the new tables.
- No RLS (ruling R-3): identity tables are not tenant rows; isolation is by privilege, as in 0004.

## 5. Go and BFF configuration

Go (`cmd/api/identity.go`), all under `COMMERCE_IDENTITY_ENABLED=1`:
- `COMMERCE_PASSWORD_LOGIN_ENABLED` `0`/`1`. With `1`: OIDC variables become optional (ruling R-4);
  required:
  - `COMMERCE_AUTH_PEPPER_FILE` (b64url32; new `deploy/secrets.manifest.tsv` row
    `commerce_auth_pepper b64url32 gen api`; rotation invalidates only open challenges and
    throttle windows);
  - `COMMERCE_SMTP_HOST` (DNS name; loopback only with `COMMERCE_IDENTITY_ALLOW_LOOPBACK_TESTS=1`),
    port fixed 465 (implicit TLS; STARTTLS/587 not supported in v1), `COMMERCE_SMTP_USERNAME`
    (full mailbox address), `COMMERCE_MAIL_FROM` (address must equal the username);
  - `COMMERCE_SMTP_PASSWORD_FILE` — new manifest row `commerce_smtp_password opaque owner api
    <rotation>`; the value is the mailbox's SMTP **authorization code** (F8), supplied by the owner
    as a file (O-D), never logged or echoed. Scope: a QQ/Exmail authorization code also grants
    IMAP/POP access to that mailbox and cannot be narrowed, so it must belong to a **dedicated
    sending mailbox**, never the owner's personal mailbox. Rotation = generate a new code in the
    mailbox settings, write the file, restart api, revoke the old code there.
  - `COMMERCE_MAIL_DAILY_CAP` (default 200, range 20–100000; §6 splits it);
  - `COMMERCE_BREACH_CHECK` `hibp`|`off` (`off` only with loopback tests).
- `COMMERCE_HIBP_BASE_URL` and a test SMTP root CA: loopback-only overrides, accepted only with
  `COMMERCE_IDENTITY_ALLOW_LOOPBACK_TESTS=1`; production uses the fixed hosts.
- Missing/invalid config fails startup (as today). Only `internal/mail` dials the SMTP host; only
  `internal/identity` dials `api.pwnedpasswords.com` (package docs name both, PROCESS §5).

Admin BFF (`apps/admin/lib/auth.ts`): `COMMERCE_OIDC_ISSUER` optional when the password flag is on;
`COMMERCE_PASSWORD_LOGIN_ENABLED` mirrors Go. OIDC button shown only when OIDC is configured.

## 6. Rate limits (fixed windows in `identity.auth_throttle`, checked before hashing, HIBP, mail or existence lookup)

| Bucket (HMAC key input) | Limit | Applies to |
| --- | --- | --- |
| `ip:<ip/32 or /64>` | 30 / 15 min | every step-1 and complete call |
| `ip48:<ipv6/48>` | 60 / 15 min | every step-1 and complete call from IPv6 (§15 A2) |
| `ip-signup:<ip/32 or /64>` | 5 / hour | sign-up |
| `ip48-signup:<ipv6/48>` | 20 / hour | sign-up from IPv6 |
| `ip-mail-unauth:<ip/32 or /64>` | 10 / UTC+8 day | sign-up, exists-notice, reset — hit before `global-mail-unauth` and before the existence lookup |
| `ip48-mail-unauth:<ipv6/48>` | 20 / UTC+8 day | same, IPv6 |
| `email-pw:<email>‖<ip bucket>` | 10 / 15 min | login password checks (known or unknown email alike); keyed per source so other sources cannot exhaust the victim's bucket |
| `email-mail-unauth:<email>‖<ip bucket>` | 1 / 60 s, 5 / hour, 10 / UTC+8 day | sign-up, exists-notice, reset — hit for every call before the existence lookup; keyed per source so another source cannot exhaust the victim's bucket |
| `email-mail-unauth-total:<email>` | 30 / UTC+8 day | same calls, all sources together (caps mail to one address) |
| `email-mail-login:<email>` | 1 / 60 s, 5 / hour, 5 / UTC+8 day | login mail only, hit after the password verified; never consumed by unauthenticated calls |
| `binding:<binding>` | 10 / 10 min | complete |
| `global-mail-unauth` | 40 % of `COMMERCE_MAIL_DAILY_CAP` per UTC+8 day (default 80) | sign-up + notice + reset, hit before the existence lookup |
| `global-mail-login-new` | 15 % (default 30) | login mail of principals with **no** active `identity.memberships` row (`has_membership=false` from `start_login_challenge`) |
| `global-mail-login` | the remaining 45 % (default 90) | login mail of principals **with** an active membership only |
| reset-code budget | 10 wrong / 24 h per (principal, ip bucket); 50 wrong / 24 h per principal | PD5, in SQL |

Shares are `floor(cap × pct / 100)`; with the minimum cap 20 they are 8 / 3 / 9 (all ≥ 1).

Over a per-client limit → 429 `throttled` with `Retry-After` = window remainder; identical for
known and unknown emails. Over a `global-mail-*` limit → 503 `mail_unavailable` (fail closed, O-B),
raised before any existence lookup, identical for known and unknown emails. `throttled` is logged with
the bucket kind only, not written as an audit row (§15 A3). The provider's real daily cap is UNKNOWN (F8): `COMMERCE_MAIL_DAILY_CAP` is
set from the first observed `smtp_rejected` rate, never raised above what the mailbox has sustained.

## 7. HTTP

### 7.1 Private Go (`/v1/identity/password/*`), same rules as merchant-browser-auth-v1

BFF key, no Origin/Cookie, strict JSON ≤ 64 KiB, no query, no-store, sanitized errors, plus exactly
one valid `X-Commerce-Client-IP` (else 400). Never auto-retried by the BFF.

| Route | Request | Success | Errors |
| --- | --- | --- | --- |
| POST `/v1/identity/password/signup` | `{email,password,locale}` | 202 `{binding,expires_at}` (always, PD6) | 422 `password_policy` (`too_short`/`too_long`/`breached`/`equals_email`), 422 invalid email, 429, 503 `busy`/`mail_unavailable` (both raised before any existence lookup) |
| POST `/v1/identity/password/login` | `{email,password,locale}` | 202 `{binding,expires_at}` | 401 `invalid_credentials`, 429, 503 `mail_unavailable`/`busy` |
| POST `/v1/identity/password/reset` | `{email,locale}` | 202 `{binding,expires_at}` (always) | 422 invalid email, 429, 503 `mail_unavailable` (before any existence lookup) |
| POST `/v1/identity/password/complete` | `{binding,purpose,code,new_password?}` (`new_password` only and required for `reset`) | 200 `{token,expires_at}` | 401 `invalid_code`, 409 `account_exists`, 422 `password_policy`, 429, 503 `busy` |

`complete` rejects a `code` that is not exactly 6 ASCII digits with 401 `invalid_code` before any
other work; for `reset`, HIBP + Argon2 of `new_password` run only after the `binding` and `ip`
throttles pass, under the PD3 semaphore.
For sign-up of a taken email and reset of an unknown email the returned binding is random and has
no row, so `complete` answers `invalid_code` exactly like a wrong code. Response time is not padded;
the work up to the response is the same and SMTP is never awaited on these paths (PD6/PD7).

### 7.2 Public admin BFF (`apps/admin/app/api/auth/password/*`)

- POST `signup`, `login`, `reset`: JSON, exact `Origin` = public origin (anonymous, like
  `/api/auth/login`), no query, strict keys; forwards client IP (PD14); on 202 sets
  `__Host-commerce_challenge` = `<binding>.<purpose>.<locale>` (Secure, HttpOnly, SameSite=Lax,
  Path=/, Max-Age ≤ 600, no Domain) and returns 202 `{step:"code",expires_at}`. Password never
  echoed or logged.
- POST `verify`: JSON `{code,new_password?}`, exact Origin, exactly one challenge cookie; calls
  `complete` once; clears the challenge cookie on 200 and 409 only — every 401 keeps it and it
  expires through Max-Age ≤ 600 (Go never signals exhaustion); on 200 calls the existing
  `setSessionCookies` (session + CSRF) and returns `{redirect:"/<locale>/"}`. A valid existing
  session is not destroyed by a failed verify.
- Existing `/api/auth/logout`, `/api/onboarding/initial-store`, `/api/stores*` unchanged.

### 7.3 Admin pages (reuse `components/Entry.tsx` shell, `globals.css` tokens, `entry-copy.ts`)

One client component `PasswordAuth.tsx` with modes `signin` | `signup` | `reset` and a `code` step,
mounted at `/[locale]/` (signed-out state, replacing the OIDC button when OIDC is off),
`/[locale]/signup`, `/[locale]/reset`. Fields: `type=email autocomplete=email`,
`type=password autocomplete=current-password|new-password`, code
`inputmode=numeric autocomplete=one-time-code maxlength=6`. Resend button disabled for 60 s.
All strings in `lib/entry-copy.ts` for zh-CN/zh-TW/en (no hard-coded text): titles, field labels,
policy hint (12+ characters), `invalid_credentials`, `invalid_code`, `throttled` (with minutes),
`account_exists`, `mail_unavailable`, `busy`, `password_policy.*`, "if an account exists, a code
was sent to <masked email>" (sign-up/reset wording must not confirm existence), expiry hint, "check
spam folder". Audit-first (CLAUDE.md): before drawing, check the existing Entry sign-in/onboarding
structure and reuse it; no new visual language. Visual approval by the owner stays pending
(merchant-browser-auth-v1), recorded as screenshots in PA11.

## 8. Idempotency and uniqueness

- Step-1 calls are **not** idempotent by design: each creates a new code and supersedes older open
  challenges per PD4 (login: all; sign-up/reset: same source, ≤ 3 open); throttles bound repetition.
- Mail send: exactly one SMTP attempt per challenge, never retried (PD7); `record_challenge_mail`
  sets the state once from `PENDING`.
- `complete`: single use by `consumed_at` under row lock; a replayed correct code → `INVALID`.
- One credential per email (UNIQUE), one per principal (PK); concurrent sign-ups serialize on the
  email advisory lock.
- Session token: 32 random bytes, only SHA-256 stored (existing).

## 9. Test gates (tiers: UNIT, REAL_PG, MOCK, HTTP_PG, BROWSER, SANDBOX, LIVE)

| Gate | Test | Tier | Required |
| --- | --- | --- | --- |
| PA01 | `TestPasswordPA01Crypto` | UNIT | PHC round trip; rejects argon2i/argon2d/bcrypt strings and bad params; verify uses `subtle.ConstantTimeCompare`; dummy path calls IDKey (counter); semaphore 5th caller waits then `busy` (Argon2 and HIBP share it); code generator uniform (χ² on 10^6 draws of the first digit), always 6 digits; HMAC input binding‖code; email normalization table (case, NFC, spaces, >254, two `@`, non-ASCII → invalid); policy table (11/12/128/129 code points, emoji counted as one, equals email at sign-up). |
| PA02 | `TestMailPA02SMTP` | UNIT (loopback TLS fake) | Implicit TLS + PLAIN auth; MIME headers exact (UTF-8 subject encoding, `Auto-Submitted`, no tracking); classification table §3: failure before `.` → FAILED, 2xx → SENT, 4xx/5xx final → FAILED, drop after `.` → UNKNOWN; **zero** re-sends in every branch (fake counts DATA); From ≠ username refused at config; logs/errors contain no recipient/code/username/secret (canary scan). |
| PA03 | `TestPasswordPA03Schema` | REAL_PG | Tables, CHECKs (negative per CHECK incl. purpose shape, non-ASCII email, `ip_hmac` length, `failed_count` 101), `operator.unlocked` accepted by the action CHECK, no plaintext-code column, definer owner/`proconfig`/ACL, PUBLIC revoked; `pg_tables.tableowner` of all four tables ≠ `commerce_identity_writer`; writer UPDATE on a non-granted column (`email_challenges.code_hmac`, `auth_events.action`) denied; writer TRUNCATE denied; privilege matrix: `commerce_identity` EXECUTE only, `commerce_runtime`/`commerce_auth` denied on all four tables; upgrade from 0066 populated DB. |
| PA04 | `TestPasswordPA04Signup` | REAL_PG | start→complete creates principal+credential+session+2 events in one tx (fault injection after credential insert leaves nothing); taken email → `EXISTS`, no row; two concurrent signups same email → one principal (two-tx witness); wrong code 5× → exhausted; expired; replay after success; wrong binding; supersede by newer challenge. |
| PA05 | `TestPasswordPA05Login` | REAL_PG + HTTP_PG | Version bound (reset between start and complete → `INVALID`); inactive principal; 100 failures → disabled, then correct password still 401; **101st and 150th wrong password on a disabled credential → 401 identical to an unknown email (status, body shape, no 5xx), `failed_count` stays 100**; Argon2 counter increments on the disabled path; success resets counter; OIDC principal with same email untouched (PD9); `has_membership` false before and true after `initial-store`, false after membership or tenant deactivated. |
| PA06 | `TestPasswordPA06Reset` | REAL_PG | All merchant sessions revoked with events, other audiences untouched; version+1; open login challenges superseded; unknown email `NONE`; disabled credential re-enabled; reset budget: 10 wrong reset codes across challenges from one ip bucket → further reset from that bucket `INVALID` (attempts unchanged), another bucket still verifies; 50 across buckets → all `INVALID`; concurrent completes on two challenges of one principal never exceed either budget (two-tx witness); PD4: a reset/sign-up challenge from bucket B does not supersede bucket A's open one, same-bucket does, a 4th open supersedes the oldest. |
| PA07 | `TestPasswordPA07Throttle` | REAL_PG + HTTP_PG | Window alignment (incl. UTC+8 day), limits of §6 at N and N+1, purge bounds (≤100 rows per call); **attacker-driven resets + wrong reset codes up to every limit, then the victim's login start + complete still succeeds**; `email-pw` from source A exhausted, victim from source B still logs in; unauth traffic exhausting `global-mail-unauth` leaves `global-mail-login` untouched; **credential disabled, attacker from source A at every limit of §6 + PD5 (starting at the UTC+8 day boundary), then the victim from source B completes a reset**; **one source at every limit of §6 (incl. unknown-email resets) leaves `global-mail-unauth` with ≥ 1 remaining hit, and the victim's reset from source B succeeds; source A's 11th unauth mail that day → 429**; a `global-mail-login` 503 consumes one `email-mail-login` hit (not refunded, §2); **one account logging in at every allowed rate for 24 h consumes ≤ 5 login mails**; **accounts without a store exhausting `global-mail-login-new` leave a store member's login start + complete working**. |
| PA08 | `TestPasswordPA08HTTP` | HTTP_PG | Strict transport rules; missing/duplicate/invalid client IP; identical status, header names, JSON key set and value types (`binding` and `expires_at` excluded from comparison) for: signup new vs taken, reset known vs unknown, login unknown vs wrong password vs disabled; after 6 wrong codes, reset `complete` responses for known and unknown emails identical; 429 and global-cap 503 identical for known/unknown; no password/code/email in logs (canary). |
| PA08b | `TestPasswordPA08bTiming` | HTTP_PG | Fake SMTP delay 400 ms: median latency reset known vs unknown (n=50 each) differs by < 50 ms; same for signup new vs taken; login disabled vs unknown email (wrong password) differs by < 50 ms; login waits for the send (≥ 400 ms). |
| PA09 | `TestPasswordPA09MockMail` | MOCK | Full sign-up/login/reset through the real SMTP adapter against `mailtest`: mail per locale contains the code, no URL, no code in subject; FAILED on login → 503; UNKNOWN → 202 and user can resend (new code, old binding `invalid_code`); background semaphore full → `FAILED` recorded, response unchanged; exactly one DATA per challenge. |
| PA10 | `tests/admin/password-bff.test.ts` | Node | Origin rule, strict keys, cookie flags/Max-Age, challenge cookie cleared on 200/409 only and kept on 401, session+CSRF cookies set via existing helper, X-Forwarded-For parsing (missing/multiple/invalid → 503/400), OIDC-less config valid, password never in response or logs. |
| PA11 | `tests/admin/password-auth.spec.ts` | BROWSER | Sign-up → code (read from the `mailtest` mailbox inspection endpoint — exists only in the test binary on loopback) → onboarding → logout → login → code → workspace; forgot → reset → signed in, old session revoked; wrong code, throttled, resend cooldown; zh-CN/zh-TW/en, desktop + mobile Chromium; autocomplete attributes; screenshots hashed for owner visual approval. |
| PA12 | `TestMailPA12SMTPProbe` | LIVE read-only (owner-run, not CI) | Against the configured mailbox: TLS handshake, AUTH success, `MAIL FROM` = username accepted, then `RSET`/`QUIT` — **no DATA, nothing sent**; plus From ≠ username rejected by the server (records the reply code); SKIP = NOT_RUN. |
| PA13 | `docs/runbooks/mail.md` check | LIVE (owner approval required) | One code mail to the owner's own mailbox; headers show `spf=pass dkim=pass` (QQ: `qq.com` alignment; Exmail: `dmarc=pass` for `xgdwm.com`); arrival time recorded; screenshot in `output/merchant-password-auth/`. |
| PA14 | root review | REVIEW + regression | Security review (enumeration, timing, lockout, CSRF, cookie, log leaks); PROCESS §5 comments; `docs/engineering/dependencies.md` line for x/crypto; depmap regenerated; `go list -deps ./cmd/api` contains no `internal/mail/mailtest`; `go test -race ./...`, `go vet ./...`, `python3 scripts/check_packet.py`; existing T03/OIDC and browser-auth gates still green. |
| PA15 | deploy preflight/smoke rule | LIVE (owner-run deploy) | `dig +short $LC_ADMIN_HOST` resolves only to the server IP, not a Cloudflare range; otherwise preflight fails while `COMMERCE_PASSWORD_LOGIN_ENABLED=1` (ruling Q6). |

Each gate records one red run before its green run (PROCESS §2.4). No real email in CI: CI uses
the loopback fake; any non-loopback SMTP host in CI fails startup.

## 10. Ownership

| Artifact | Owner |
| --- | --- |
| `0070_merchant_password_auth.sql`, go.mod/go.sum (x/crypto), secrets manifest rows `commerce_auth_pepper` + `commerce_smtp_password` (with compose `secrets:` + `*_FILE` wiring), compose env, PA15 preflight rule, `tasks.json`, runbook sections `docs/runbooks/merchant-onboarding.md#unlock-password` and `#deactivate-principal` (explicit `<a id="…">`) | integrator |
| `internal/mail` (SMTP adapter + loopback fake `internal/mail/mailtest`) | integration_worker |
| `internal/identity` password/challenge/throttle/HIBP code, background sender, `mailcopy.go`, `internal/identityhttp` routes, `cmd/api/identity.go` config | commerce_worker |
| BFF routes, `PasswordAuth.tsx`, `entry-copy.ts`, `lib/auth.ts` config | ui_worker |
| PA03–PA11 tests | independent test_worker |
| PA14 | security_reviewer |

Sequence: freeze this file → 0070 + PA03 ‖ `internal/mail` + PA02 → identity service PA01/PA04–PA07
→ HTTP PA08/PA08b/PA09 → BFF + UI PA10/PA11 → PA14 → owner PA12/PA13/PA15 after the server and
the sending mailbox exist.

## 11. Known limits / NOT_RUN

- Evidence: DESIGN only; PA01–PA15 NOT_RUN. PA12/PA13 need the sending mailbox and its secret file.
- **The emailed code is not a second factor in NIST terms** (F4: email SHALL NOT be used for OOB
  authentication). It proves mailbox control; a mailbox compromise + password leak = account
  takeover, and reset needs only the mailbox. **Minimum length 12 is below NIST's 15 for
  single-factor passwords** — accepted gap (ruling Q2); the owner may raise it to 15 (one constant
  + copy change). Upgrade signal: first merchant with payouts or staff ⇒ add TOTP or passkeys.
- **Personal-mailbox transport:** the QQ daily/burst caps are UNKNOWN (F8); a provider rejection
  makes login answer 503 `mail_unavailable` for everyone until the provider window resets, and the
  provider cap is shared by login and unauthenticated mail (our split protects only our own
  budget). Upgrade signal: first `smtp_rejected` for frequency, or > 50 auth mails/day ⇒ Exmail
  (or a provider API behind the same interface). Mail from `@qq.com` may land in spam at non-QQ
  receivers; the UI says "check spam folder".
- **Lockout bounds:** an attacker who knows only the email can disable the credential with 100 wrong
  passwords (PD12, NIST cap; one source needs ~2.5 h under `email-pw`). From one source they can no longer stop the victim's reset: the per-email unauth mail bucket, supersede and reset budget are per source, and `ip-mail-unauth` keeps one source from exhausting `global-mail-unauth` (that needs ≥ ⌈0.4·cap/10⌉ sources = 8 IPv4 /32 or 4 IPv6 /48 at the default cap) (§6, PD4, PD5). **A distributed attacker (≥ 3 sources/day; an IPv6 /48 holds 65,536 /64
  sources) can still keep a disabled credential locked indefinitely** — exhausting
  `email-mail-unauth-total` (30/day), exhausting `global-mail-unauth`, crowding the 3 open-challenge slots, or the 50-wrong
  per-principal reset budget. Recovery = owner-approved operator runbook
  `docs/runbooks/merchant-onboarding.md#unlock-password`: migration-owner SQL in one transaction,
  `UPDATE identity.password_credentials SET disabled_at=NULL, failed_count=0 WHERE principal_id=$1`
  + `INSERT INTO identity.auth_events(principal_id, action) VALUES ($1,'operator.unlocked')`, run
  by Claude on the server (O-D) only after the owner approves the specific incident in chat.
  Unlock grants no access (password + emailed code are still required); it cannot help if the
  attacker keeps disabling. Upgrade signal: first incident ⇒ CAPTCHA on reset/login-failure.
  Sign-up has the same shape: one source can no longer block a new merchant's own sign-up (PD4
  same-source supersede, per-source mail bucket); a distributed attacker can.
- Users behind one shared IPv4 (CGNAT, office) share 10 sign-up/reset mails per day.
- **Mail budget shares:** unauthenticated traffic can exhaust `global-mail-unauth` for the day
  (sign-up/reset then 503). Accounts without a store can exhaust `global-mail-login-new` (their own
  logins then 503; a new merchant still gets a session directly from sign-up `complete`). Merchants
  with a store keep a login share that accounts without a store cannot use; a store-holding attacker
  needs ≥ ⌈0.45·cap/5⌉ accounts with stores (18 at the default cap; each needing a sign-up mail and
  onboarding, which stays behind `COMMERCE_ONBOARDING_ENABLED`) to exhaust it. The accounts persist,
  so after a one-time setup they can deny store members' login mail every UTC+8 day. Detection:
  `throttled` audit with bucket kind `global-mail-login`. Response: owner-approved operator
  deactivation of the offending principals (`identity.principals.active=false`; an inactive principal
  gets zero rows from `password_login_material`, so no login mail), runbook
  `docs/runbooks/merchant-onboarding.md#deactivate-principal` (sibling of `#unlock-password`).
  Upgrade signal: first `global-mail-login` exhaustion ⇒ onboarding allowlist or per-principal fair share.
- **At most 5 login codes per email per UTC+8 day** (`email-mail-login`): a 6th login that day gets
  429 until the next UTC+8 day. With `COMMERCE_SESSION_TTL=8h` (`deploy/env/api.env.example`) that
  covers normal use on two devices; raising it weakens the bound above linearly. Retries during a
  `global-mail-login*` 503 also consume it (§2); recovery that day is a reset.
- Background sign-up/reset sends are lost on a crash (`PENDING` remains); the user presses resend.
- **Open sign-up** (ruling Q7): anyone can create a principal; store creation stays behind
  `COMMERCE_ONBOARDING_ENABLED`, and a principal without a store has no Meta, Stripe or tenant
  access. Switching to an allowlist is a later owner decision.
- `equals_email` is not checked at reset (PD11). Internationalized (EAI/non-ASCII) email addresses
  are not accepted in v1.
- Changing Argon2 parameters later needs a new definer and a contract revision (no rehash-on-login
  in v1).
- No remember-this-device (every login sends a code, ruling Q4); no email change; no staff
  invitations; no operator-assisted recovery when the mailbox is lost (manual, NOT_IMPLEMENTED; the
  operator unlock above only clears `disabled_at`).
- No CAPTCHA/bot challenge; throttles and the global mail budget are the only abuse ceiling.
- No bounce handling; a bouncing address keeps failing quietly (signup/reset still answer 202).
- Per-IP limits are wrong behind a Cloudflare proxy; PA15 refuses that configuration.
- Exmail DMARC starts at `p=none`; tightening is an operational follow-up.
- Existing OIDC principals cannot add a password (PD9).

## 12. Owner questions (each has a default)

1. **Sending mailbox** (blocks PA12/PA13 only, not implementation): which QQ mailbox sends auth
   mail? **Default: a new dedicated QQ mailbox used only for platform sending** (its authorization
   code also opens its IMAP), display name `xgdwm`; the owner enables SMTP, generates the
   authorization code and places it as the `commerce_smtp_password` secret file on the server
   (O-D). Upgrade to Exmail `no-reply@xgdwm.com` when the upgrade signal in §11 fires.

2. **Residual targeted lockout** (blocks go-live acceptance, not implementation): accept that a
   distributed attacker (≥ 3 sources/day; ≥ 8 IPv4 /32 or 4 IPv6 /48 for exhausting `global-mail-unauth`) can keep one merchant's disabled credential locked, with
   recovery by the operator unlock runbook (§11)? **Default: accept; each unlock needs the owner's
   approval of that incident in chat; the first incident triggers a CAPTCHA contract.**

All other former questions (Q2–Q9) are decided by the integrator rulings in §14.

## 13. Review disposition (`output/contract-review/r2-design-wave.json` → `auth`)

Round 3 (`output/contract-review/r2-round3.json` → `auth`, verdict BLOCK; ruling X1: P1 applied with the reviewer's exact text, no further full review):

| # | Sev | Finding | Verification | Disposition |
| --- | --- | --- | --- | --- |
| R3-1 | P1 | One source exhausts `global-mail-unauth` (80 unauth mails to ≥ 8 arbitrary emails, ~45 min under `ip`), making every reset 503 → renewable one-source lockout; §11 claim false; PA07 unpassable | Confirmed: §6 had no per-source daily cap on unauth mail; unknown emails count (buckets hit before lookup) | Fixed with the reviewer's text: §6 `ip-mail-unauth` 10/day + `ip48-mail-unauth` 20/day; §11 lockout sentence replaced, `global-mail-unauth` added to the distributed list and §12 Q2; §11 shared-IPv4 limit; PA07 one-source case. §2 reset throttle list updated. |
| R3-2 | P2 | Persistent store-holding accounts deny members' login mail daily; no detection/response | Confirmed: open sign-up (Q7); `deploy/env/compose.env.example:60` `LC_ONBOARDING_ENABLED=1` → `COMMERCE_ONBOARDING_ENABLED` (`deploy/compose.yml:147`); `identity.principals.active` exists (0001 l.26) and `password_login_material` returns zero rows when inactive (§4.2) | Fixed with the reviewer's text (§11), runbook anchor made concrete (`#deactivate-principal`, §10). |
| R3-3 | P2 | `email-mail-login` consumed on a global-share 503; retries can exhaust the member's own daily budget | Confirmed: §2 order hits `email-mail-login` before `global-mail-login*` | Fixed with the reviewer's first option: not refunded, UI says wait (§2); §11 5-codes bullet; PA07 asserts the consumption. |

Round 2 (all three P1s verified against this file and `migrations/0001`/`0004`; none rebutted):

| # | Sev | Finding | Verification | Disposition |
| --- | --- | --- | --- | --- |
| R2-1 | P1 | 101st wrong password violates `CHECK (failed_count BETWEEN 0 AND 100)` → 5xx existence oracle; Argon2 on disabled unspecified | Confirmed: round-1 §4.1 CHECK + §4.2 `failed_count+1`; §2 did not say whether verify runs when `disabled` | Fixed with the reviewer's text: `LEAST(…,100)`, never raises (§4.2); §2 always verifies, disabled ⇒ 401; PA05 101st/150th case; PA08b disabled vs unknown; PA03 negative `failed_count` 101. |
| R2-2 | P1 | Round-1 #2 still open: one self-registered account drains `global-mail-login` | Confirmed: 10/hour × 24 = 240 > 120 default; no daily limit on `email-mail-login` | Fixed: `email-mail-login` 5/hour + 5/UTC+8 day; new `global-mail-login-new` 15 % for principals without an active membership, `global-mail-login` 45 % for members (§6). `has_membership` is read in `start_login_challenge` — verified the writer has SELECT on `identity.memberships` and `control.tenants` (0004 lines 45–48) and neither has RLS (0001 enables RLS only on `control.stores`, `ops.audit_events`). §11, PA07. Round-1 #2 "Fixed" was wrong and is superseded by this row. |
| R2-3 | P1 | Lockout renewable indefinitely (disable + daily `email-mail-unauth` + PD4 supersede + PD5) | Confirmed: every lockout primitive was keyed by email or principal only | Fixed with the reviewer's text: per-(email, source) unauth bucket + 30/day total (§6); `ip_hmac` on challenges, same-source supersede, ≤ 3 open (PD4, §4.2); PD5 per (principal, source) 10 + per principal 50, over-budget does not count; §11 honest residual + operator unlock runbook + `operator.unlocked` action; §12 Q2; PA06/PA07. |

Also checked (task: O-B): no Resend-specific assumption remains (idempotency key, region, free-tier
cap); `grep -i 'resend'` hits only the "Resend is not used"/rejected-alternative lines, the round-1 disposition, and the "resend" button (new-code recovery).

Round 1:

| # | Sev | Finding | Disposition |
| --- | --- | --- | --- |
| 1 | P1 | Lockout via reset codes / shared mail bucket | Fixed: PD5 reset-only; `email-mail-unauth`/`email-mail-login` split; PA07 victim-login case. Also fixed the sibling path the review missed: `email-pw` keyed per (email, source). |
| 2 | P1 | Global mail cap vs provider cap; 429 → UNKNOWN | Fixed under O-B: `global-mail-unauth` 40 % / `global-mail-login` 60 % of `COMMERCE_MAIL_DAILY_CAP` (default 200/day), fail closed 503; `ip48-signup`; SMTP final 4xx/5xx → FAILED (no UNKNOWN for a received reply). Resend-plan text removed. The login-share part was incomplete — superseded by R2-2. |
| 3 | P1 | Reset/sign-up timing oracle; 503 existence leak | Fixed: PD7 background send for signup/reset; every 503 raised before the existence lookup; unauth buckets hit before the lookup; PA08b. |
| 4 | P1 | Writer owns tables | Confirmed against 0004 (tables owned by the migration role, writer has explicit grants); fixed §4.1 heading + PA03 owner/column/TRUNCATE checks. |
| 5 | P1 | Exhausted signal in BFF | Fixed: cookie cleared on 200/409 only; Go never signals exhaustion; PA08/PA10. |
| 6 | P2 | `equals_email` at reset; HIBP/Argon2 before code check | Fixed: PD11, §7.1 order + 6-digit precheck, HIBP under PD3. |
| 7 | P2 | Rehash promise unimplemented | Fixed: removed; §11 note. |
| 8 | P2 | PD5 count index + race | Fixed stronger: count `attempts` on `email_challenges` (partial index), principal `FOR UPDATE` (0004 grants `UPDATE (id)` for this). |
| 9 | P2 | Collation-dependent `lower()` | Confirmed: no collation set in `deploy/compose.yml`/`postgresql.conf` (image default). Fixed: ASCII-only CHECK. |
| 10 | P2 | PD8 overstates protection | Confirmed (`issue_merchant_session(p_issuer,p_subject,…)` in 0004); PD8 amended. `dsn_lc_api_identity` already has its own manifest row. |
| 11 | P2 | Mail secret has no manifest row | Fixed for SMTP: `commerce_smtp_password opaque owner api` row, dedicated mailbox, rotation. |
| 12 | P2 | PA08 byte-identical; mailbox endpoint location | Fixed: PA08 comparison rule; `internal/mail/mailtest` (pattern `stripetest`) + PA14 `go list -deps` check. |
| 13 | P2 | Cloudflare proxy breaks per-IP buckets | Fixed: PA15 preflight rule (ruling Q6). |
| 14 | P2 | NIST 15 vs default 12 | Contradiction resolved by ruling Q2 (12): gap stated explicitly in PD11/§11; not changed to 15. |
| 15 | P2 | Open sign-up | Rebutted by ruling Q7 (open): a principal without a store has no Meta/Stripe/tenant access and store creation is behind `COMMERCE_ONBOARDING_ENABLED` (the reviewer's `LC_ONBOARDING_ENABLED` is actually `COMMERCE_ONBOARDING_ENABLED`, `cmd/api/identity.go`); recorded in §11. |

## 14. Integrator rulings (2026-09-29)

Round-3 rulings (2026-09-30): `docs/delivery/units/r2-design-rulings.md` "Round-3 integrator rulings" —
X1 (round-3 P1 applied with the reviewer's exact text, cheap P2s applied, §13 Round 3), X3 (below),
X6 (frozen at "v1 FROZEN 2026-09-30"; §12 owner questions keep their defaults and never block implementation).

Genuine owner inputs from the rulings file: none for this contract (they concern CVS, Stripe live,
billing and ads). Open owner questions here are only §12 Q1 (sending mailbox) and Q2 (residual
lockout acceptance), each with a default.

Source: `docs/delivery/units/r2-design-rulings.md` (binding). Accepted defaults the owner may
revise before go-live:

- **O-A** email + password + emailed code; OIDC optional. **O-B** generic SMTP (stdlib, 465),
  QQ mailbox first, Exmail recommended upgrade, no Resend; no idempotency key — one attempt after
  commit, resend = new code; global cap configurable (`COMMERCE_MAIL_DAILY_CAP`), default 200/day,
  split 40 % unauth / 15 % login without store / 45 % login with store (§6), fail closed with 503.
  **O-D** production operated by Claude; secrets as owner-supplied files, never in chat.
- **Q2** minimum password 12 (NIST gap recorded, §11). **Q3** HIBP k-anonymity on, fail-open.
  **Q4** code on every login, no remember-browser. **Q5** reset signs in + revokes other sessions.
  **Q6** admin host DNS-only (no Cloudflare proxy) until a proxy review (PA15). **Q7** open sign-up;
  store creation still behind `COMMERCE_ONBOARDING_ENABLED`. **Q8** 180 d audit / 2 d throttle.
  **Q9** 6-digit code.
- **R-1** accept `golang.org/x/crypto` (argon2id only). **R-2** send after commit, no queue —
  applied as: login awaits the send in the request; sign-up/reset send in a bounded detached
  goroutine after the 202 (required by round-1 P1 #3; still no queue, no persistence of the code).
  **X3 (2026-09-30):** this R-2 deviation (sign-up/reset send detached after the 202, login
  synchronous) is accepted — it closes the timing oracle. §12 Q2 residual distributed lockout: default
  accepted (operator unlock runbook; CAPTCHA contract on the first incident).
  **R-3** privilege-controlled tables like 0004, with the owner-role P1 fixed (tables owned by the
  migration role). **R-4** OIDC optional when password login is enabled. **R-5**
  `X-Commerce-Client-IP` trusted only after the BFF-key check. **R-6** migration 0071 released.

## 15. Lane-close amendments (2026-09-30, r2/auth closing pass; binding over the text above)

Findings: independent test F1 (`tests/foundation/password_finding_test.go`) and the round-1 review P2s
(`output/r2-implement-result.json` key `auth`). Each item names its gate.

- **A1 (F1, P2): one throttle key per window.** `PK (bucket, window_start)` has no window length, so two
  windows of one bucket that start at the same instant (60 s + 1 h in the first minute of an hour; 1 h + day in
  the first hour of a UTC+8 day) shared a row and every hit counted twice: a merchant's third login mail in that
  hour was refused and the day row kept them out for up to 23 h. Fix in the shared definer, so every caller is
  covered: `auth_throttle_hit` stores `sha256(p_bucket || int4send(window seconds) || int4send(offset))`. The column
  is still 32 bytes, the table shape, PK and signature are unchanged, and Go still passes the §6 bucket HMAC.
  Gates: `TestPasswordFindingF1ThrottleWindowsShareRows` (no longer behind the `finding` tag) and the PA07 hour/day
  row-count check after five login mails.
- **A2 (round-1 P2): `ip48` bucket on every step-1 and complete call.** IPv6 only, 60 / 15 min, hit right after
  `ip` (§6). Without it a holder of one /48 (65,536 /64s) could keep all PD3 slots busy with fresh random
  bindings (reset `complete`) or dummy-PHC verifies (login), each /64 staying under its own 30 / 15 min.
  Gate: PA07 subtest `ip48_60_per_15min_counts_every_step1_and_complete_call` (call 60 passes, 61 and 62 are 429,
  another /48 unaffected). Not added: a read-only "does the challenge exist" definer before hashing on reset.
- **A3 (round-1 P2): `throttled` and `password.breach_check_unavailable` are logged, not audited.** §4.2 has no
  definer that can write them and none is added: a throttled request is attacker-driven and high-volume, so an
  audit row per refused request would let one source bloat `auth_events`; HIBP outages are visible as the
  log line plus the fail-open count. The two actions stay in the `auth_events` CHECK for operator use. PA08 already
  accepts the log line.
- **A4 (test finding): the one-source guarantee needs `COMMERCE_MAIL_DAILY_CAP` >= 100.** At the minimum cap 20
  the `global-mail-unauth` share is 8, which one source (`ip-mail-unauth` 10/day, `ip48-mail-unauth` 20/day) can
  exhaust. The range stays 20-100000 (a private pilot may use a small cap); a public host runs >= 100 (share 40 >
  the per-/48 limit 20 plus one victim reset). The one-source PA07 scenario runs at the default cap 200.
- **A5 (test finding): private-wire statuses.** A body that fails the strict-JSON rules (unknown field, trailing
  JSON, `null`, wrong type) answers 400 `invalid_json`, the same as the existing identity handlers; 422 is for
  semantic errors (`password_policy`, invalid email, and `invalid_request` for a `complete` whose `new_password` does
  not match its purpose). merchant-browser-auth-v1's "invalid 422" is read as the semantic case.
- **A6 (note, no change): dead limit.** `email-mail-unauth`'s 10/day equals `ip-mail-unauth`'s and is hit by the same
  calls, so one source never observes it independently; it stays as defence in depth for a rotated `ip` key.
