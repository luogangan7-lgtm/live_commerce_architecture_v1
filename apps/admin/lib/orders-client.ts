// Admin orders client: browser -> BFF `/api/stores/{store}/orders*` and `order-actions`
// -> Go `internal/httpapi/{orders,refunds,shipments}.go`. Reads parse the frozen DTOs; writes reuse writeSettings
// (cookie CSRF + session fence + Idempotency-Key) and never trust their own response body for state (re-GET instead).
import { csrfCookie, safeError, sessionBoundary, writeSettings } from "./settings-client";
import {
  parseOrderActions,
  parseOrderDetail,
  parseOrderList,
  parseRefundList,
  parseShipmentHistory,
  type OrderFilter,
} from "./orders-model";

export type OrderReadCode = "signed-out" | "forbidden" | "not-found" | "unavailable";
export class OrderReadError extends Error {
  constructor(readonly code: OrderReadCode) {
    super(code);
  }
}

// Shared by logistics-client.ts (same BFF envelope: private/no-store JSON, 401/403/404 mapped to a read code).
export async function read(path: string, signal: AbortSignal): Promise<unknown> {
  let response: Response;
  try {
    response = await fetch(path, {
      method: "GET",
      cache: "no-store",
      credentials: "same-origin",
      signal,
    });
  } catch {
    throw new OrderReadError("unavailable");
  }
  if (response.status === 401) throw new OrderReadError("signed-out");
  if (response.status === 403) throw new OrderReadError("forbidden");
  if (response.status === 404) throw new OrderReadError("not-found");
  if (!response.ok || response.headers.get("content-type")?.split(";", 1)[0] !== "application/json" ||
    response.headers.get("cache-control") !== "private, no-store")
    throw new OrderReadError("unavailable");
  try {
    return await response.json();
  } catch {
    throw new OrderReadError("unavailable");
  }
}

export async function readOrderList(store: string, state: OrderFilter, cursor: string, signal: AbortSignal) {
  const query = `limit=10&state=${state}${cursor ? `&cursor=${cursor}` : ""}`;
  try {
    return parseOrderList(await read(`/api/stores/${store}/orders?${query}`, signal));
  } catch (error) {
    if (error instanceof OrderReadError) throw error;
    throw new OrderReadError("unavailable");
  }
}

export async function readOrderDetail(store: string, id: string, signal: AbortSignal) {
  try {
    return parseOrderDetail(await read(`/api/stores/${store}/orders/${id}`, signal), id);
  } catch (error) {
    if (error instanceof OrderReadError) throw error;
    throw new OrderReadError("unavailable");
  }
}

async function get<T>(path: string, parse: (value: unknown) => T, signal: AbortSignal) {
  try {
    return parse(await read(path, signal));
  } catch (error) {
    if (error instanceof OrderReadError) throw error;
    throw new OrderReadError("unavailable");
  }
}
const orderPath = (store: string, id: string) => `/api/stores/${store}/orders/${id}`;

// Go GET order-actions: which action buttons to offer. The server re-authorizes every write.
export const readOrderActions = (store: string, signal: AbortSignal) =>
  get(`/api/stores/${store}/order-actions`, parseOrderActions, signal);
// Go GET orders/{id}/refunds (orders:read).
export const readRefunds = (store: string, id: string, currency: string, signal: AbortSignal) =>
  get(`${orderPath(store, id)}/refunds`, (value) => parseRefundList(value, currency), signal);
// Go GET orders/{id}/shipment/history (orders:read; merchant-only fields).
export const readShipmentHistory = (store: string, id: string, signal: AbortSignal) =>
  get(`${orderPath(store, id)}/shipment/history`, parseShipmentHistory, signal);
export const exportUnshippedHref = (store: string) => `/api/stores/${store}/orders/unshipped.csv`;

export type WriteResult = { ok: true } | { ok: false; code: string; uncertain: boolean };
async function write(store: string, method: "POST" | "PUT", resource: string, key: string, body: string, boundary: string): Promise<WriteResult> {
  try {
    const result = await writeSettings<unknown>(store, { key, method, resource }, body, boundary);
    if (result.error === null && !result.uncertain) return { ok: true };
    return { ok: false, code: result.error?.code ?? "retry_later", uncertain: result.uncertain };
  } catch {
    // session_changed: nothing was sent.
    return { ok: false, code: "unauthorized", uncertain: false };
  }
}
// Go POST orders/{id}/refunds (payments:refund; body exactly amount_minor/reason/expected_refundable_minor).
export const postRefund = (store: string, id: string, key: string, body: string, boundary: string) =>
  write(store, "POST", `orders/${id}/refunds`, key, body, boundary);
// Go PUT orders/{id}/shipment (fulfillment:write; one command route for record, correct and void).
export const putShipment = (store: string, id: string, key: string, body: string, boundary: string) =>
  write(store, "PUT", `orders/${id}/shipment`, key, body, boundary);

// Go POST orders/{id}/refunds/{refund}/refresh: keyless and bodyless; throttled in SQL.
export async function refreshRefund(store: string, id: string, refundId: string, boundary: string): Promise<"scheduled" | "later" | WriteResult & { ok: false }> {
  const csrf = csrfCookie();
  try {
    if (!csrf || (await sessionBoundary(csrf)) !== boundary) return { ok: false, code: "unauthorized", uncertain: false };
    const response = await fetch(`${orderPath(store, id)}/refunds/${refundId}/refresh`, {
      method: "POST",
      credentials: "same-origin",
      headers: { "X-CSRF-Token": csrf },
      signal: AbortSignal.timeout(8000),
    });
    const value: unknown = await response.json().catch(() => null);
    if (response.ok) {
      const scheduled = value && typeof value === "object" && (value as Record<string, unknown>).scheduled;
      return scheduled === true ? "scheduled" : "later";
    }
    const code = safeError(value).code;
    return response.status === 429 || code === "rate_limited" ? "later" : { ok: false, code, uncertain: false };
  } catch {
    return { ok: false, code: "retry_later", uncertain: false };
  }
}
