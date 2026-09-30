// Admin billing client: browser -> BFF `/api/stores/{store}/billing*` -> Go `internal/httpapi/billing.go`
// (contract customers-billing-v1 §5; billing-core FROZEN block). Reads parse billing-model; the two POSTs are keyless
// (Stripe params carry a wall-clock expiry, §6) and return a Stripe bearer URL that the caller passes straight to
// `location.assign` (I11): it is never stored, logged or kept in state, and a failed parse discloses nothing.
import { get, post, type WriteOutcome } from "./customers-client";
import { safeError } from "./settings-client";
import { parseBillingStatus, parseRedirect, parseStanding, type BillingStatus, type Standing } from "./billing-model";

const base = (store: string) => `/api/stores/${store}/billing`;
// Go GET billing (billing:manage): status, usage, configured plans; `stale` when Stripe was unreachable for refresh.
export const readBilling = (store: string, signal: AbortSignal): Promise<BillingStatus> =>
  get(base(store), parseBillingStatus, signal);
// Go GET billing/standing (store:read): every member; used by the banner.
export const readStanding = (store: string, signal: AbortSignal): Promise<Standing> =>
  get(`${base(store)}/standing`, parseStanding, signal);

async function redirect(store: string, path: "checkout" | "portal", body: string | undefined, boundary: string): Promise<WriteOutcome<string>> {
  // 30 s: the server makes up to three Stripe calls (each 10 s, contract §7). No key and no retry: a repeated click
  // makes a new session; the server keeps at most one open (§5 step 4), so a duplicate click is harmless.
  const sent = await post(store, `billing/${path}`, { body, boundary, timeoutMs: 30000 });
  if (!("response" in sent)) return { ...sent, uncertain: false };
  const { response } = sent;
  const value: unknown = await response.json().catch(() => null);
  if (!response.ok) return { ok: false, code: safeError(value).code, uncertain: false };
  try {
    return { ok: true, value: parseRedirect(value) };
  } catch {
    return { ok: false, code: "retry_later", uncertain: false };
  }
}
// Go POST billing/checkout (billing:manage; body exactly {price_id}; 409 subscription_exists, 503 billing_unavailable).
export const startCheckout = (store: string, priceID: string, boundary: string) =>
  redirect(store, "checkout", JSON.stringify({ price_id: priceID }), boundary);
// Go POST billing/portal (billing:manage; no body; 409 no_billing_customer).
export const openPortal = (store: string, boundary: string) => redirect(store, "portal", undefined, boundary);
