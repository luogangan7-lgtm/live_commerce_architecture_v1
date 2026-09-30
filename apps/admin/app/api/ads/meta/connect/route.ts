// BFF POST /api/ads/meta/connect -> Go POST /v1/admin/stores/{store_id}/ads/meta/connect (ads:manage + integration:manage).
// Starts the Meta "Facebook Login for Business" flow (contracts/meta-ads-v1.md §2 step 1): Go stores the hashed
// state and answers {state_id, dialog_url, expires_at}; this route validates that the dialog host is exactly
// https://www.facebook.com, binds the return to the store with the httpOnly cookie `lc_ads_connect`
// (Path=/api/ads/meta/callback, 10 min) and answers only {dialog_url}. It never sees or stores a Meta token.
import { callBackend, fixtureSession } from "@/lib/backend";
import {
  authConfig,
  authenticatedStores,
  clearAuthCookies,
  localError,
  readBody,
  requireCSRF,
  requireOrigin,
  safeError,
  sessionToken,
} from "@/lib/auth";
import { connectCookie, parseConnectBody, safeDialogURL, validIdempotencyKey } from "@/lib/ads-request";

export async function POST(request: Request) {
  if (request.url.includes("?")) return localError(422, "invalid_request");
  let token: string | undefined;
  let allowedStore: string | null = null;
  if (authConfig) {
    token = sessionToken(request) ?? undefined;
    if (!token) return localError(401, "unauthorized");
    if (!requireOrigin(request) || !requireCSRF(request)) return localError(403, "forbidden");
  } else {
    // Local fixture only: same loopback/origin rules as the generic store BFF.
    const session = fixtureSession();
    if (!session) return localError(401, "unauthorized");
    const host = request.headers.get("host");
    if ((host !== "127.0.0.1:3100" && host !== "localhost:3100") || request.headers.get("origin") !== `http://${host}`)
      return localError(403, "forbidden");
    allowedStore = session.storeID;
  }
  const key = request.headers.get("idempotency-key") ?? "";
  if (!validIdempotencyKey(key)) return localError(422, "invalid_request");
  let store: string | null;
  try {
    store = parseConnectBody(await readBody(request, "application/json", 1024));
  } catch {
    return localError(400, "invalid_json");
  }
  if (!store) return localError(422, "invalid_request");
  if (token) {
    const listed = await authenticatedStores(token);
    if (!listed.stores) {
      const denied = await safeError(listed.response);
      if (denied.status === 401) clearAuthCookies(denied.headers);
      return denied;
    }
    if (!listed.stores.some((item) => item.id === store)) return localError(404, "not_found");
  } else if (store !== allowedStore) return localError(404, "not_found");

  // Go POST ads/meta/connect is keyed (command.Run): a byte-identical retry with the same key returns the same state.
  const upstream = await callBackend(
    "ads/meta/connect",
    { method: "POST", headers: { "Idempotency-Key": key } }, // Go adsNoBody: 422 on any body
    token,
    store,
  );
  if (authConfig && upstream.status === 401) {
    const denied = await safeError(upstream);
    clearAuthCookies(denied.headers);
    return denied;
  }
  if (!upstream.ok) return safeError(upstream);
  let dialog: string | null = null;
  try {
    const body: unknown = await upstream.json();
    dialog = safeDialogURL(body && typeof body === "object" ? (body as Record<string, unknown>).dialog_url : null);
  } catch {
    dialog = null;
  }
  if (!dialog) return localError(503, "retry_later");
  return new Response(JSON.stringify({ dialog_url: dialog }), {
    status: 200,
    headers: {
      "Content-Type": "application/json",
      "Cache-Control": "no-store",
      "Referrer-Policy": "no-referrer",
      "Set-Cookie": connectCookie(store),
    },
  });
}

const unsupported = () => localError(405, "method_not_allowed", "POST");
export const GET = unsupported;
export const PUT = unsupported;
export const PATCH = unsupported;
export const DELETE = unsupported;
export const OPTIONS = unsupported;
export const HEAD = unsupported;
