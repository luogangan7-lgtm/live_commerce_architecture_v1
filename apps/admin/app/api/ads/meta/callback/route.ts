// BFF GET /api/ads/meta/callback -> Go GET /v1/admin/stores/{store_id}/ads/meta/callback?code&state (session principal +
// store must equal the state row's). This is the redirect URI registered on the Meta app (owner step, contract §11).
// Go consumes the state and exchanges the code server-to-server (D6: answers 200 {state_id}); this route turns that
// into a 303 to /<locale>/ads?store=..&connect=<state_id> (or &connect_error=<fixed code>).
// Secrets: `code`/`state` are one-time credentials. They are length/charset-bounded, forwarded only to Go, never
// logged, never echoed into any URL or body; responses are no-store + no-referrer and the binding cookie is cleared.
// Operators must also exclude this path from access logs (Caddy; integrator hook).
import { callBackend } from "@/lib/backend";
import { authConfig, redirect, sessionToken } from "@/lib/auth";
import {
  callbackRedirect, clearConnectCookie, connectErrorFor, localeFromCookie, parseCallbackQuery, storeFromCookie,
} from "@/lib/ads-request";

function done(location: string) {
  const response = redirect(location);
  response.headers.set("Referrer-Policy", "no-referrer");
  response.headers.append("Set-Cookie", clearConnectCookie());
  return response;
}

export async function GET(request: Request) {
  const locale = localeFromCookie(request.headers.get("cookie"));
  const store = storeFromCookie(request.headers.get("cookie"));
  const query = parseCallbackQuery(new URL(request.url).search);
  // No store binding cookie: the return did not start in this browser (or it expired) -> never guess a store.
  if (!store) return done(callbackRedirect(locale, null, { error: "state_mismatch" }));
  if (query.kind === "denied") return done(callbackRedirect(locale, store, { error: "denied" }));
  if (query.kind === "invalid") return done(callbackRedirect(locale, store, { error: "invalid_request" }));
  let token: string | undefined;
  if (authConfig) {
    token = sessionToken(request) ?? undefined;
    if (!token) return done(callbackRedirect(locale, store, null)); // signed out: the page shows its sign-in state
  }
  const params = new URLSearchParams({ code: query.code, state: query.state });
  // Single attempt on purpose: the state is single-use, so an ambiguous failure must restart the flow, not retry.
  const upstream = await callBackend(`ads/meta/callback?${params}`, { method: "GET" }, token, store);
  let body: unknown = null;
  try {
    body = await upstream.json();
  } catch {
    body = null;
  }
  if (upstream.ok) {
    const id = body && typeof body === "object" ? (body as Record<string, unknown>).state_id : null;
    if (typeof id === "string") {
      const target = callbackRedirect(locale, store, { connect: id });
      if (target.includes("connect=")) return done(target);
    }
    return done(callbackRedirect(locale, store, { error: "unavailable" }));
  }
  const error = connectErrorFor(upstream.status, body);
  return done(callbackRedirect(locale, store, error ? { error } : null));
}

const unsupported = () =>
  new Response(null, { status: 405, headers: { Allow: "GET", "Cache-Control": "no-store", "Referrer-Policy": "no-referrer" } });
export const POST = unsupported;
export const PUT = unsupported;
export const PATCH = unsupported;
export const DELETE = unsupported;
export const OPTIONS = unsupported;
export const HEAD = unsupported;
