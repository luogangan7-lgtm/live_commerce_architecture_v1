// GET /api/auth/callback → POST /v1/identity/login/complete (internal/identityhttp); OIDC only, 404 without COMMERCE_OIDC_ISSUER (U4).
import {
  authConfig,
  authFailure,
  clearLoginCookie,
  disabledResponse,
  loginBinding,
  privateIdentity,
  redirect,
  safeJSON,
  setSessionCookies,
} from "@/lib/auth";

function callbackQuery(request: Request) {
  const params = new URL(request.url).searchParams;
  const keys = [...params.keys()];
  if (keys.some((key) => !["state", "code", "iss", "error"].includes(key)))
    return null;
  if (
    ["state", "code", "iss", "error"].some(
      (key) => params.getAll(key).length > 1,
    )
  )
    return null;
  if (params.has("error")) return { error: true as const };
  const state = params.get("state") ?? "";
  const code = params.get("code") ?? "";
  const iss = params.get("iss");
  if (
    !state ||
    !code ||
    state.length > 2048 ||
    code.length > 4096 ||
    (iss !== null && (iss.length > 2048 || iss !== authConfig?.issuer))
  )
    return null;
  return { state, code };
}

export async function GET(request: Request) {
  if (!authConfig?.issuer) return disabledResponse(); // U4: no OIDC issuer => no callback
  const binding = loginBinding(request);
  const locale = binding?.locale ?? "zh-CN";
  const query = callbackQuery(request);
  if (!binding || !query || "error" in query) {
    const response = authFailure(locale);
    clearLoginCookie(response.headers);
    return response;
  }
  const upstream = await privateIdentity("login/complete", {
    state: query.state,
    binding: binding.binding,
    code: query.code,
  });
  if (!upstream.ok) {
    const response = authFailure(locale);
    clearLoginCookie(response.headers);
    return response;
  }
  const body = await safeJSON<{ token?: unknown; expires_at?: unknown }>(
    upstream,
  );
  const response = redirect(`/${locale}/`);
  clearLoginCookie(response.headers);
  if (!setSessionCookies(response.headers, body?.token, body?.expires_at)) {
    const failed = authFailure(locale);
    clearLoginCookie(failed.headers);
    return failed;
  }
  return response;
}

const unsupported = () =>
  authConfig?.issuer
    ? new Response(null, {
        status: 405,
        headers: { Allow: "GET", "Cache-Control": "no-store" },
      })
    : disabledResponse();
export const POST = unsupported;
export const PUT = unsupported;
export const DELETE = unsupported;
export const PATCH = unsupported;
export const OPTIONS = unsupported;
export const HEAD = unsupported;
