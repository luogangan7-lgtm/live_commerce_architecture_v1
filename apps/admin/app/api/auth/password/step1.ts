// Shared body of POST /api/auth/password/{signup,login,reset} and /verify (merchant-password-auth-v1
// §7.2). Go endpoints behind them: POST /v1/identity/password/{signup,login,reset,complete}
// (internal/identityhttp/password.go), reached only through privateIdentity with the BFF key.
// It never stores, echoes or logs the password or code: both are parsed, forwarded once and dropped,
// and every error body is rebuilt locally from an allow-listed code (never the upstream message).
// It never retries: a repeated POST would burn throttles and mail budget (auth-ui U3).
import {
  authConfig,
  disabledResponse,
  hasNoQuery,
  localError,
  passwordLoginOn,
  privateIdentity,
  readBody,
  requireOrigin,
  safeJSON,
  setSessionCookies,
} from "@/lib/auth";
import {
  CLEAR_CHALLENGE_COOKIE,
  challengeCookie,
  clearsChallenge,
  clientIPFromHeaders,
  parseChallengeCookie,
  parseStep1,
  parseVerify,
  type Purpose,
} from "@/lib/password-request";

const upstreamCodes = new Set([
  "invalid_request",
  "invalid_credentials",
  "invalid_code",
  "account_exists",
  "invalid_email",
  "password_policy",
  "throttled",
  "busy",
  "mail_unavailable",
]);
const policyReasons = new Set([
  "too_short",
  "too_long",
  "breached",
  "equals_email",
]);
// Statuses Go may legitimately answer on these routes (§7.1); anything else is a BFF fault => 503.
const upstreamStatuses = new Set([400, 401, 409, 422, 429, 503]);

const nostore = { "Cache-Control": "no-store" };

// Common guards: 404 when off, exact Origin (anonymous, like /api/auth/login), no query, client IP.
function guard(request: Request) {
  if (!passwordLoginOn()) return { response: disabledResponse() };
  if (!hasNoQuery(request) || !requireOrigin(request))
    return { response: localError(403, "forbidden") };
  // U2: Caddy always sets X-Forwarded-For; it is forwarded on the BFF-key call only.
  const header = request.headers.get("x-forwarded-for");
  const ip = clientIPFromHeaders(header === null ? [] : [header]);
  if ("status" in ip)
    return {
      response:
        ip.status === 400
          ? localError(400, "invalid_request")
          : localError(503, "retry_later"),
    };
  return { ip: ip.ip };
}

async function jsonBody(request: Request): Promise<unknown> {
  try {
    return JSON.parse(await readBody(request, "application/json"));
  } catch {
    return undefined;
  }
}

// Rebuilds a safe error from Go's answer: allow-listed status and code, bounded Retry-After, and the
// single extra key `reason` for password_policy (A10). Unknown shapes become 503 retry_later.
async function passwordError(upstream: Response) {
  if (!upstreamStatuses.has(upstream.status))
    return localError(503, "retry_later");
  const body = await safeJSON<{
    code?: unknown;
    details?: { reason?: unknown };
    reason?: unknown;
  }>(upstream);
  const code = typeof body?.code === "string" ? body.code : "";
  if (!upstreamCodes.has(code)) return localError(503, "retry_later");
  const reason = body?.reason ?? body?.details?.reason;
  const details: Record<string, string> =
    code === "password_policy" &&
    typeof reason === "string" &&
    policyReasons.has(reason)
      ? { reason }
      : {};
  const safe = localError(upstream.status, code, undefined, details);
  const retryAfter = upstream.headers.get("retry-after") ?? "";
  if (
    upstream.status === 429 &&
    /^(?:[1-9][0-9]{0,3})$/.test(retryAfter) &&
    Number(retryAfter) <= 86400
  )
    safe.headers.set("Retry-After", retryAfter);
  return safe;
}

export async function handleStep1(request: Request, purpose: Purpose) {
  const checked = guard(request);
  if (checked.response) return checked.response;
  const parsed = parseStep1(purpose, await jsonBody(request));
  if (!parsed) return localError(422, "invalid_request");
  const upstream = await privateIdentity(
    `password/${purpose}`,
    parsed,
    undefined,
    undefined,
    { clientIP: checked.ip, timeoutMs: purpose === "login" ? 14000 : 6000 },
  );
  if (upstream.status !== 202) return passwordError(upstream);
  const body = await safeJSON<{ binding?: unknown; expires_at?: unknown }>(
    upstream,
  );
  const cookie =
    typeof body?.binding === "string" && typeof body.expires_at === "string"
      ? challengeCookie(
          body.binding,
          purpose,
          parsed.locale,
          body.expires_at,
          new Date(),
        )
      : "";
  if (!cookie || typeof body?.expires_at !== "string")
    return localError(503, "retry_later");
  const response = Response.json(
    { step: "code", expires_at: body.expires_at },
    { status: 202, headers: nostore },
  );
  response.headers.append("Set-Cookie", cookie);
  return response;
}

export async function handleVerify(request: Request) {
  const checked = guard(request);
  if (checked.response) return checked.response;
  // Purpose and locale come from the cookie, never the body (U5). Missing/duplicate => start over.
  const challenge = parseChallengeCookie(request.headers.get("cookie"));
  if (!challenge) return localError(422, "invalid_request");
  const parsed = parseVerify(await jsonBody(request), challenge.purpose);
  if (!parsed) return localError(422, "invalid_request");
  const upstream = await privateIdentity(
    "password/complete",
    { binding: challenge.binding, purpose: challenge.purpose, ...parsed },
    undefined,
    undefined,
    { clientIP: checked.ip },
  );
  if (upstream.status !== 200) {
    const failed = await passwordError(upstream);
    // A failed verify never touches an existing session; only 409 drops the challenge (Go never
    // signals exhaustion, so 401 keeps it until Max-Age).
    if (clearsChallenge(failed.status))
      failed.headers.append("Set-Cookie", CLEAR_CHALLENGE_COOKIE);
    return failed;
  }
  const body = await safeJSON<{ token?: unknown; expires_at?: unknown }>(
    upstream,
  );
  const response = Response.json(
    { redirect: `/${challenge.locale}/` },
    { status: 200, headers: nostore },
  );
  if (!setSessionCookies(response.headers, body?.token, body?.expires_at))
    return localError(503, "retry_later");
  response.headers.append("Set-Cookie", CLEAR_CHALLENGE_COOKIE);
  return response;
}

// Method stubs shared by the four route files.
export const unsupported = () =>
  authConfig?.passwordLogin
    ? localError(405, "method_not_allowed", "POST")
    : disabledResponse();
