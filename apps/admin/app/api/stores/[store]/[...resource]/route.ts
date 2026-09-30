import { callBackend, fixtureSession } from "@/lib/backend";
import {
  orderActionRoute, validCSVHeaders, validKeylessCommandRequest, validKeylessRequest, validOrdersQuery,
} from "@/lib/orders-request";
import { customersRoute, validCustomersBody, validCustomersRequest } from "@/lib/customers-request";
import { logisticsRoute } from "@/lib/logistics-request";
import { validStudioInputToken, validStudioQuery } from "@/lib/studio-request";
import {
  claimLinkRoute, claimsCollection, claimsRoutes, claimsSubpath, validClaimLink,
} from "@/lib/claims-request";
import { adsAny, adsBodyless, adsKeyless, adsRoutes, validAdsQuery, validIfMatch } from "@/lib/ads-request";
import { parseStudioInput, parseStudioInputPrepared } from "@/lib/studio-model";
import {
  authConfig,
  authenticatedStores,
  clearAuthCookies,
  localError,
  requireCSRF,
  requireOrigin,
  readBody,
  safeError,
  sessionToken,
} from "@/lib/auth";

const uuid = "[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}";
const account = `provider-accounts(?:/${uuid})?`;
const setting = `markets/${uuid}/countries/[A-Z]{2}/(?:delivery-services|payment-methods)/[a-z][a-z0-9_-]{0,39}`;
const inspect = `markets/${uuid}/countries/[A-Z]{2}/payment-methods/[a-z][a-z0-9_-]{0,39}/inspect`;
const deliveryCollection = `markets/${uuid}/countries/[A-Z]{2}/delivery-services`;
const paymentCollection = `markets/${uuid}/countries/TW/payment-methods`;
const policy = `${deliveryCollection}/[a-z][a-z0-9_-]{0,39}/policy`;
const purchaseEntry = `products/${uuid}/purchase-entry`;
const orders = `orders(?:/${uuid})?`;
const studioDetail = `live-sessions/${uuid}`;
const studioInput = `${studioDetail}/input(?:/(?:start|token|prepared))?`;
const studioInputRead = `${studioDetail}/input(?:/prepared)?`;
const studioInputReadRoute = new RegExp(`^${studioInputRead}$`);
const studioInputRoute = new RegExp(`^${studioInput}$`);
const studioAction = `${studioDetail}/(?:rehearsal/(?:start|stop)|input/(?:start|token))`;
const studioAny = new RegExp(`^(?:live-sessions|${studioDetail}|${studioAction}|${studioInputRead}|${studioDetail}/${claimsSubpath})$`);
const routes: Record<string, RegExp> = {
  GET: new RegExp(
    `^(catalog-ledger|products|warehouses|inventory|products/${uuid}/skus|${purchaseEntry}|${account}|${setting}|markets|${deliveryCollection}|${paymentCollection}|${policy}|${orders}|live-sessions|${studioDetail}|${studioInputRead}|${claimsRoutes.GET}|${adsRoutes.GET})$`,
  ),
  POST: new RegExp(
    `^(products|skus|warehouses|inventory/adjustments|products/${uuid}/archive|skus/${uuid}/(archive|price)|provider-accounts|provider-accounts/${uuid}/rotate|${inspect}|markets|live-sessions|${studioAction}|${claimsRoutes.POST}|${adsRoutes.POST})$`,
  ),
  PATCH: new RegExp(`^(products/${uuid}|skus/${uuid}|${studioDetail}|${claimsRoutes.PATCH})$`),
  // Studio PUT is only the comment-source bind (claims-request.ts); settings PUTs are the rest.
  PUT: new RegExp(`^(${setting}|${policy}|${claimsRoutes.PUT}|${adsRoutes.PUT})$`),
};
const exactStore = new RegExp(`^${uuid}$`);
const inspectRoute = new RegExp(`^${inspect}$`);
const discoveryRoute = new RegExp(
  `^(markets|${deliveryCollection}|${paymentCollection}|${policy})$`,
);
const pagedSettingsRoute = new RegExp(`^(markets|${deliveryCollection})$`);
const purchaseEntryRoute = new RegExp(`^${purchaseEntry}$`);
const orderRoute = new RegExp(`^${orders}$`);
type Context = { params: Promise<{ store: string; resource: string[] }> };

async function route(request: Request, context: Context) {
  const error = localError;
  const { store, resource } = await context.params;
  const path = resource.join("/");
  // Refund/shipment/export/permission resources (orders-request.ts grammar) -> Go refunds.go/shipments.go.
  const action = orderActionRoute(request.method, path);
  // customers-billing-ui: customers/finance/billing resources (lib/customers-request.ts grammar) -> Go customers.go/finance.go/billing.go.
  const customers = customersRoute(request.method, path);
  // taiwan-cvs-logistics-v1 §8/§16.5: GET|PUT logistics/ecpay, POST logistics/ecpay/enabled, GET|PUT logistics/cvs-settings.
  const logistic = logisticsRoute(request.method, path);
  const studio = path.startsWith("live-sessions");
  if (studio && !authConfig) return error(404, "not_found");
  const input = studioInputRoute.test(path);
  // Trusted deployment origin, not forwarded headers or fixture auth, controls
  // the browser secret boundary. The Go runtime stays explicitly injected/off.
  if (input && !authConfig?.publicOrigin.startsWith("https://")) return error(404, "not_found");
  if (exactStore.test(store) && studio && studioAny.test(path) && !routes[request.method]?.test(path))
    return error(405, "method_not_allowed", "GET, POST, PATCH, PUT");
  if (!exactStore.test(store) || (!routes[request.method]?.test(path) && !action && !customers && !logistic))
    return error(404, "not_found");
  const order = request.method === "GET" && orderRoute.test(path);
  const accountRoute = path.startsWith("provider-accounts");
  const inspection = request.method === "POST" && inspectRoute.test(path);
  // Ads (adsRoutes): session/store authority only, and only `ads/report` may carry a query (from,to).
  const ads = adsAny.test(path);
  if (ads && !validAdsQuery(request.url, path)) return error(422, "invalid_request");
  // New setup routes require actual session/store authority, never a shared fixture.
  if ((studio || order || action || customers || logistic || accountRoute || ads || discoveryRoute.test(path)) && !authConfig)
    return error(404, "not_found");
  // Query grammar, Idempotency-Key presence and empty/JSON body declaration (keyless: billing POSTs; bodyless: export, portal).
  if (customers && !validCustomersRequest(customers, request)) return error(422, "invalid_request");
  // Exact resources: no query at all, including a bare trailing '?'.
  if ((action || logistic) && request.url.includes("?")) return error(422, "invalid_request");
  // Reads and the keyless refresh carry no body and no key; only commands do.
  // (keyless-command = print-form: a JSON body but no Idempotency-Key.)
  if (action && action !== "command" && action !== "keyless-command" && !validKeylessRequest(action, request))
    return error(422, "invalid_request");
  if (action === "keyless-command" && !validKeylessCommandRequest(request)) return error(422, "invalid_request");
  // Logistics reads carry no body/key/transfer-encoding, like every other exact BFF read.
  if (logistic === "get" && (request.body !== null || request.headers.has("transfer-encoding") ||
    request.headers.has("idempotency-key") || (request.headers.has("content-length") && request.headers.get("content-length") !== "0")))
    return error(422, "invalid_request");
  // URL.search drops an empty trailing '?'. Exact resources must reject that too;
  // Only collection GETs inherit the bounded pagination parser in Go.
  const exactResource =
    ((path === "markets" || path.startsWith("markets/")) &&
      !(request.method === "GET" && pagedSettingsRoute.test(path))) ||
    (accountRoute &&
      !(request.method === "GET" && path === "provider-accounts"));
  if (exactResource && request.url.includes("?"))
    return error(422, "invalid_request");
  const url = new URL(request.url);
  if (studio) {
    if (!validStudioQuery(request.url, request.method === "GET" && (path === "live-sessions" || claimsCollection(path))))
      return error(422, "invalid_request");
    if (
      request.method === "GET" &&
      (request.body !== null || request.headers.has("transfer-encoding") ||
        request.headers.has("idempotency-key") ||
        (request.headers.has("content-length") && request.headers.get("content-length") !== "0"))
    ) return error(422, "invalid_request");
  }
  if (order) {
    if (
      request.body !== null ||
      request.headers.has("transfer-encoding") ||
      request.headers.has("idempotency-key") ||
      (request.headers.has("content-length") &&
        request.headers.get("content-length") !== "0")
    )
      return error(422, "invalid_request");
    if (!validOrdersQuery(request.url, path !== "orders"))
      return error(422, "invalid_request");
  }
  if (
    request.method === "GET" &&
    purchaseEntryRoute.test(path) &&
    (!/^\?locale=(?:en|zh-CN|zh-TW)$/.test(url.search) ||
      request.body !== null ||
      request.headers.has("idempotency-key") ||
      (request.headers.has("content-length") &&
        request.headers.get("content-length") !== "0") ||
      request.headers.has("transfer-encoding"))
  )
    return error(422, "invalid_request");
  let token: string | undefined;
  if (authConfig) {
    token = sessionToken(request) ?? undefined;
    if (!token) {
      const denied = error(401, "unauthorized");
      if (order || action || customers || logistic) clearAuthCookies(denied.headers);
      return denied;
    }
    if (
      request.method !== "GET" &&
      (!requireOrigin(request) || !requireCSRF(request))
    )
      return error(403, "forbidden");
    const listed = await authenticatedStores(token);
    if (!listed.stores) {
      const denied = await safeError(listed.response);
      if (denied.status === 401) clearAuthCookies(denied.headers);
      return denied;
    }
    if (!listed.stores.some((item) => item.id === store))
      return error(404, "not_found");
  } else {
    const session = fixtureSession();
    if (!session) return error(401, "unauthorized");
    if (store !== session.storeID) return error(404, "not_found");
    if (url.hostname !== "127.0.0.1" && url.hostname !== "localhost")
      return error(403, "forbidden");
    const host = request.headers.get("host");
    if (host !== "127.0.0.1:3100" && host !== "localhost:3100")
      return error(403, "forbidden");
  }
  const init: RequestInit = { method: request.method };
  if (request.method !== "GET" && action !== "refresh") {
    if (!authConfig) {
      const host = request.headers.get("host");
      if (request.headers.get("origin") !== `http://${host}`)
        return error(403, "forbidden");
    }
    if (
      request.headers.get("content-type")?.split(";")[0] !== "application/json"
    )
      return error(415, "json_required");
    const key = request.headers.get("idempotency-key") ?? "";
    // Billing POSTs are keyless by contract (§6), print-form is a keyless command; every other command needs its key.
    const keyless = customers === "checkout" || customers === "portal" || action === "keyless-command";
    if (!inspection && !keyless && !/^[A-Za-z0-9_.:-]{8,128}$/.test(key))
      return error(422, "invalid_request");
    const customersBodyless = customers === "export" || customers === "portal";
    if (customersBodyless) {
      init.body = undefined; // validCustomersRequest already required an empty declared body
    } else if (studio) {
      try {
        if (!request.body) return error(400, "invalid_json");
        init.body = await readBody(request, "application/json");
      } catch {
        return error(400, "invalid_json");
      }
    } else {
      const reader = request.body?.getReader();
      if (!reader) return error(400, "invalid_json");
      const chunks: Uint8Array[] = [];
      let size = 0;
      while (true) {
        const part = await reader.read();
        if (part.done) break;
        size += part.value.byteLength;
        if (size > 65536) {
          await reader.cancel();
          return error(400, "invalid_json");
        }
        chunks.push(part.value);
      }
      const data = new Uint8Array(size);
      let at = 0;
      for (const chunk of chunks) {
        data.set(chunk, at);
        at += chunk.length;
      }
      init.body = new TextDecoder().decode(data);
    }
    // Exact bodies for the customers/billing commands (closed keys, ERASE word, consent pairs, price id).
    if (customers && !validCustomersBody(customers, init.body ?? "")) return error(400, "invalid_json");
    // PUT ads/drafts/{id} is revision-guarded: forward exactly one bare-decimal If-Match, never anything else.
    const ifMatch = request.headers.get("if-match");
    const draftPut = request.method === "PUT" && /^ads\/drafts\//.test(path);
    if (draftPut && !validIfMatch(ifMatch)) return error(422, "invalid_request");
    // ads pause/end: the browser sends "{}", the Go route takes no body.
    const adsNoBody = adsBodyless.test(path);
    if (adsNoBody) {
      if (init.body !== "{}") return error(422, "invalid_request");
      delete init.body;
    }
    init.headers = {
      ...(customersBodyless || adsNoBody ? {} : { "Content-Type": "application/json" }),
      ...(!inspection && !keyless && !adsKeyless.test(path) ? { "Idempotency-Key": key } : {}),
      ...(draftPut ? { "If-Match": ifMatch as string } : {}),
    };
  }
  const response = await callBackend(path + url.search, init, token, store);
  if (studio) {
    let body: string;
    try {
      const tokenResponse = input && path.endsWith("/token");
      const inputRead = studioInputReadRoute.test(path);
      const claimLink = claimLinkRoute(path);
      body = await readBody(response, "application/json", tokenResponse || inputRead || claimLink ? 8192 : 256 << 10);
      const parsed: unknown = JSON.parse(body);
      if (tokenResponse && response.ok && !validStudioInputToken(parsed)) return error(503, "retry_later");
      // The claim link token leaves the BFF only in this closed shape (§7.1 M7).
      if (claimLink && response.ok && !validClaimLink(parsed)) return error(503, "retry_later");
      if (inputRead && response.ok) {
        if (path.endsWith("/prepared")) parseStudioInputPrepared(parsed);
        else parseStudioInput(parsed);
      }
    } catch {
      return error(503, "retry_later");
    }
    if (!response.ok) {
      const denied = await safeError(new Response(body, {
        status: response.status,
        headers: { "Content-Type": "application/json", "Retry-After": response.headers.get("retry-after") ?? "" },
      }));
      if (response.status === 401) clearAuthCookies(denied.headers);
      return denied;
    }
    return new Response(body, {
      status: response.status,
      headers: { "Content-Type": "application/json", "X-Request-ID": response.headers.get("x-request-id") ?? "" },
    });
  }
  if (authConfig && response.status === 401) {
    const denied = await safeError(response);
    clearAuthCookies(denied.headers);
    return denied;
  }
  if (!response.ok) return safeError(response);
  if (customers === "export" || customers === "finance-csv") {
    // PII exports: streamed straight through, never buffered or stored here; only the exact attachment shape passes.
    const json = customers === "export";
    const disposition = response.headers.get("content-disposition") ?? "";
    if (
      response.status !== 200 || !response.body ||
      response.headers.get("content-type")?.toLowerCase() !== (json ? "application/json" : "text/csv; charset=utf-8") ||
      !(json ? /^attachment; filename="customer-[0-9a-f-]{36}\.json"$/ : /^attachment; filename="finance-[0-9-]{10}-[0-9-]{10}\.csv"$/).test(disposition)
    )
      return error(503, "retry_later");
    return new Response(response.body, {
      status: 200,
      headers: {
        "Content-Type": json ? "application/json" : "text/csv; charset=utf-8",
        "Content-Disposition": disposition,
        "Cache-Control": "no-store, private",
        "X-Content-Type-Options": "nosniff",
        "X-Request-ID": response.headers.get("x-request-id") ?? "",
      },
    });
  }
  if (action === "csv") {
    // PII export: streamed straight through, never buffered or stored here; refuse anything but the exact attachment.
    if (response.status !== 200 || !response.body || !validCSVHeaders(response.headers))
      return error(503, "retry_later");
    return new Response(response.body, {
      status: 200,
      headers: {
        "Content-Type": "text/csv; charset=utf-8",
        "Content-Disposition": response.headers.get("content-disposition") ?? "",
        "Cache-Control": "no-store, private",
        "X-Export-Truncated": response.headers.get("x-export-truncated") ?? "",
        "X-Content-Type-Options": "nosniff",
        "X-Request-ID": response.headers.get("x-request-id") ?? "",
      },
    });
  }
  return new Response(response.body, {
    status: response.status,
    headers: {
      "Content-Type": "application/json",
      "Cache-Control": order || action || customers || logistic ? "private, no-store" : "no-store",
      "X-Request-ID": response.headers.get("x-request-id") ?? "",
    },
  });
}
async function proxy(request: Request, context: Context) {
  const path = (await context.params).resource.join("/");
  const response = await route(request, context);
  if (path.startsWith("live-sessions")) response.headers.set("Cache-Control", "private, no-store");
  // Every M7 answer (success or not) forbids a Referer, like the Go route (§7.1).
  if (claimLinkRoute(path)) response.headers.set("Referrer-Policy", "no-referrer");
  return response;
}
export const GET = proxy;
export const POST = proxy;
export const PATCH = proxy;
export const PUT = proxy;
const unsupported = async (_request: Request, context: Context) => {
  const path = (await context.params).resource.join("/");
  const response = (path.startsWith("live-sessions") && !authConfig) ||
    (studioInputRoute.test(path) && !authConfig?.publicOrigin.startsWith("https://"))
    ? localError(404, "not_found")
    : path.startsWith("live-sessions") && !studioAny.test(path)
    ? localError(404, "not_found")
    : localError(405, "method_not_allowed", "GET, POST, PATCH, PUT");
  if (path.startsWith("live-sessions"))
    response.headers.set("Cache-Control", "private, no-store");
  if (claimLinkRoute(path)) response.headers.set("Referrer-Policy", "no-referrer");
  return response;
};
export const DELETE = unsupported;
export const OPTIONS = unsupported;
export const HEAD = unsupported;
