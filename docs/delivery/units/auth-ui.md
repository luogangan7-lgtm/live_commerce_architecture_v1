# Unit auth-ui — admin password BFF routes, `PasswordAuth` sign-in/sign-up/reset pages, copy

Role: ui_worker (mid tier). Base = `00c1d94` + integrator F0, SHA recorded at dispatch. Worktree
`.worktrees/auth-ui`, branch `unit/auth-ui`. No delegation, no new dependency, no lockfile change.
Contract: `contracts/merchant-password-auth-v1.md` (v1 FROZEN 2026-09-30) §2, §5 (admin BFF paragraph),
§6 (only the user-visible codes), §7.1 (private wire), §7.2, §7.3, §9 PA10/PA11 (what the gates will
assert), §11 (UI-visible limits). Private Go wire is frozen in `docs/delivery/units/auth-core.md`
(FROZEN block + A10). Runs parallel with auth-core against that frozen wire.

**Goal:** a merchant can sign up, sign in and reset from the admin host with email + password + an
emailed code, in 3 locales, without the BFF ever echoing, logging or storing the password or code.

## Read (by section)
PROCESS.md; the contract sections above; `contracts/merchant-browser-auth-v1.md` cookie/CSRF/Origin
rules. Code by symbol: `apps/admin/lib/auth.ts` (`readConfig`, `authConfig`, `requireOrigin`,
`hasNoQuery`, `readBody`, `exactJSON`, `privateIdentity`, `safeError`, `setSessionCookies`,
`cookie`, `maxAge`, `localError`, `redirect`), `apps/admin/app/api/auth/{login,callback,logout}/route.ts`,
`apps/admin/components/Entry.tsx` (signed-out block ~l.360–400), `apps/admin/lib/{entry-copy,entry-state}.ts`,
`apps/admin/app/[locale]/page.tsx`, `app/globals.css` tokens (read only), `tests/admin/{auth,entry}.spec.ts`
(must stay green).

## Defaults adopted
- U1 Pure, framework-free request logic lives in a new `apps/admin/lib/password-request.ts` (Node-test
  importable, like `orders-request.ts`); routes are thin wrappers.
- U2 Client IP: exactly one `X-Forwarded-For` header whose whole value is one IP literal (no list, no
  port, no zone) else 400 `invalid_request`; missing ⇒ 503 `retry_later` (Caddy always sets it;
  PD14/R-5). Forwarded as `X-Commerce-Client-IP` on the BFF-key call only.
- U3 `privateIdentity` gains an optional `timeoutMs` (default 6000); password `login` uses 14000
  (auth-core A6 sends ≤ 10 s); others keep 6000. A BFF timeout ⇒ 503 `retry_later`; never retried.
- U4 `readConfig` with `COMMERCE_PASSWORD_LOGIN_ENABLED=1` makes `COMMERCE_OIDC_ISSUER` optional
  (`issuer: string | null`); exported `readAuthConfig(env)` for PA10; `/api/auth/login|callback`
  answer 404 when `issuer` is null. The OIDC button renders only when `issuer` is set.
- U5 Challenge cookie value `<binding>.<purpose>.<locale>`, `Max-Age = min(600, expires_at − now)`;
  verify reads purpose/locale from the cookie, never from the body.
- U6 Pages: `/[locale]/` signed-out state renders `PasswordAuth mode="signin"` when password login is
  on (else today's OIDC entry); `/[locale]/signup`, `/[locale]/reset` render the other modes; a valid
  session on those pages redirects to `/<locale>/`. Masked email = first char + `***` + `@domain`.
- U7 `throttled` shows minutes = ceil(`Retry-After`/60); `mail_unavailable` on login says "wait, do not
  retry now" (§2 round-3 P2); resend disabled 60 s, re-posts step 1 with values held in component state
  only (never storage).

## FROZEN TS interface (auth-tests' PA10 imports exactly these)
```ts
// apps/admin/lib/password-request.ts — no next/* imports
export const CHALLENGE_COOKIE = "__Host-commerce_challenge";
export type Purpose = "signup" | "login" | "reset";
export function clientIPFromHeaders(values: string[]): { ip: string } | { status: 400 | 503 } // U2
export function parseStep1(purpose: Purpose, body: unknown):
  { email: string; password?: string; locale: "zh-CN" | "zh-TW" | "en" } | null  // strict keys per §7.1
export function parseVerify(body: unknown, purpose: Purpose): { code: string; new_password?: string } | null
export function challengeCookie(binding: string, purpose: Purpose, locale: string, expiresAt: string, now: Date): string // Set-Cookie value, U5
export function parseChallengeCookie(cookieHeader: string | null): { binding: string; purpose: Purpose; locale: string } | null // exactly one
export function clearsChallenge(status: number): boolean // true only for 200 and 409
// apps/admin/lib/auth.ts
export function readAuthConfig(env: Record<string, string | undefined>): AuthConfig | null // U4
```
Routes (§7.2): `apps/admin/app/api/auth/password/{signup,login,reset,verify}/route.ts`, POST only,
exact Origin, no query, strict JSON; step 1 → 202 `{step:"code",expires_at}`; verify → 200
`{redirect:"/<locale>/"}` via existing `setSessionCookies`; a failed verify keeps a valid existing
session.

## Build
1. `password-request.ts` + `auth.ts` changes (U2–U5) + the four routes. Password/code never in a
   response, log line, URL or error message.
2. `components/PasswordAuth.tsx` (client) — modes `signin|signup|reset` + `code` step; fields/
   autocomplete/`inputmode` exactly per §7.3; reuse the `Entry.tsx` shell, `globals.css` tokens and
   existing form/button classes (audit-first: no new visual language, no new tokens).
3. Pages `app/[locale]/signup/page.tsx`, `app/[locale]/reset/page.tsx`; `Entry.tsx` + `[locale]/page.tsx`
   switch per U6.
4. Copy: every string in `lib/entry-copy.ts` for zh-CN/zh-TW/en (list in §7.3 + `busy`, `retry_later`,
   `invalid_email`, `password_policy.{too_short,too_long,breached,equals_email}`, "check spam folder");
   sign-up/reset wording never confirms existence.

Every new/edited route/component file starts with the PROCESS §5 comment naming the BFF route(s) it
calls and the Go endpoint behind them (e.g. `// POST /api/auth/password/login → POST
/v1/identity/password/login (internal/identityhttp/password.go)`).

## Write paths
`apps/admin/app/api/auth/password/**`, `apps/admin/app/[locale]/{signup,reset}/page.tsx`,
`apps/admin/app/[locale]/page.tsx`, `apps/admin/components/{PasswordAuth.tsx,Entry.tsx}`,
`apps/admin/lib/{password-request.ts,auth.ts,entry-copy.ts,entry-state.ts}`,
`apps/admin/app/api/auth/{login,callback}/route.ts` (U4 404 only), `output/auth-ui/**`.
Forbidden: Go, SQL, contracts, `tests/**` (PA10/PA11 are auth-tests'), `globals.css`, `next.config.ts`,
admin nav/`WorkspaceFrame.tsx`, lockfiles, new packages, other R2 units' pages.

## Verify
```sh
pnpm typecheck:admin && pnpm build:admin
bash scripts/dev/test-local.sh --browser-identity        # OIDC regression (auth.spec/entry flows) after M2
bash scripts/dev/test-local.sh --browser-admin-legacy    # entry-mock regression
```
Record command, SHA, exit, counts → `/Volumes/data/live_commerce_architecture_v1/output/auth-ui/`;
screenshots 1586×992 + 390px × 3 locales × {signin, signup, code, reset, throttled} for the owner's
visual approval (merchant-browser-auth-v1: pending).

## Order / Non-goals / Return
Author at dispatch against the frozen wire; real-chain run after M2 (auth-core + auth-mail merged).
Merges in the same integration batch as auth-core. No OIDC removal, no account settings/password
change page, no remember-device, no CAPTCHA, no client-side throttling authority.
Return commit SHA, model/reasoning, base, paths, commands + exits + counts, screenshots path, how
U1–U7 were applied, risks, NOT_RUN. Any deviation from the frozen TS/wire = stop and escalate.
