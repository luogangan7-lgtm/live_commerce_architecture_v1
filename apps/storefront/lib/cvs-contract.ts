// Pure CVS (convenience-store pickup) wire contract for the buyer storefront: validators for the
// FROZEN JSON of contracts/taiwan-cvs-logistics-v1.md §5.2/§16.1 (Go internal/buyerhttp/cvs.go via the BFF
// /api/buyer/cvs-selections[/{id}[/verify]] and /api/buyer/cvs-stores), plus the input mirrors the UI shows
// as early feedback. Owns: shapes, allowlists, store-code table, recipient mirror, checkout-draft storage.
// It never: talks to the network, decides money, or replaces a server rule (Go stays the authority on every
// write; these helpers only refuse malformed reads and give inline hints).
// External hosts named here (official store-search pages, ECPay map endpoints) are retrieved 2026-09-30
// (see the per-constant comments); we never fetch or scrape them (AGENTS.md: no chain private endpoints).

export const CVS_KINDS = [
  "cvs_711",
  "cvs_familymart",
  "cvs_hilife",
  "cvs_okmart",
] as const;
export type CvsKind = (typeof CVS_KINDS)[number];
export const isCvsKind = (v: unknown): v is CvsKind =>
  typeof v === "string" && (CVS_KINDS as readonly string[]).includes(v);

export const PAYMENT_MODES = ["card", "pay_at_pickup"] as const;
export type PaymentMode = (typeof PAYMENT_MODES)[number];
export const isPaymentMode = (v: unknown): v is PaymentMode =>
  typeof v === "string" && (PAYMENT_MODES as readonly string[]).includes(v);

const UUID = /^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/;
const isRecord = (v: unknown): v is Record<string, unknown> =>
  !!v && typeof v === "object" && !Array.isArray(v);
const exactKeys = (v: Record<string, unknown>, keys: string[]) =>
  Object.keys(v).sort().join() === [...keys].sort().join();
const isTime = (v: unknown): v is string =>
  typeof v === "string" && Number.isFinite(Date.parse(v));
const isString = (v: unknown, max: number): v is string =>
  typeof v === "string" && [...v].length <= max;

// ---- ECPay e-map form (§5.2): the only cross-site form the buyer page ever submits ---------------
// https://developers.ecpay.com.tw/ (electronic map, retrieved 2026-09-30); the action host list must equal
// the integrator's CSP form-action addition (output/cvs-ui/integrator-hooks.patch).
export const CVS_MAP_ACTIONS = [
  "https://logistics-stage.ecpay.com.tw/Express/map",
  "https://logistics.ecpay.com.tw/Express/map",
] as const;
const MAP_FIELDS = [
  "MerchantID",
  "MerchantTradeNo",
  "LogisticsType",
  "LogisticsSubType",
  "IsCollection",
  "ServerReplyURL",
  "Device",
];
const SUBTYPES = ["UNIMARTC2C", "FAMIC2C", "HILIFEC2C", "OKMARTC2C"];

export type MapForm = { action: string; fields: Record<string, string> };
export type CvsSelectionOpen = {
  selection_id: string;
  expires_at: string;
  form: MapForm;
};
// The action is checked against the allowlist so a compromised or buggy upstream can never make the
// browser POST the cart draft to another host; ServerReplyURL must be https.
export function validMapForm(v: unknown): v is MapForm {
  if (!isRecord(v) || !exactKeys(v, ["action", "fields"])) return false;
  if (!(CVS_MAP_ACTIONS as readonly string[]).includes(String(v.action)))
    return false;
  const f = v.fields;
  if (!isRecord(f) || !exactKeys(f, MAP_FIELDS)) return false;
  if (!MAP_FIELDS.every((k) => isString(f[k], 512) && (f[k] as string) !== ""))
    return false;
  return (
    f.LogisticsType === "CVS" &&
    f.IsCollection === "N" &&
    SUBTYPES.includes(String(f.LogisticsSubType)) &&
    /^https:\/\//.test(String(f.ServerReplyURL)) &&
    /^[01]$/.test(String(f.Device))
  );
}
export const validCvsSelectionOpen = (v: unknown): v is CvsSelectionOpen =>
  isRecord(v) &&
  exactKeys(v, ["selection_id", "expires_at", "form"]) &&
  typeof v.selection_id === "string" &&
  UUID.test(v.selection_id) &&
  isTime(v.expires_at) &&
  validMapForm(v.form);

// §5.2 allowlist: the product page the buyer checks out on; anything else is 422 bad_return_path in Go.
export const RETURN_PATH = /^\/(?:zh-TW|zh-CN|en)\/products\/[A-Za-z0-9_-]{1,64}$/;
export const validReturnPath = (v: unknown): v is string =>
  typeof v === "string" && RETURN_PATH.test(v);

// ---- selection projection (GET / verify) ------------------------------------------------------------
export const SELECTION_STATES = [
  "OPEN",
  "RETURNED",
  "VERIFIED",
  "REJECTED",
  "EXPIRED",
] as const;
export type SelectionState = (typeof SELECTION_STATES)[number];
export type CvsPickupRow = {
  pickup_id: string;
  kind: CvsKind;
  code: string;
  name: string;
  address: string;
  outside: boolean;
};
export type CvsSelection = {
  selection_id: string;
  state: SelectionState;
  reject_code: string | null;
  retry_after_s: number | null;
  pickup: CvsPickupRow | null;
};
const validPickupRow = (v: unknown): v is CvsPickupRow =>
  isRecord(v) &&
  exactKeys(v, ["pickup_id", "kind", "code", "name", "address", "outside"]) &&
  typeof v.pickup_id === "string" &&
  UUID.test(v.pickup_id) &&
  isCvsKind(v.kind) &&
  isString(v.code, 32) &&
  (v.code as string) !== "" &&
  isString(v.name, 120) &&
  (v.name as string) !== "" &&
  isString(v.address, 400) &&
  (v.address as string) !== "" &&
  typeof v.outside === "boolean";
// reject_code/retry_after_s are null when not applicable (Go emits the keys always; absent is read as null).
export function validCvsSelection(v: unknown): v is CvsSelection {
  if (!isRecord(v)) return false;
  const keys = ["selection_id", "state", "reject_code", "retry_after_s", "pickup"];
  const present = Object.keys(v).filter((k) => keys.includes(k));
  if (present.length !== Object.keys(v).length) return false;
  if (typeof v.selection_id !== "string" || !UUID.test(v.selection_id))
    return false;
  if (!(SELECTION_STATES as readonly string[]).includes(String(v.state)))
    return false;
  const reject = v.reject_code ?? null;
  const retry = v.retry_after_s ?? null;
  const pickup = v.pickup ?? null;
  if (
    !(reject === null || (typeof reject === "string" && /^[a-z_]{1,40}$/.test(reject)))
  )
    return false;
  if (
    !(
      retry === null ||
      (typeof retry === "number" && Number.isSafeInteger(retry) && retry >= 0 && retry <= 86400)
    )
  )
    return false;
  if (!(pickup === null || validPickupRow(pickup))) return false;
  // VERIFIED <=> a pickup row; REJECTED <=> a reject code (schema CHECKs of §4.2).
  return (v.state === "VERIFIED") === (pickup !== null) &&
    (v.state === "REJECTED") === (reject !== null);
}
export const normalizeSelection = (v: CvsSelection): CvsSelection => ({
  ...v,
  reject_code: v.reject_code ?? null,
  retry_after_s: v.retry_after_s ?? null,
  pickup: v.pickup ?? null,
});

// ---- buyer-entered store (§16.1) ---------------------------------------------------------------------
export type BuyerStore = {
  pickup_id: string;
  kind: CvsKind;
  code: string;
  name: string;
  address: string;
  source: "buyer_entered";
};
export const validBuyerStore = (v: unknown): v is BuyerStore =>
  isRecord(v) &&
  exactKeys(v, ["pickup_id", "kind", "code", "name", "address", "source"]) &&
  typeof v.pickup_id === "string" &&
  UUID.test(v.pickup_id) &&
  isCvsKind(v.kind) &&
  isString(v.code, 32) &&
  isString(v.name, 120) &&
  isString(v.address, 400) &&
  v.source === "buyer_entered";

// §16.1 store_code table (Go twin: fulfillment.ValidBuyerStoreCode). Hi-Life length is UNKNOWN upstream
// (3..8 digits accepted until evidence); codes stay strings so leading zeros survive.
const CODE_RULES: Record<CvsKind, RegExp> = {
  cvs_711: /^[0-9]{6}$/,
  cvs_familymart: /^[0-9]{6}$/,
  cvs_okmart: /^[0-9]{4}$/,
  cvs_hilife: /^[0-9]{3,8}$/,
};
export const validStoreCode = (kind: CvsKind, code: string) =>
  CODE_RULES[kind].test(code);
const NO_CONTROL = /[\p{Cc}\p{Zl}\p{Zp}]/u;
export const validStoreName = (v: string) => {
  const n = [...v.trim()].length;
  return n >= 1 && n <= 40 && !NO_CONTROL.test(v);
};
export const validStoreAddress = (v: string) => {
  const n = [...v.trim()].length;
  return n >= 5 && n <= 120 && !NO_CONTROL.test(v);
};
export type BuyerStoreBody = {
  cart_version: number;
  market_id: string;
  service_code: string;
  store_code: string;
  store_name: string;
  store_address: string;
};
export const validBuyerStoreBody = (
  v: unknown,
  kind: CvsKind,
): v is BuyerStoreBody =>
  isRecord(v) &&
  exactKeys(v, [
    "cart_version",
    "market_id",
    "service_code",
    "store_code",
    "store_name",
    "store_address",
  ]) &&
  typeof v.cart_version === "number" &&
  Number.isSafeInteger(v.cart_version) &&
  v.cart_version >= 1 &&
  typeof v.market_id === "string" &&
  UUID.test(v.market_id) &&
  typeof v.service_code === "string" &&
  /^[a-z][a-z0-9_-]{0,39}$/.test(v.service_code) &&
  typeof v.store_code === "string" &&
  validStoreCode(kind, v.store_code) &&
  typeof v.store_name === "string" &&
  v.store_name === v.store_name.trim() &&
  validStoreName(v.store_name) &&
  typeof v.store_address === "string" &&
  v.store_address === v.store_address.trim() &&
  validStoreAddress(v.store_address);

// Official store-search pages shown next to the code field (§16.1). Opened in a new tab with
// rel="noopener noreferrer"; never fetched or scraped. Retrieved 2026-09-30.
//  - 7-ELEVEN https://emap.pcsc.com.tw/  (official e-map)
//  - FamilyMart https://www.family.com.tw/Marketing/zh/Map: HTTP 403 to a script fetcher, but REACHABLE in a
//    real browser 2026-09-30 (page "店舖查詢"); evidence output/cvs-ui/familymart-link.txt. Fallback if it
//    ever fails: https://family.map.com.tw/famiport/storeNumberFreeze.aspx.
//  - Hi-Life https://www.hilife.com.tw/storeInquiry_street.aspx
//  - OK mart https://www.okmart.com.tw/convenient_shopSearch
export const FAMILYMART_LINK = "https://www.family.com.tw/Marketing/zh/Map";
export const CVS_SEARCH_LINKS: Record<CvsKind, string> = {
  cvs_711: "https://emap.pcsc.com.tw/",
  cvs_familymart: FAMILYMART_LINK,
  cvs_hilife: "https://www.hilife.com.tw/storeInquiry_street.aspx",
  cvs_okmart: "https://www.okmart.com.tw/convenient_shopSearch",
};

// ---- recipient mirror (§16.2 C3, F5: fulfillment.ecpay_recipient_ok) ---------------------------------------
// Name: no digits, ASCII symbols or emoji; width 4..10 where CJK/full-width counts 2. Phone: digits after
// removing spaces/hyphens/parentheses; +8869xxxxxxxx is read as 09xxxxxxxx; must be ^09[0-9]{8}$.
// This is input feedback only; Go/SQL re-check and answer 422 cvs_recipient_rejected.
const WIDE =
  /[ᄀ-ᅟ⺀-〾ぁ-꓏가-힣豈-﫿︰-﹯＀-｠￠-￦\u{20000}-\u{3fffd}]/u;
export function nameWidth(name: string): number {
  let width = 0;
  for (const ch of name) width += WIDE.test(ch) ? 2 : 1;
  return width;
}
export function recipientNameOK(name: string): boolean {
  const v = name.trim();
  if (v === "" || /[\p{N}\p{Extended_Pictographic}\p{Cc}]/u.test(v)) return false;
  if (/[!-/:-@[-`{-~]/.test(v)) return false;
  const w = nameWidth(v);
  return w >= 4 && w <= 10;
}
export function normalizeTwMobile(phone: string): string | null {
  const stripped = phone.replace(/[\s\-()]/g, "");
  const local = /^\+8869[0-9]{8}$/.test(stripped)
    ? `0${stripped.slice(4)}`
    : stripped;
  return /^09[0-9]{8}$/.test(local) ? local : null;
}

// ---- checkout draft (map round trip) -----------------------------------------------------------------------
// The map is a same-tab cross-site navigation, so the typed recipient inputs must survive it. sessionStorage
// (per tab, cleared when the tab closes) holds ONLY name, phone and payment mode; it is read once and removed
// on return, and is never written to localStorage or the URL. The cart itself is server-side.
export type CvsDraft = {
  v: 1;
  recipient_name: string;
  phone: string;
  payment_mode: PaymentMode;
};
type Store = Pick<Storage, "getItem" | "setItem" | "removeItem">;
const draftKey = (context: string) => `commerce-cvs-draft-v1:${context}`;
export function saveCvsDraft(store: Store, context: string, d: Omit<CvsDraft, "v">) {
  store.setItem(draftKey(context), JSON.stringify({ v: 1, ...d }));
}
export function takeCvsDraft(store: Store, context: string): CvsDraft | null {
  let raw: string | null;
  try {
    raw = store.getItem(draftKey(context));
    if (raw !== null) store.removeItem(draftKey(context));
  } catch {
    return null;
  }
  if (raw === null || raw.length > 2048) return null;
  try {
    const v: unknown = JSON.parse(raw);
    if (
      isRecord(v) &&
      exactKeys(v, ["v", "recipient_name", "phone", "payment_mode"]) &&
      v.v === 1 &&
      isString(v.recipient_name, 120) &&
      isString(v.phone, 32) &&
      isPaymentMode(v.payment_mode)
    )
      return v as CvsDraft;
  } catch {
    /* corrupt draft: the buyer retypes two short fields */
  }
  return null;
}
export function forgetCvsDraft(store: Store, context: string) {
  try {
    store.removeItem(draftKey(context));
  } catch {
    /* nothing to forget */
  }
}

// `?cvs_selection=<uuid>` is the only query the storefront product page accepts on return (§5.2); anything
// else (extra params, malformed id) is ignored rather than trusted.
export function cvsReturnID(search: string): string | null {
  const m = /^\?cvs_selection=([0-9a-f-]{36})$/.exec(search);
  return m && UUID.test(m[1]) ? m[1] : null;
}

// ---- error codes ----------------------------------------------------------------------------------------------
// Codes Go can answer on the buyer CVS/checkout routes (§5.2, §16.1, §16.2, taiwan-cvs cvs-core C7); the BFF
// passes exactly these through and the UI maps them to inline text. Unknown codes degrade to "unavailable".
export const CVS_ERROR_CODES = [
  "bad_return_path",
  "bad_return_origin",
  "service_unavailable",
  "selection_replay_new_key",
  "bad_store_code",
  "bad_store_name",
  "bad_store_address",
  "cvs_recipient_rejected",
  "cvs_amount_exceeds",
  "cvs_environment_mismatch",
  "cvs_source_mismatch",
  "pay_at_pickup_unavailable",
  "pay_at_pickup_amount_exceeds",
  "pay_at_pickup_limit",
] as const;
export type CvsErrorCode = (typeof CVS_ERROR_CODES)[number];
export const isCvsErrorCode = (v: unknown): v is CvsErrorCode =>
  typeof v === "string" && (CVS_ERROR_CODES as readonly string[]).includes(v);
// Definite refusals: nothing was committed even when the HTTP status is 429, so the client must not treat
// them as an uncertain outcome that locks the purchase (see buyer-server failure()).
export const DEFINITE_CVS_CODES: readonly string[] = ["pay_at_pickup_limit"];

// Which input a code belongs to, so the message renders next to it.
export function errorField(
  code: string,
): "store_code" | "store_name" | "store_address" | "recipient" | "store" | "payment" | null {
  switch (code) {
    case "bad_store_code":
      return "store_code";
    case "bad_store_name":
      return "store_name";
    case "bad_store_address":
      return "store_address";
    case "cvs_recipient_rejected":
      return "recipient";
    case "cvs_source_mismatch":
    case "cvs_environment_mismatch":
    case "service_unavailable":
      return "store";
    case "pay_at_pickup_unavailable":
    case "pay_at_pickup_amount_exceeds":
    case "pay_at_pickup_limit":
    case "cvs_amount_exceeds":
      return "payment";
    default:
      return null;
  }
}

// ---- buyer order projection (§5.3, §16.4, §16.8) ------------------------------------------------------------
export const BUYER_SHIPMENT_STATES = [
  "REQUESTED",
  "UNKNOWN",
  "FAILED",
  "ABANDONED",
  "CREATED",
  "AT_DC",
  "AT_STORE",
  "PICKED_UP",
  "UNCLAIMED",
] as const;
export type BuyerShipmentState = (typeof BUYER_SHIPMENT_STATES)[number];
export type BuyerCvsShipment = {
  state: BuyerShipmentState;
  chain: CvsKind;
  store_name: string;
  store_code: string;
  updated_at: string;
};
export const validBuyerCvsShipment = (v: unknown): v is BuyerCvsShipment =>
  isRecord(v) &&
  exactKeys(v, ["state", "chain", "store_name", "store_code", "updated_at"]) &&
  (BUYER_SHIPMENT_STATES as readonly string[]).includes(String(v.state)) &&
  isCvsKind(v.chain) &&
  isString(v.store_name, 120) &&
  isString(v.store_code, 32) &&
  isTime(v.updated_at);
export const COLLECTION_STATES = [
  "PENDING",
  "COLLECTED",
  "RETURNED",
  "REFUNDED_OFFLINE",
  "CANCELLED",
  "RESTOCKED",
] as const;
export type CollectionState = (typeof COLLECTION_STATES)[number];
export const isCollectionState = (v: unknown): v is CollectionState =>
  typeof v === "string" && (COLLECTION_STATES as readonly string[]).includes(v);

// Buyer-facing shipment key: REQUESTED/UNKNOWN/FAILED/ABANDONED are one "processing" text (§5.3: never an
// error detail); "delivered" wording exists only for PICKED_UP.
export function shipmentCopyKey(
  state: BuyerShipmentState,
): "processing" | "CREATED" | "AT_DC" | "AT_STORE" | "PICKED_UP" | "UNCLAIMED" {
  return state === "REQUESTED" ||
    state === "UNKNOWN" ||
    state === "FAILED" ||
    state === "ABANDONED"
    ? "processing"
    : state;
}
