// Owns the buyer privacy wire contract in the storefront (customers-billing-v1 §5 "Buyer", customers-core FROZEN block):
// the route-table entries and body shapes the BFF (lib/buyer-server.ts, integrator hook) spreads in, the request body
// builders, the closed response validators, and the notice version constant.
// BFF /api/buyer/{privacy,consents,privacy/export,privacy/erasure} -> Go /v1/buyer/{privacy,consents,privacy/export,
// privacy/erasure} (internal/buyerhttp/privacy.go; internal/customers.{ReadBuyerPrivacy,BuyerSetConsent,BuyerExport,BuyerErase}).
// Non-goals: no fetching, no storage, no consent authority. Go decides what is granted, erased or exportable, sets the
// consent `source` from `context` and the `policy_version` itself; a failed validation is an unknown result, never a
// partial render. Depends on nothing (pure; shared by the BFF, the browser components and node:test).

// Must equal internal/customers.PrivacyPolicyVersion (customers-core D4): the notice text and its version ship together,
// so changing the notice is a code change in both places.
export const LC_PRIVACY_POLICY_VERSION = "lc-2026-10";

export type ConsentPurpose = "marketing_messages" | "ads_personalization";
export type ConsentChannel = "meta_dm" | "meta_ads";
export const consentPairs: readonly { purpose: ConsentPurpose; channel: ConsentChannel }[] = [
  { purpose: "marketing_messages", channel: "meta_dm" },
  { purpose: "ads_personalization", channel: "meta_ads" },
];
export type ConsentContext = "checkout" | "settings";

export type BuyerPrivacy = {
  consents: { marketing_messages: boolean; ads_personalization: boolean };
  erased: boolean;
};
export type ConsentResult = { purpose: ConsentPurpose; channel: ConsentChannel; granted: boolean; occurred_at: string };
export type ErasureSummary = {
  consents_withdrawn: number;
  sessions_revoked: number;
  snapshots_redacted: number;
  bundles_relabelled: number;
};

// Route-table entries in the shape of buyer-server.ts `exact` (suffix -> method -> route without `method`).
// `body` names an entry of `privacyBodies`; "empty" is the existing `{}` shape. Idempotency-Key is required on every
// mutation by the BFF's own rule (not on GET). Integrator: spread into `exact`, add the "boolean" shape kind.
export const privacyRoutes = {
  privacy: { GET: { privatePath: "privacy" } },
  consents: { PUT: { privatePath: "consents", body: "consent" } },
  "privacy/export": { POST: { privatePath: "privacy/export", body: "empty" } },
  "privacy/erasure": { POST: { privatePath: "privacy/erasure", body: "erasure" } },
} as const;

// Body shapes in the buyer-server.ts `Shape` vocabulary (+ "boolean", which that vocabulary lacks today). Closed: any
// other key is refused. Values (purpose/channel pair, the word ERASE) are validated again by Go.
export const privacyBodies = {
  consent: { purpose: "string", channel: "string", granted: "boolean", context: "string" },
  erasure: { confirm: "string" },
} as const;

export function consentBody(purpose: ConsentPurpose, channel: ConsentChannel, granted: boolean, context: ConsentContext) {
  return { purpose, channel, granted, context };
}
export const erasureBody = { confirm: "ERASE" } as const;

function exact(value: unknown, keys: readonly string[]): value is Record<string, unknown> {
  return (
    value !== null && typeof value === "object" && !Array.isArray(value) &&
    Object.keys(value).length === keys.length && keys.every((key) => Object.hasOwn(value, key))
  );
}
const count = (value: unknown) => Number.isSafeInteger(value) && (value as number) >= 0 && (value as number) <= 1_000_000_000;
const instant = (value: unknown) =>
  typeof value === "string" && /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?Z$/.test(value) && Number.isFinite(Date.parse(value));

/** GET privacy: current consents and whether an erasure exists. */
export function validBuyerPrivacy(value: unknown): value is BuyerPrivacy {
  if (!exact(value, ["consents", "erased"]) || typeof value.erased !== "boolean") return false;
  const consents = value.consents;
  return (
    exact(consents, ["marketing_messages", "ads_personalization"]) &&
    typeof consents.marketing_messages === "boolean" && typeof consents.ads_personalization === "boolean" &&
    // An erased owner never holds a granted consent (customers.consent_allows is false for it).
    (!value.erased || (!consents.marketing_messages && !consents.ads_personalization))
  );
}
/** PUT consents 200: echo of the stored event; the page still re-reads GET privacy before showing state. */
export function validConsentResult(value: unknown): value is ConsentResult {
  return (
    exact(value, ["purpose", "channel", "granted", "occurred_at"]) &&
    consentPairs.some((pair) => pair.purpose === value.purpose && pair.channel === value.channel) &&
    typeof value.granted === "boolean" && instant(value.occurred_at)
  );
}
/** POST privacy/erasure 200: Go's envelope {erased, orders_retained, summary:{4 counts}} (buyerhttp erasureResponse, contract §5:
 * the buyer is told order records are kept for legal retention). The page shows no counts, only the erased state. */
export function validErasureSummary(value: unknown): value is { erased: true; orders_retained: true; summary: ErasureSummary } {
  if (!exact(value, ["erased", "orders_retained", "summary"]) || value.erased !== true || value.orders_retained !== true) return false;
  const summary = value.summary;
  return exact(summary, ["consents_withdrawn", "sessions_revoked", "snapshots_redacted", "bundles_relabelled"]) &&
    [summary.consents_withdrawn, summary.sessions_revoked, summary.snapshots_redacted, summary.bundles_relabelled].every(count);
}
/** POST privacy/export 200: the lc.customer-export.v1 envelope; the buyer copy never carries customer_id (customers-core D8). */
export function validBuyerExport(value: unknown): boolean {
  return (
    exact(value, ["format", "generated_at", "store", "orders", "consents", "claims", "privacy_actions"]) &&
    value.format === "lc.customer-export.v1" && instant(value.generated_at) &&
    value.store !== null && typeof value.store === "object" && !Array.isArray(value.store) &&
    Array.isArray(value.orders) && Array.isArray(value.consents) && Array.isArray(value.claims) &&
    Array.isArray(value.privacy_actions)
  );
}

/** Error codes the privacy pages word specifically (Go `responseError`, customers-core D3). */
export type PrivacyFailure = "erased" | "erasure_blocked" | "export_too_large" | "idempotency_conflict" | "other";
export function privacyFailure(status: number, code: string): PrivacyFailure {
  if (status === 410 || code === "erased") return "erased";
  if (code === "erasure_blocked") return "erasure_blocked";
  if (code === "export_too_large") return "export_too_large";
  if (code === "idempotency_conflict") return "idempotency_conflict";
  return "other";
}
