// Owns the buyer claim-link wire contract in the storefront: the canonical link-token
// grammar, the #t= fragment reader, and closed validators for the B1 preview and B2
// redeem projections (contracts/live-keyword-claims-v1.md §7.2, §11.1).
// Non-goals: no fetching, no storage, no admission (Go decides binding, versions and
// availability); a failed validation is an unknown result, never a partial render.
// Depends on nothing (pure; shared by the BFF in buyer-server.ts, the browser page and
// node:test).

export type ClaimPreviewLine = {
  keyword: string;
  sku_id: string;
  sku_code: string;
  product_name: string;
  currency: string;
  unit_price_minor: number;
  quantity: number;
  pending: boolean;
  available: boolean;
};
export type ClaimPreview = {
  bundle_version: number;
  bound: boolean;
  expires_at: string;
  lines: ClaimPreviewLine[];
};
export type ClaimCart = {
  id: string;
  currency: string;
  version: number;
  items: { sku_id: string; quantity: number }[];
};
export type ClaimRedeemed = {
  bundle_version: number;
  cart: ClaimCart;
  applied: { sku_id: string; quantity: number }[];
  skipped: { sku_id: string; reason: "unavailable" | "offer_inactive" }[];
};

// 32 random bytes as canonical unpadded base64url: 42 free characters and a final one
// whose two unused low bits are zero.
export const CLAIM_TOKEN = /^[A-Za-z0-9_-]{42}[AEIMQUYcgkosw048]$/;
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

/** Reads `#t=<token>` from a location hash; anything else (extra keys, bad token) is null. */
export function claimFragment(hash: string): string | null {
  const match = /^#t=([A-Za-z0-9_-]{43})$/.exec(hash);
  return match && CLAIM_TOKEN.test(match[1]) ? match[1] : null;
}

function exact(value: unknown, keys: readonly string[]): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value) &&
    Object.keys(value).length === keys.length && keys.every((key) => Object.hasOwn(value, key));
}
const count = (value: unknown, min: number) => Number.isSafeInteger(value) && (value as number) >= min;
const time = (value: unknown) => typeof value === "string" && value.length <= 40 && Number.isFinite(Date.parse(value));
const item = (value: unknown) => exact(value, ["sku_id", "quantity"]) &&
  typeof value.sku_id === "string" && UUID.test(value.sku_id) && count(value.quantity, 1);

/** Closed B1 projection (no label, actor, platform, owner or scene title). */
export function validClaimPreview(value: unknown): value is ClaimPreview {
  if (!exact(value, ["bundle_version", "bound", "expires_at", "lines"]) || !count(value.bundle_version, 1) ||
    typeof value.bound !== "boolean" || !time(value.expires_at) || !Array.isArray(value.lines) ||
    value.lines.length < 1 || value.lines.length > 50) return false;
  return value.lines.every((line: unknown) =>
    exact(line, ["keyword", "sku_id", "sku_code", "product_name", "currency", "unit_price_minor", "quantity", "pending", "available"]) &&
    typeof line.keyword === "string" && /^[A-Z0-9]{1,16}$/.test(line.keyword) &&
    typeof line.sku_id === "string" && UUID.test(line.sku_id) && typeof line.sku_code === "string" &&
    typeof line.product_name === "string" && typeof line.currency === "string" && /^[A-Z]{3}$/.test(line.currency) &&
    count(line.unit_price_minor, 0) && count(line.quantity, 1) && (line.quantity as number) <= 999 &&
    typeof line.pending === "boolean" && typeof line.available === "boolean");
}

/** The existing buyer cart projection (GET/PUT cart and B2's cart). */
export function validClaimCart(value: unknown): value is ClaimCart {
  // A buyer without a cart row yet has id "" and version 0 (storefront.GetCart).
  return exact(value, ["id", "currency", "version", "items"]) && typeof value.id === "string" && (value.id === "" || UUID.test(value.id)) &&
    typeof value.currency === "string" && /^[A-Z]{3}$/.test(value.currency) && count(value.version, 0) &&
    Array.isArray(value.items) && value.items.length <= 50 && value.items.every(item);
}

/** Closed B2 projection. */
export function validClaimRedeemed(value: unknown): value is ClaimRedeemed {
  return exact(value, ["bundle_version", "cart", "applied", "skipped"]) && count(value.bundle_version, 1) &&
    validClaimCart(value.cart) && Array.isArray(value.applied) && value.applied.every(item) &&
    Array.isArray(value.skipped) && value.skipped.every((entry: unknown) => exact(entry, ["sku_id", "reason"]) &&
      typeof entry.sku_id === "string" && UUID.test(entry.sku_id) &&
      (entry.reason === "unavailable" || entry.reason === "offer_inactive"));
}
