// POST /api/auth/login → POST /v1/identity/login/start (internal/identityhttp); OIDC only, 404 without COMMERCE_OIDC_ISSUER (U4).
import {
  authConfig,
  disabledResponse,
  isLocale,
  type Locale,
  localError,
  privateIdentity,
  readBody,
  redirect,
  requireOrigin,
  safeJSON,
  setLoginCookie,
  validAuthorizationURL,
} from "@/lib/auth";

export async function POST(request: Request) {
  // U4: password-only deployments have no OIDC issuer; this route then does not exist.
  if (!authConfig?.issuer) return disabledResponse();
  if (new URL(request.url).search || !requireOrigin(request))
    return localError(403, "forbidden");
  let locale: Locale = "zh-CN";
  try {
    const form = new URLSearchParams(
      await readBody(request, "application/x-www-form-urlencoded", 1024),
    );
    const entries = [...form.entries()];
    if (
      entries.length !== 1 ||
      entries[0][0] !== "locale" ||
      !isLocale(entries[0][1])
    )
      return localError(422, "invalid_request");
    locale = entries[0][1] as Locale;
  } catch {
    return localError(422, "invalid_request");
  }
  const upstream = await privateIdentity("login/start", {});
  if (!upstream.ok)
    return upstream.status >= 500
      ? localError(503, "retry_later")
      : localError(upstream.status, "invalid_request");
  const body = await safeJSON<{
    authorization_url?: unknown;
    binding?: unknown;
    expires_at?: unknown;
  }>(upstream);
  const location = validAuthorizationURL(body?.authorization_url);
  if (!location || typeof body?.binding !== "string")
    return localError(503, "retry_later");
  const response = redirect(location);
  if (!setLoginCookie(response.headers, body.binding, locale, body.expires_at))
    return localError(503, "retry_later");
  return response;
}

export const GET = () =>
  authConfig?.issuer
    ? localError(405, "method_not_allowed", "POST")
    : disabledResponse();
export const PUT = GET;
export const DELETE = GET;
export const PATCH = GET;
export const OPTIONS = GET;
export const HEAD = GET;
