// Admin logistics client: browser -> BFF `/api/stores/{store}/logistics/{ecpay,ecpay/enabled,cvs-settings}` and
// `/api/stores/{store}/orders/{id}/{cvs-shipment,cvs-shipment/print-form,cvs-shipment/abandon,collection,
// pay-at-pickup-release}` -> Go internal/httpapi/cvs.go (contracts/taiwan-cvs-logistics-v1.md §8, §16.4-§16.8).
// Reads parse the frozen DTOs (logistics-model.ts); writes reuse writeSettings (cookie CSRF + session fence +
// Idempotency-Key) and are never trusted for state: callers re-GET after every response (no optimistic UI).
// Only print-form is keyless (§8), so it has its own fetch. ECPay keys are sent once and never read back.
import { csrfCookie, safeError, sessionBoundary, writeSettings } from "./settings-client";
import { OrderReadError, read } from "./orders-client";
import {
  parseCvsSettings,
  parseCvsShipment,
  parseEcpay,
  parsePrintForm,
  printBody,
  type CvsSettings,
  type CvsShipment,
  type EcpayConnection,
  type PrintForm,
} from "./logistics-model";

async function get<T>(path: string, parse: (value: unknown) => T, signal: AbortSignal): Promise<T> {
  try {
    return parse(await read(path, signal));
  } catch (error) {
    if (error instanceof OrderReadError) throw error;
    throw new OrderReadError("unavailable"); // a body that fails the frozen shape reads as unavailable
  }
}
const logisticsPath = (store: string, resource: string) => `/api/stores/${store}/logistics/${resource}`;
const orderPath = (store: string, id: string) => `/api/stores/${store}/orders/${id}`;

// Go GET logistics/ecpay (integration:read). 404 = no connection yet; 403 = this member may not see the card.
export async function readEcpay(store: string, signal: AbortSignal): Promise<EcpayConnection | null> {
  try {
    return await get(logisticsPath(store, "ecpay"), parseEcpay, signal);
  } catch (error) {
    if (error instanceof OrderReadError && error.code === "not-found") return null;
    throw error;
  }
}
// Go GET logistics/cvs-settings (integration:read). Go answers defaults with version 0 when no row exists (C8).
export const readCvsSettings = (store: string, signal: AbortSignal): Promise<CvsSettings> =>
  get(logisticsPath(store, "cvs-settings"), parseCvsSettings, signal);
// Go GET orders/{id}/cvs-shipment (orders:read). 404 = the order has no CVS shipment record.
export async function readCvsShipment(store: string, id: string, signal: AbortSignal): Promise<CvsShipment | null> {
  try {
    return await get(`${orderPath(store, id)}/cvs-shipment`, parseCvsShipment, signal);
  } catch (error) {
    if (error instanceof OrderReadError && error.code === "not-found") return null;
    throw error;
  }
}

export type WriteResult = { ok: true } | { ok: false; code: string; uncertain: boolean };
async function write(store: string, method: "POST" | "PUT", resource: string, key: string, body: string, boundary: string): Promise<WriteResult> {
  try {
    const result = await writeSettings<unknown>(store, { key, method, resource }, body, boundary);
    if (result.error === null && !result.uncertain) return { ok: true };
    return { ok: false, code: result.error?.code ?? "retry_later", uncertain: result.uncertain };
  } catch {
    return { ok: false, code: "unauthorized", uncertain: false }; // session_changed: nothing was sent
  }
}
// Go PUT logistics/ecpay (integration:manage): register or rotate; the probe runs server-side (§7.3).
export const putEcpay = (store: string, key: string, body: string, boundary: string) =>
  write(store, "PUT", "logistics/ecpay", key, body, boundary);
// Go POST logistics/ecpay/enabled (integration:manage): body exactly {expected_version, enabled}.
export const postEcpayEnabled = (store: string, key: string, body: string, boundary: string) =>
  write(store, "POST", "logistics/ecpay/enabled", key, body, boundary);
// Go PUT logistics/cvs-settings (integration:manage): chains, pay-at-pickup toggle and limits (§16.5).
export const putCvsSettings = (store: string, key: string, body: string, boundary: string) =>
  write(store, "PUT", "logistics/cvs-settings", key, body, boundary);
// Go POST orders/{id}/cvs-shipment (fulfillment:write): 202 REQUESTED; the result arrives asynchronously.
export const postCvsShipment = (store: string, id: string, key: string, body: string, boundary: string) =>
  write(store, "POST", `orders/${id}/cvs-shipment`, key, body, boundary);
// Go POST orders/{id}/cvs-shipment/abandon (fulfillment:write): Go queries ECPay first (MAC-verified).
export const postCvsAbandon = (store: string, id: string, key: string, body: string, boundary: string) =>
  write(store, "POST", `orders/${id}/cvs-shipment/abandon`, key, body, boundary);
// Go POST orders/{id}/collection (fulfillment:write): manual collected / returned / refunded_offline (§16.4).
export const postCollection = (store: string, id: string, key: string, body: string, boundary: string) =>
  write(store, "POST", `orders/${id}/collection`, key, body, boundary);
// Go POST orders/{id}/pay-at-pickup-release (fulfillment:write): cancel / restock, one audited stock writer (§16.8).
export const postRelease = (store: string, id: string, key: string, body: string, boundary: string) =>
  write(store, "POST", `orders/${id}/pay-at-pickup-release`, key, body, boundary);

// Go POST orders/{id}/cvs-shipment/print-form (fulfillment:write): keyless, no state change; the signed form is
// posted by the print tab (app/[locale]/orders/cvs-print). Nothing is stored and the fields are never logged.
export async function postPrintForm(
  store: string,
  id: string,
  thermal: boolean,
): Promise<{ ok: true; form: PrintForm } | { ok: false; code: string }> {
  const csrf = csrfCookie();
  if (!csrf) return { ok: false, code: "unauthorized" };
  try {
    await sessionBoundary(csrf); // the cookie must be stable across the hash await
    const response = await fetch(`${orderPath(store, id)}/cvs-shipment/print-form`, {
      method: "POST",
      credentials: "same-origin",
      cache: "no-store",
      headers: { "Content-Type": "application/json", "X-CSRF-Token": csrf },
      body: printBody(thermal),
      signal: AbortSignal.timeout(15000),
    });
    const value: unknown = await response.json().catch(() => null);
    if (!response.ok) return { ok: false, code: safeError(value).code };
    return { ok: true, form: parsePrintForm(value) };
  } catch {
    return { ok: false, code: "retry_later" };
  }
}
