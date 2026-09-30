// Pure request logic for the merchant password BFF (contracts/merchant-password-auth-v1.md §7.2, unit
// auth-ui U1-U5). Used by app/api/auth/password/{signup,login,reset,verify}/route.ts, which forward to
// POST /v1/identity/password/{signup,login,reset,complete} (internal/identityhttp/password.go).
// Framework-free on purpose: no next/*, no lib/auth.ts (that is server-only), so node --test can
// import it. It never reads env, never logs, and never sees or returns a password or code except to
// hand the parsed value back to the calling route, which forwards it once and drops it.
import { isIP } from "node:net";

export const CHALLENGE_COOKIE = "__Host-commerce_challenge";
export type Purpose = "signup" | "login" | "reset";
type PasswordLocale = "zh-CN" | "zh-TW" | "en";

const purposes: readonly string[] = ["signup", "login", "reset"];
const locales: readonly string[] = ["zh-CN", "zh-TW", "en"];
const CHALLENGE_MAX_AGE = 600;
// Go binding = 32 random bytes, base64url unpadded (auth-core A1).
const BINDING = /^[A-Za-z0-9_-]{43}$/;
const challengeValue =
  /^([A-Za-z0-9_-]{43})\.(signup|login|reset)\.(zh-CN|zh-TW|en)$/;
// Go bounds the real policy (12-128 code points, email <= 254); these only cap what the BFF forwards.
const EMAIL_MAX = 320;
const SECRET_MAX = 1024;
const CODE_MAX = 32;

// Clears the challenge cookie (200 and 409 only, see clearsChallenge).
export const CLEAR_CHALLENGE_COOKIE = `${CHALLENGE_COOKIE}=; Path=/; Max-Age=0; Expires=Thu, 01 Jan 1970 00:00:00 GMT; Secure; HttpOnly; SameSite=Lax`;

function isPurpose(value: unknown): value is Purpose {
  return typeof value === "string" && purposes.includes(value);
}

function isLocale(value: unknown): value is PasswordLocale {
  return typeof value === "string" && locales.includes(value);
}

function exactKeys(record: Record<string, unknown>, keys: readonly string[]) {
  const actual = Object.keys(record).sort();
  const expected = [...keys].sort();
  return (
    actual.length === expected.length &&
    actual.every((key, i) => key === expected[i])
  );
}

function asRecord(body: unknown) {
  return body && typeof body === "object" && !Array.isArray(body)
    ? (body as Record<string, unknown>)
    : null;
}

function boundedString(value: unknown, max: number) {
  return typeof value === "string" && value.length > 0 && value.length <= max
    ? value
    : null;
}

// U2: exactly one X-Forwarded-For header whose whole value is one IP literal (no list, port,
// brackets or zone). Missing => 503 (Caddy always sets it; PD14/R-5); anything else malformed => 400.
// Callers pass [request.headers.get("x-forwarded-for")] minus nulls; Fetch joins duplicate headers
// with ", " so a duplicate arrives as one comma value and fails the literal check.
export function clientIPFromHeaders(
  values: string[],
): { ip: string } | { status: 400 | 503 } {
  if (values.length === 0) return { status: 503 };
  if (values.length !== 1) return { status: 400 };
  const value = values[0];
  if (value.includes("%") || isIP(value) === 0) return { status: 400 };
  return { ip: value };
}

// Strict step-1 body per §7.1: signup/login {email,password,locale}, reset {email,locale}.
export function parseStep1(
  purpose: Purpose,
  body: unknown,
): { email: string; password?: string; locale: PasswordLocale } | null {
  const record = asRecord(body);
  if (!record || !isPurpose(purpose)) return null;
  const withPassword = purpose !== "reset";
  if (
    !exactKeys(
      record,
      withPassword ? ["email", "password", "locale"] : ["email", "locale"],
    )
  )
    return null;
  const email = boundedString(record.email, EMAIL_MAX);
  if (!email || !isLocale(record.locale)) return null;
  if (!withPassword) return { email, locale: record.locale };
  const password = boundedString(record.password, SECRET_MAX);
  return password ? { email, password, locale: record.locale } : null;
}

// Strict verify body: {code} for signup/login, {code,new_password} for reset. Purpose comes from the
// challenge cookie, never the body (U5). The 6-digit rule is Go's (401 invalid_code before any work);
// the BFF only bounds the type so there is one rule, not two.
export function parseVerify(
  body: unknown,
  purpose: Purpose,
): { code: string; new_password?: string } | null {
  const record = asRecord(body);
  if (!record || !isPurpose(purpose)) return null;
  const reset = purpose === "reset";
  if (!exactKeys(record, reset ? ["code", "new_password"] : ["code"]))
    return null;
  const code = boundedString(record.code, CODE_MAX);
  if (!code) return null;
  if (!reset) return { code };
  const password = boundedString(record.new_password, SECRET_MAX);
  return password ? { code, new_password: password } : null;
}

// U5: Set-Cookie value `<binding>.<purpose>.<locale>`, Secure, HttpOnly, SameSite=Lax, Path=/,
// Max-Age = min(600, expires_at - now), no Domain. Returns "" when the binding, purpose, locale or
// expiry is unusable so the route answers 503 instead of setting a dead cookie.
export function challengeCookie(
  binding: string,
  purpose: Purpose,
  locale: string,
  expiresAt: string,
  now: Date,
): string {
  if (!BINDING.test(binding) || !isPurpose(purpose) || !isLocale(locale))
    return "";
  const remaining = Math.floor((Date.parse(expiresAt) - now.getTime()) / 1000);
  if (!Number.isFinite(remaining) || remaining <= 0) return "";
  const age = Math.min(remaining, CHALLENGE_MAX_AGE);
  return `${CHALLENGE_COOKIE}=${binding}.${purpose}.${locale}; Path=/; Max-Age=${age}; Secure; HttpOnly; SameSite=Lax`;
}

// Exactly one challenge cookie with a well-formed value; a duplicate name or any other shape is null.
export function parseChallengeCookie(
  cookieHeader: string | null,
): { binding: string; purpose: Purpose; locale: string } | null {
  const values: string[] = [];
  for (const part of (cookieHeader ?? "").split(";")) {
    const at = part.indexOf("=");
    if (at < 0 || part.slice(0, at).trim() !== CHALLENGE_COOKIE) continue;
    values.push(part.slice(at + 1).trim());
  }
  if (values.length !== 1) return null;
  const match = challengeValue.exec(values[0]);
  return match
    ? { binding: match[1], purpose: match[2] as Purpose, locale: match[3] }
    : null;
}

// Clears only on 200 (success) and 409 (account_exists). Every 401 keeps the cookie: Go never signals
// exhaustion, so the cookie simply expires through Max-Age <= 600 and a wrong code can be retried.
export function clearsChallenge(status: number): boolean {
  return status === 200 || status === 409;
}
