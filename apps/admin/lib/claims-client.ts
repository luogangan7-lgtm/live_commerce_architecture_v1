// Owns the Studio › Claims browser requests: M1–M7 and the comment-source read/bind (claim-source) through the same private Studio
// read/write boundary (same-origin BFF, CSRF header, session-boundary check before and
// after every write, private no-store JSON only), plus the existing catalog reads the
// offer form and link builder need (products, SKUs, purchase-entry origin).
// Non-goals: no retries or key generation (the component owns one key per submit and
// reuses it only for an explicit "retry same request"), no storage of any response, and
// no token persistence: an issued link token is returned to the caller's memory only.
// Depends on: studio-client.ts (read, write, StudioError), claims-model.ts (parsers).

import { read, StudioError, write } from "./studio-client";
import { parseClaimSource, parseClaimSourceEnvelope } from "./claim-source-model";
import type { claimSourceBody } from "./claims-request";
import {
  catalogProduct, catalogSKU, parseBoard, parseBundlePage, parseCatalogPage, parseClaimLink,
  parseManualResult, parseOffer, parsePurchaseEntry, parseWindow,
  type CatalogProduct, type CatalogSKU, type MatchMode,
} from "./claims-model";

const base = (store: string, session: string) => `/api/stores/${store}/live-sessions/${session}/claims`;
// Any parse failure after a write is an unknown result: it may have committed.
async function parsed<T>(work: () => Promise<unknown>, parse: (value: unknown) => T, uncertain: boolean): Promise<T> {
  try { return parse(await work()); }
  catch (error) { throw error instanceof StudioError ? error : new StudioError(uncertain ? "uncertain" : "unavailable"); }
}

// Every call below goes through the admin BFF (same-origin, signed session cookie); Go
// re-checks live:read (reads) or live:manage (writes) inside its own transaction. The only
// caller is components/StudioClaims.tsx.

/** M1: window, offers and per-reason stats of the current round (live:read). */
export const readClaimsBoard = (store: string, session: string, signal: AbortSignal) =>
  parsed(() => read(base(store, session), signal), (value) => parseBoard(value, session), false);
/** M6: bundles newest first, 100 per page; cursor "" is the first page (live:read). */
export const readClaimBundles = (store: string, session: string, cursor: string, signal: AbortSignal) =>
  parsed(() => read(`${base(store, session)}/bundles?limit=100${cursor ? `&cursor=${cursor}` : ""}`, signal), parseBundlePage, false);

/** M2: open, close or re-mode the window with a version CAS (live:manage). */
export const setClaimWindow = (store: string, session: string, body: { expected_version: number; state: "OPEN" | "CLOSED"; match_mode: MatchMode }, key: string, boundary: string) =>
  parsed(() => write(`${base(store, session)}/window`, "POST", body, key, boundary), (value) => parseWindow(value, session), true);
/** M3: bind a keyword to a SKU in this scene (live:manage). */
export const createClaimOffer = (store: string, session: string, body: { keyword: string; sku_id: string; max_quantity_per_claim: number }, key: string, boundary: string) =>
  parsed(() => write(`${base(store, session)}/offers`, "POST", body, key, boundary), (value) => parseOffer(value, session), true);
/** M4: change an offer's limit or active flag with a version CAS (live:manage). */
export const updateClaimOffer = (store: string, session: string, offer: string, body: { expected_version: number; max_quantity_per_claim: number; active: boolean }, key: string, boundary: string) =>
  parsed(() => write(`${base(store, session)}/offers/${offer}`, "PATCH", body, key, boundary), (value) => parseOffer(value, session), true);
/** M5: record one operator-attested comment; REJECTED is a result, not an error (live:manage). */
export const recordManualClaim = (store: string, session: string, body: { text: string } & ({ bundle_id: string } | { actor_label: string }), key: string, boundary: string) =>
  parsed(() => write(`${base(store, session)}/manual`, "POST", body, key, boundary), parseManualResult, true);
/**
 * M7: issue, replace or release+replace a bundle link with a generation CAS (live:manage).
 * The returned token (first execution only) must stay in component memory and be dropped
 * when the one-time dialog closes; a replay returns token null.
 */
export const issueClaimLink = (store: string, session: string, bundle: string, body: { expected_generation: number; release_binding: boolean }, key: string, boundary: string) =>
  parsed(() => write(`${base(store, session)}/bundles/${bundle}/link`, "POST", body, key, boundary), parseClaimLink, true);

/** Comment source of the scene: the bound Meta post/media or null (live:read side; Go decides). */
export const readClaimSource = (store: string, session: string, signal: AbortSignal) =>
  parsed(() => read(`/api/stores/${store}/live-sessions/${session}/claim-source`, signal), parseClaimSourceEnvelope, false);
/**
 * Bind, rebind (version CAS) or deactivate the comment source (live:manage + integration:execute).
 * Go → live.put_claim_source inside command.Run; a definite refusal carries error.api
 * (input_invalid, input_unresolvable, binding_missing, binding_ambiguous, source_conflict, version_changed).
 */
export const putClaimSource = (store: string, session: string, body: ReturnType<typeof claimSourceBody>, key: string, boundary: string) =>
  parsed(() => write(`/api/stores/${store}/live-sessions/${session}/claim-source`, "PUT", body, key, boundary), parseClaimSource, true);

// Catalog routes answer "no-store" (not "private, no-store"), so they use this plain read.
async function catalogRead(path: string, signal: AbortSignal): Promise<unknown> {
  let response: Response;
  try { response = await fetch(path, { method: "GET", cache: "no-store", credentials: "same-origin", signal }); }
  catch { throw new StudioError("unavailable"); }
  if (!response.ok) throw new StudioError(response.status === 401 ? "signed-out" : response.status === 403 ? "forbidden" : response.status === 404 ? "not-found" : "unavailable");
  try { return await response.json(); } catch { throw new StudioError("unavailable"); }
}

/** Active products of the store (first 100), for the offer form's product picker. */
export const readClaimProducts = (store: string, signal: AbortSignal): Promise<CatalogProduct[]> =>
  parsed(() => catalogRead(`/api/stores/${store}/products?limit=100`, signal),
    (value) => parseCatalogPage(value, catalogProduct).items.filter((item) => item.status === "active"), false);
/** Active SKUs of one product (first 100), for the offer form's SKU picker. */
export const readClaimSKUs = (store: string, product: string, signal: AbortSignal): Promise<CatalogSKU[]> =>
  parsed(() => catalogRead(`/api/stores/${store}/products/${product}/skus?limit=100`, signal),
    (value) => parseCatalogPage(value, catalogSKU).items.filter((item) => item.status === "active" && item.product_id === product), false);

/**
 * The published storefront origin, read through the existing merchant purchase-entry
 * projection (the only merchant route that resolves it). The origin is store-wide, so the
 * first configured product decides; a store-level state (unpublished, no domain) or no
 * configured product within the first ten active products returns null.
 */
export async function readStorefrontOrigin(store: string, products: CatalogProduct[], signal: AbortSignal): Promise<string | null> {
  for (const product of products.slice(0, 10)) {
    const entry = await parsed(() => catalogRead(`/api/stores/${store}/products/${product.id}/purchase-entry?locale=en`, signal),
      (value) => parsePurchaseEntry(value, product.id, "en"), false);
    if (entry.state === "configured") {
      const url = new URL(entry.url);
      if (url.protocol !== "https:" || url.pathname !== `/en/products/${product.id}`) throw new StudioError("unavailable");
      return url.origin;
    }
    if (entry.state === "storefront_unavailable" || entry.state === "domain_selection_required") return null;
  }
  return null;
}
