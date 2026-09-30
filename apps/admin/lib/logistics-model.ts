// Admin logistics model: strict parsers for the CVS / ECPay DTOs of contracts/taiwan-cvs-logistics-v1.md §8 and
// §16.5 (BFF `/api/stores/{store}/logistics/{ecpay,cvs-settings}` and `/orders/{id}/cvs-shipment*` ->
// Go internal/httpapi/cvs.go) plus the input mirrors the forms use for early feedback.
// Owns: exact response shapes (unknown keys refuse the whole read, like orders-model), allowlists for the signed
// print form, and the request-body builders that put exactly the frozen keys on the wire.
// It never decides eligibility, money or permission: Go/SQL stay the authority for every write; a parser only
// refuses a malformed read. No secret is parsed or kept here: ECPay keys are write-only and absent from every DTO.

export const CHAINS = ["cvs_711", "cvs_familymart", "cvs_hilife", "cvs_okmart"] as const;
export type Chain = (typeof CHAINS)[number];
export const isChain = (v: unknown): v is Chain =>
  typeof v === "string" && (CHAINS as readonly string[]).includes(v);

function object(value: unknown, keys: string[]): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("unavailable");
  const result = value as Record<string, unknown>;
  if (Object.keys(result).sort().join(",") !== [...keys].sort().join(",")) throw new Error("unavailable");
  return result;
}
const isTime = (v: unknown): v is string => typeof v === "string" && Number.isFinite(Date.parse(v));
const isInt = (v: unknown, min: number, max: number): v is number =>
  typeof v === "number" && Number.isSafeInteger(v) && v >= min && v <= max;
const isText = (v: unknown, max: number): v is string =>
  typeof v === "string" && v.length <= max && !/[\p{Cc}\p{Zl}\p{Zp}]/u.test(v);
const oneOf = <T extends string>(v: unknown, values: readonly T[]): v is T =>
  typeof v === "string" && (values as readonly string[]).includes(v);

// ---- ECPay connection card (§8 GET/PUT logistics/ecpay) -------------------------------------------------
export const ECPAY_MODES = ["C2C", "B2C"] as const;
export const ECPAY_ENVIRONMENTS = ["SANDBOX", "LIVE"] as const;
export type EcpayEnvironment = (typeof ECPAY_ENVIRONMENTS)[number];
export type EcpayMode = (typeof ECPAY_MODES)[number];
export type EcpayConnection = {
  environment: EcpayEnvironment;
  mode: EcpayMode;
  merchant_id: string;
  version: number;
  enabled: boolean;
  qualified_at: string | null;
  ok_verified: boolean;
  status_url: string;
};
// status_url is the route ECPay posts status to (read-only, copyable); it must be a plain https URL.
export const validStatusURL = (v: unknown): v is string => {
  if (typeof v !== "string" || v.length > 512 || !v.startsWith("https://") || /[\s\u0000-\u001f#]/.test(v)) return false;
  try {
    const u = new URL(v);
    return u.protocol === "https:" && !u.username && !u.password && !u.search && !u.hash && u.hostname.includes(".");
  } catch {
    return false;
  }
};
export function parseEcpay(value: unknown): EcpayConnection {
  const v = object(value, [
    "environment", "mode", "merchant_id", "version", "enabled", "qualified_at", "ok_verified", "status_url",
  ]);
  if (
    !oneOf(v.environment, ECPAY_ENVIRONMENTS) || !oneOf(v.mode, ECPAY_MODES) ||
    typeof v.merchant_id !== "string" || !/^[0-9A-Za-z]{1,16}$/.test(v.merchant_id) ||
    !isInt(v.version, 1, Number.MAX_SAFE_INTEGER) || typeof v.enabled !== "boolean" ||
    !(v.qualified_at === null || isTime(v.qualified_at)) || typeof v.ok_verified !== "boolean" ||
    !validStatusURL(v.status_url) ||
    // An enabled profile is always qualified (the enable definer requires it, §4.2).
    (v.enabled && v.qualified_at === null)
  )
    throw new Error("unavailable");
  return v as EcpayConnection;
}
// A connection that can be chosen as "API (ECPay)" for a CVS delivery service (§4.1 predicate).
export const ecpayQualified = (c: EcpayConnection | null) => !!c && c.enabled && c.qualified_at !== null;

// PUT body, exactly the frozen keys (§8). Keys/IV/sender are typed by the merchant and never re-displayed.
export type EcpayConnectInput = {
  expected_version: number;
  environment: EcpayEnvironment;
  mode: EcpayMode;
  merchant_id: string;
  hash_key: string;
  hash_iv: string;
  sender_name: string;
  sender_cell_phone: string;
};
export const validMerchantID = (v: string) => /^[0-9A-Za-z]{1,16}$/.test(v);
// ECPay HashKey/HashIV are short ASCII secrets; only the shape is checked here (16 chars is the documented size
// for logistics accounts; 8..64 printable ASCII keeps this from rejecting a variant that Go's probe would accept).
export const validSecretText = (v: string) => /^[\x21-\x7e]{8,64}$/.test(v);
export const validSenderName = (v: string) => {
  const n = [...v.trim()].length;
  return n >= 1 && n <= 10 && !/[\p{Cc}\p{N}]/u.test(v);
};
export const validSenderPhone = (v: string) => /^09[0-9]{8}$/.test(v.trim());
export function ecpayConnectBody(f: {
  expectedVersion: number; environment: EcpayEnvironment; mode: EcpayMode; merchantID: string;
  hashKey: string; hashIV: string; senderName: string; senderPhone: string;
}): EcpayConnectInput | null {
  if (
    !isInt(f.expectedVersion, 0, Number.MAX_SAFE_INTEGER - 1) || !oneOf(f.environment, ECPAY_ENVIRONMENTS) ||
    !oneOf(f.mode, ECPAY_MODES) || !validMerchantID(f.merchantID.trim()) || !validSecretText(f.hashKey) ||
    !validSecretText(f.hashIV) || !validSenderName(f.senderName) || !validSenderPhone(f.senderPhone)
  )
    return null;
  return {
    expected_version: f.expectedVersion, environment: f.environment, mode: f.mode,
    merchant_id: f.merchantID.trim(), hash_key: f.hashKey, hash_iv: f.hashIV,
    sender_name: f.senderName.trim(), sender_cell_phone: f.senderPhone.trim(),
  };
}

// ---- CVS store settings card (§16.5 GET/PUT logistics/cvs-settings) ---------------------------------------
export type CvsSettings = {
  version: number;
  enabled_chains: Chain[];
  pay_at_pickup_enabled: boolean;
  pay_at_pickup_max_twd: number | null;
  pay_at_pickup_max_open: number;
};
export function parseCvsSettings(value: unknown): CvsSettings {
  const v = object(value, [
    "version", "enabled_chains", "pay_at_pickup_enabled", "pay_at_pickup_max_twd", "pay_at_pickup_max_open",
  ]);
  if (
    !isInt(v.version, 0, Number.MAX_SAFE_INTEGER) || !Array.isArray(v.enabled_chains) ||
    !v.enabled_chains.every(isChain) || new Set(v.enabled_chains).size !== v.enabled_chains.length ||
    typeof v.pay_at_pickup_enabled !== "boolean" ||
    !(v.pay_at_pickup_max_twd === null || isInt(v.pay_at_pickup_max_twd, 1, 20000)) ||
    !isInt(v.pay_at_pickup_max_open, 1, 500) ||
    // Schema CHECK: enabling pay-at-pickup needs a maximum amount.
    (v.pay_at_pickup_enabled && v.pay_at_pickup_max_twd === null)
  )
    throw new Error("unavailable");
  return v as CvsSettings;
}
export type CvsSettingsInput = { expected_version: number } & Omit<CvsSettings, "version">;
// Text -> body; null when a field is outside §16.5's ranges (1..20000 TWD, 1..500 open orders).
export function cvsSettingsBody(f: {
  expectedVersion: number; chains: Chain[]; payAtPickup: boolean; maxTWD: string; maxOpen: string;
}): CvsSettingsInput | null {
  const whole = (t: string) => (/^[0-9]{1,6}$/.test(t.trim()) ? Number(t.trim()) : null);
  const max = whole(f.maxTWD);
  const open = whole(f.maxOpen);
  if (
    !isInt(f.expectedVersion, 0, Number.MAX_SAFE_INTEGER - 1) || !f.chains.every(isChain) ||
    new Set(f.chains).size !== f.chains.length || open === null || open < 1 || open > 500 ||
    (max !== null && (max < 1 || max > 20000)) || (f.payAtPickup && max === null) ||
    // the server default keeps a max even when pay-at-pickup is off; an empty box sends null
    (f.maxTWD.trim() !== "" && max === null)
  )
    return null;
  return {
    expected_version: f.expectedVersion,
    enabled_chains: CHAINS.filter((c) => f.chains.includes(c)),
    pay_at_pickup_enabled: f.payAtPickup,
    pay_at_pickup_max_twd: max,
    pay_at_pickup_max_open: open,
  };
}

// ---- order CVS shipment (§8 GET orders/{id}/cvs-shipment, no PII, no trade number) ------------------------
export const SHIPMENT_STATES = [
  "REQUESTED", "UNKNOWN", "FAILED", "ABANDONED", "CREATED", "AT_DC", "AT_STORE", "PICKED_UP", "UNCLAIMED",
] as const;
export type ShipmentState = (typeof SHIPMENT_STATES)[number];
export type ShipmentEvent = {
  source: string;
  event_code: string;
  provider_code: string | null;
  provider_message: string | null;
  from_state: string | null;
  to_state: string | null;
  received_at: string;
};
export type CvsAttempt = {
  attempt: number;
  state: ShipmentState;
  subtype: string;
  environment: EcpayEnvironment;
  receiver_store_id: string;
  goods_amount: number;
  collection_amount: number | null;
  provider_logistics_id: string | null;
  code: string | null;
  print_available: boolean;
  result_code: string | null;
  last_status_code: string | null;
  last_status_at: string | null;
  alerts: string[];
  created_at: string;
  updated_at: string;
  version: number;
  events: ShipmentEvent[];
};
export type CvsShipment = {
  current_attempt: number | null;
  expected_version: number;
  validity_days: number | null;
  attempts: CvsAttempt[];
};
const nullableText = (v: unknown, max: number) => v === null || isText(v, max);
const eventKeys = ["source", "event_code", "provider_code", "provider_message", "from_state", "to_state", "received_at"];
const attemptKeys = [
  "attempt", "state", "subtype", "environment", "receiver_store_id", "goods_amount", "collection_amount",
  "provider_logistics_id", "code", "print_available", "result_code", "last_status_code", "last_status_at",
  "alerts", "created_at", "updated_at", "version", "events",
];
function parseEvent(raw: unknown): ShipmentEvent {
  const e = object(raw, eventKeys);
  if (
    !isText(e.source, 40) || !isText(e.event_code, 80) || !nullableText(e.provider_code, 40) ||
    !nullableText(e.provider_message, 400) || !nullableText(e.from_state, 40) || !nullableText(e.to_state, 40) ||
    !isTime(e.received_at)
  )
    throw new Error("unavailable");
  return e as ShipmentEvent;
}
function parseAttempt(raw: unknown): CvsAttempt {
  const a = object(raw, attemptKeys);
  if (
    !isInt(a.attempt, 1, 1000) || !oneOf(a.state, SHIPMENT_STATES) || !isText(a.subtype, 40) ||
    !oneOf(a.environment, ECPAY_ENVIRONMENTS) || !isText(a.receiver_store_id, 32) ||
    !isInt(a.goods_amount, 1, 20000) || !(a.collection_amount === null || isInt(a.collection_amount, 1, 20000)) ||
    !nullableText(a.provider_logistics_id, 40) || !(a.code === null || (typeof a.code === "string" && /^[A-Za-z0-9 -]{1,40}$/.test(a.code))) ||
    typeof a.print_available !== "boolean" || !nullableText(a.result_code, 80) || !nullableText(a.last_status_code, 8) ||
    !(a.last_status_at === null || isTime(a.last_status_at)) || !Array.isArray(a.alerts) || a.alerts.length > 10 ||
    !a.alerts.every((x) => typeof x === "string" && /^[a-z_]{1,64}$/.test(x)) || !isTime(a.created_at) ||
    !isTime(a.updated_at) || !isInt(a.version, 1, Number.MAX_SAFE_INTEGER) || !Array.isArray(a.events) ||
    a.events.length > 500
  )
    throw new Error("unavailable");
  // §4.2: a printable label exists only once ECPay created it; never claim print for another state.
  if (a.print_available && !["CREATED", "AT_DC", "AT_STORE", "PICKED_UP", "UNCLAIMED"].includes(a.state as string))
    throw new Error("unavailable");
  return { ...(a as Omit<CvsAttempt, "events">), events: a.events.map(parseEvent) };
}
export function parseCvsShipment(value: unknown): CvsShipment {
  const v = object(value, ["current_attempt", "expected_version", "validity_days", "attempts"]);
  if (
    !(v.current_attempt === null || isInt(v.current_attempt, 1, 1000)) ||
    !isInt(v.expected_version, 0, Number.MAX_SAFE_INTEGER) ||
    !(v.validity_days === null || isInt(v.validity_days, 1, 60)) || !Array.isArray(v.attempts) || v.attempts.length > 5
  )
    throw new Error("unavailable");
  const attempts = v.attempts.map(parseAttempt);
  const numbers = attempts.map((a) => a.attempt);
  if (
    new Set(numbers).size !== numbers.length ||
    (v.current_attempt === null ? attempts.length !== 0 : !numbers.includes(v.current_attempt as number))
  )
    throw new Error("unavailable");
  return { ...(v as Omit<CvsShipment, "attempts">), attempts };
}
export const currentAttempt = (s: CvsShipment | null): CvsAttempt | null =>
  s?.attempts.find((a) => a.attempt === s.current_attempt) ?? null;

// POST cvs-shipment 202
export type CvsSubmitted = { attempt: number; state: "REQUESTED"; operation_id: string };
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
export function parseCvsSubmitted(value: unknown): CvsSubmitted {
  const v = object(value, ["attempt", "state", "operation_id"]);
  if (!isInt(v.attempt, 1, 1000) || v.state !== "REQUESTED" || typeof v.operation_id !== "string" || !UUID.test(v.operation_id))
    throw new Error("unavailable");
  return v as CvsSubmitted;
}

// POST print-form 200: a signed ECPay form the print tab posts top-level. The action must be one of the two ECPay
// logistics hosts and one of the four documented print paths (§8, F8); fields are plain strings.
export type PrintForm = { action: string; fields: Record<string, string> };
const PRINT_HOSTS = ["logistics-stage.ecpay.com.tw", "logistics.ecpay.com.tw"];
const PRINT_PATHS = [
  "/express/printunimartc2corderinfo", "/express/printfamic2corderinfo", "/express/printhilifec2corderinfo",
  "/helper/printtradedocument",
];
export function validPrintAction(v: unknown): v is string {
  if (typeof v !== "string" || v.length > 256 || !v.startsWith("https://") || /[\s#?]/.test(v)) return false;
  try {
    const u = new URL(v);
    return u.protocol === "https:" && !u.username && !u.password && !u.port && !u.search && !u.hash &&
      PRINT_HOSTS.includes(u.hostname) && PRINT_PATHS.includes(u.pathname.toLowerCase());
  } catch {
    return false;
  }
}
export function parsePrintForm(value: unknown): PrintForm {
  const v = object(value, ["action", "fields"]);
  const f = v.fields;
  if (
    !validPrintAction(v.action) || !f || typeof f !== "object" || Array.isArray(f) ||
    Object.keys(f).length < 1 || Object.keys(f).length > 30 ||
    !Object.entries(f as Record<string, unknown>).every(
      ([k, x]) => /^[A-Za-z][A-Za-z0-9]{0,39}$/.test(k) && typeof x === "string" && x.length <= 2048,
    )
  )
    throw new Error("unavailable");
  return v as PrintForm;
}
// The body of print-form is exactly {thermal} (cvs-core C11).
export const printBody = (thermal: boolean) => JSON.stringify({ thermal });

// abandon body: exactly {expected_version, i_checked_ecpay_backend}; request body exactly {expected_version}.
export const requestBody = (expectedVersion: number) => JSON.stringify({ expected_version: expectedVersion });
export const abandonBody = (expectedVersion: number, checked: boolean) =>
  JSON.stringify({ expected_version: expectedVersion, i_checked_ecpay_backend: checked });

// ---- pay-at-pickup collection / release (§16.4, §16.8) ------------------------------------------------------
export const COLLECTION_STATES = ["PENDING", "COLLECTED", "RETURNED", "REFUNDED_OFFLINE", "CANCELLED", "RESTOCKED"] as const;
export type CollectionState = (typeof COLLECTION_STATES)[number];
export const isCollectionState = (v: unknown): v is CollectionState => oneOf(v, COLLECTION_STATES);
export type CollectionAction = "collected" | "returned" | "refunded_offline";
export const collectionBody = (expected: CollectionState, state: CollectionAction) =>
  JSON.stringify({ expected_state: expected, state });
export type ReleaseAction = "cancel" | "restock";
export const releaseBody = (action: ReleaseAction, expected: CollectionState) =>
  JSON.stringify({ action, expected_state: expected });
export function parseCollectionResult(value: unknown): { order_id: string; collection_state: CollectionState } {
  const v = object(value, ["order_id", "collection_state"]);
  if (typeof v.order_id !== "string" || !UUID.test(v.order_id) || !isCollectionState(v.collection_state)) throw new Error("unavailable");
  return v as { order_id: string; collection_state: CollectionState };
}
export function parseReleaseResult(value: unknown) {
  const v = object(value, ["order_id", "collection_state", "commercial_state", "released_lines"]);
  if (
    typeof v.order_id !== "string" || !UUID.test(v.order_id) || !isCollectionState(v.collection_state) ||
    !oneOf(v.commercial_state, ["DRAFT", "AWAITING_PAYMENT", "CONFIRMED", "CANCELLED"] as const) ||
    !isInt(v.released_lines, 0, 1000)
  )
    throw new Error("unavailable");
  return v as { order_id: string; collection_state: CollectionState; commercial_state: string; released_lines: number };
}

// Which pay-at-pickup buttons to offer (a hint only; every rule is re-checked by SQL, §16.4/§16.8):
//  collected/returned  need a shipped order and PENDING; refunded_offline needs COLLECTED;
//  cancel              needs PENDING and an unshipped order (MANUAL_UNASSIGNED);
//  restock             needs RETURNED.
export function collectionActions(
  state: CollectionState | null,
  fulfillment: string,
): { collected: boolean; returned: boolean; refunded_offline: boolean; cancel: boolean; restock: boolean } {
  const shipped = fulfillment === "MERCHANT_SHIPPED" || fulfillment === "PROVIDER_LABEL_CREATED";
  return {
    collected: state === "PENDING" && shipped,
    returned: state === "PENDING" && shipped,
    refunded_offline: state === "COLLECTED",
    cancel: state === "PENDING" && fulfillment === "MANUAL_UNASSIGNED",
    restock: state === "RETURNED",
  };
}

// The "lapsed" hint for abandoning a CREATED label: DB time decides (F9), this only avoids offering the button
// when the validity window clearly has not passed.
export function labelLapsed(a: CvsAttempt, validityDays: number | null, now: number): boolean {
  return validityDays !== null && now > Date.parse(a.created_at) + validityDays * 86_400_000;
}
