// BFF request grammar for the merchant ads page: generic route `/api/stores/{store}/ads/*`
// -> Go `/v1/admin/stores/{store_id}/ads/*` (internal/httpapi/ads.go), plus the pure helpers of the two dedicated
// Meta-connect routes `/api/ads/meta/connect` (POST -> Go POST ads/meta/connect) and `/api/ads/meta/callback`
// (GET -> Go GET ads/meta/callback). Contract: contracts/meta-ads-v1.md §2, §7; docs/delivery/units/ads-ui.md U1/U2.
// Non-goals: no authentication, CSRF or store authority (route handlers + Go decide), no draft/domain validation
// (Go decides). Pure and dependency-free so proxy/route code and node:test share one grammar.

const uuid = "[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}";

// Relative to `stores/{store_id}/`, per method: exactly the Frozen HTTP resources except meta/connect and
// meta/callback (those two are the dedicated routes). Fragments, spliced into the `[...resource]` method table
// like `claimsRoutes`; the BFF forwards each path unchanged.
export const adsRoutes = {
  GET: `ads/(?:settings|report|drafts(?:/${uuid})?|meta/states/${uuid})`,
  POST: `ads/(?:meta/bindings|drafts|drafts/${uuid}/(?:approve|publish|pause|end))`,
  PUT: `ads/(?:drafts/${uuid}|capi)`,
} as const;
export const adsAny = new RegExp(`^(?:${adsRoutes.GET}|${adsRoutes.POST}|${adsRoutes.PUT})$`);
// Go ads.go transport: pause/end are bodiless (adsNoBody -> 422 on any body), approve is keyless (adsRoute keyed=false ->
// 422 on an Idempotency-Key). The page still posts `{}` + a key through the generic JSON BFF; the BFF drops them for these.
export const adsBodyless = new RegExp(`^ads/drafts/${uuid}/(?:pause|end)$`);
export const adsKeyless = new RegExp(`^ads/drafts/${uuid}/approve$`);
const reportRoute = /^ads\/report$/;

/** Raw-URL query rule, checked before Next drops an empty '?': only `ads/report` has a query. */
export function validAdsQuery(rawURL: string, path: string): boolean {
  const at = rawURL.indexOf("?");
  if (at < 0) return !reportRoute.test(path); // report without from/to is not a valid request
  if (!reportRoute.test(path)) return false;
  const seen = new Map<string, string>();
  for (const segment of rawURL.slice(at + 1).split("&")) {
    const m = /^(from|to)=([0-9]{4}-[0-9]{2}-[0-9]{2})$/.exec(segment);
    if (!m || seen.has(m[1])) return false;
    seen.set(m[1], m[2]);
  }
  const from = seen.get("from");
  const to = seen.get("to");
  return from !== undefined && to !== undefined && windowOK(from, to);
}
function windowOK(from: string, to: string): boolean {
  const day = (v: string) => {
    const [y, m, d] = v.split("-").map(Number);
    const t = new Date(Date.UTC(y, m - 1, d));
    return t.getUTCFullYear() === y && t.getUTCMonth() === m - 1 && t.getUTCDate() === d ? t.getTime() : NaN;
  };
  const a = day(from);
  const b = day(to);
  const span = Math.round((b - a) / 86_400_000);
  return Number.isFinite(span) && span >= 0 && span + 1 <= 92; // §7: at most 92 days (inclusive count)
}

/** PUT drafts/{id} needs `If-Match: <revision>` (a bare decimal revision; the BFF must forward it). */
export const validIfMatch = (value: string | null) => value !== null && /^(?:0|[1-9][0-9]{0,9})$/.test(value);
export const validIdempotencyKey = (value: string) => /^[A-Za-z0-9_.:-]{8,128}$/.test(value);

// ---------- U1 connect BFF ----------
export const connectCookieName = "lc_ads_connect";
export const connectCookiePath = "/api/ads/meta/callback";
const cookieAttrs = `Path=${connectCookiePath}; Secure; HttpOnly; SameSite=Lax`;
/** Binds the callback to the store the merchant started from (10 min = the state TTL). Lax so the top-level return from Meta carries it. */
export const connectCookie = (store: string) => `${connectCookieName}=${store}; ${cookieAttrs}; Max-Age=600`;
export const clearConnectCookie = () => `${connectCookieName}=; ${cookieAttrs}; Max-Age=0; Expires=Thu, 01 Jan 1970 00:00:00 GMT`;

/** The only body the connect BFF accepts: `{"store":"<uuid>"}`. */
export function parseConnectBody(text: string): string | null {
  try {
    const value: unknown = JSON.parse(text);
    if (!value || typeof value !== "object" || Array.isArray(value)) return null;
    const keys = Object.keys(value);
    const store = (value as Record<string, unknown>).store;
    return keys.length === 1 && typeof store === "string" && new RegExp(`^${uuid}$`).test(store) ? store : null;
  } catch {
    return null;
  }
}

// The page navigates to the dialog only when its origin is exactly https://www.facebook.com (never a server-chosen host).
export const dialogOrigin = "https://www.facebook.com";
export function safeDialogURL(value: unknown): string | null {
  if (typeof value !== "string" || value.length > 4096) return null;
  try {
    const url = new URL(value);
    return url.origin === dialogOrigin && !url.username && !url.password && !url.hash ? url.toString() : null;
  } catch {
    return null;
  }
}

// ---------- U2 callback BFF ----------
export const callbackCodeMax = 2048;
export type CallbackQuery = { kind: "ok"; code: string; state: string } | { kind: "denied" } | { kind: "invalid" };
/**
 * Bound `code`/`state` before they reach Go. Meta appends `error*` params when the merchant cancels (-> denied).
 * Duplicates and out-of-charset values are invalid. Nothing here logs or echoes the values; unknown extra keys
 * are ignored and never forwarded.
 */
export function parseCallbackQuery(search: string): CallbackQuery {
  const params = new URLSearchParams(search);
  if (["code", "state", "error"].some((k) => params.getAll(k).length > 1)) return { kind: "invalid" };
  if (params.has("error")) return { kind: "denied" };
  const code = params.get("code") ?? "";
  const state = params.get("state") ?? "";
  if (!/^[A-Za-z0-9._~-]{1,2048}$/.test(code) || !/^[A-Za-z0-9_-]{16,128}$/.test(state)) return { kind: "invalid" };
  return { kind: "ok", code, state };
}
export const canonicalStore = (value: string | null | undefined) => !!value && new RegExp(`^${uuid}$`).test(value);
/** Store from the `lc_ads_connect` cookie, exactly one copy, canonical uuid, else null (-> state_mismatch). */
export function storeFromCookie(header: string | null): string | null {
  const found: string[] = [];
  for (const part of (header ?? "").split(";")) {
    const at = part.indexOf("=");
    if (at >= 0 && part.slice(0, at).trim() === connectCookieName) found.push(part.slice(at + 1).trim());
  }
  return found.length === 1 && canonicalStore(found[0]) ? found[0] : null;
}
const locales = ["zh-CN", "zh-TW", "en"] as const;
export type AdsLocale = (typeof locales)[number];
/** Existing `commerce_locale` cookie value, default zh-TW (U2). */
export function localeFromCookie(header: string | null): AdsLocale {
  for (const part of (header ?? "").split(";")) {
    const at = part.indexOf("=");
    if (at >= 0 && part.slice(0, at).trim() === "commerce_locale") {
      const v = part.slice(at + 1).trim();
      if ((locales as readonly string[]).includes(v)) return v as AdsLocale;
    }
  }
  return "zh-TW";
}
/** Callback 303 target: only fixed codes and canonical uuids ever appear in the URL (never code/state). */
export function callbackRedirect(locale: AdsLocale, store: string | null, result: { connect: string } | { error: string } | null): string {
  const query: string[] = [];
  if (store) query.push(`store=${store}`);
  if (result && "connect" in result && new RegExp(`^${uuid}$`).test(result.connect)) query.push(`connect=${result.connect}`);
  else if (result && "error" in result && /^[a-z_]{1,40}$/.test(result.error)) query.push(`connect_error=${result.error}`);
  return `/${locale}/ads${query.length ? `?${query.join("&")}` : ""}`;
}
/** Go status -> the fixed `connect_error` value (D6 codes); null = signed out (the page shows its own sign-in state). */
export function connectErrorFor(status: number, body: unknown): string | null {
  const o = body && typeof body === "object" ? (body as Record<string, unknown>) : {};
  const raw = typeof o.code === "string" ? o.code : typeof o.error === "string" ? o.error : "";
  if (status === 401) return null;
  if (raw === "state_mismatch" || raw === "state_expired" || raw === "meta_connect_failed") return raw;
  if (status === 403 || status === 404) return "forbidden";
  if (status === 409) return "state_mismatch";
  if (status === 410) return "state_expired";
  if (status === 502) return "meta_connect_failed";
  if (status === 400 || status === 422) return "invalid_request";
  return "unavailable";
}
