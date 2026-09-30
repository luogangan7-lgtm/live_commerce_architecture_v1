// Admin customers/finance model: strict parsers for the frozen Go DTOs (BFF `/api/stores/{store}/customers*`,
// `finance/summary` -> Go `internal/customers` + `internal/reporting`, contract customers-billing-v1 §5, customers-core
// FROZEN block). Unknown key, wrong type or broken invariant = invalid response (thrown "unavailable"); the server
// stays the authority for consent, erasure and money, these parsers only refuse malformed reads.
// Depends on orders-model.ts (`parseOrderSummary`: the detail's orders[] is the merchant-orders Summary shape).
import { canonicalCursor, canonicalUUID, parseOrderSummary, type OrderSummary } from "./orders-model.ts";

export type ConsentPurpose = "marketing_messages" | "ads_personalization";
export type ConsentChannel = "meta_dm" | "meta_ads";
export const consentPairs: readonly { purpose: ConsentPurpose; channel: ConsentChannel }[] = [
  { purpose: "marketing_messages", channel: "meta_dm" },
  { purpose: "ads_personalization", channel: "meta_ads" },
];
export type Consents = { marketing_messages: boolean; ads_personalization: boolean };
export type Customer = {
  customer_id: string;
  first_seen_at: string;
  last_activity_at: string;
  display_name: string | null;
  phone_last3: string | null;
  orders_count: number;
  paid_orders_count: number;
  captured_minor: number;
  refunded_minor: number;
  currency: string | null;
  claims_count: number;
  platforms: string[];
  consents: Consents;
  active: boolean;
};
export type CustomerList = { items: Customer[]; next_cursor: string };
export type ClaimSummary = { session_id: string; platform: string; bound_at: string; line_count: number };
export type ConsentEvent = {
  purpose: ConsentPurpose;
  channel: ConsentChannel;
  granted: boolean;
  source: string;
  policy_version: string;
  occurred_at: string;
};
export type PrivacyAction = { kind: string; via: string; completed_at: string; summary: Record<string, unknown> | null };
export type CustomerDetail = Customer & {
  orders: OrderSummary[];
  claims: ClaimSummary[];
  consent_history: ConsentEvent[];
  privacy_actions: PrivacyAction[];
};
export type FinanceRow = {
  day: string;
  currency: string;
  environment: string;
  captured_count: number;
  captured_minor: number;
  refunded_minor: number;
  net_minor: number;
};
export type FinanceSummary = { from: string; to: string; timezone: string; rows: FinanceRow[]; totals: FinanceRow[] };
export type ErasureSummary = {
  consents_withdrawn: number;
  sessions_revoked: number;
  snapshots_redacted: number;
  bundles_relabelled: number;
};

export const maxListItems = 100;
const maxMoney = 1_000_000_000_000;
// Go time.Time / SQL to_char output: RFC3339 UTC, fraction of any length (contract does not pin the digits).
const instant = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?Z$/;
const token = /^[a-z][a-z0-9_-]{0,31}$/;
const privacyKinds = ["EXPORT", "ERASURE"];
const privacyVia = ["buyer", "merchant", "restore"];

export function object(value: unknown, keys: string[]): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("unavailable");
  const result = value as Record<string, unknown>;
  if (Object.keys(result).sort().join(",") !== [...keys].sort().join(",")) throw new Error("unavailable");
  return result;
}
export function isInstant(value: unknown): value is string {
  return typeof value === "string" && instant.test(value) && Number.isFinite(Date.parse(value));
}
export function count(value: unknown): value is number {
  return Number.isSafeInteger(value) && (value as number) >= 0 && (value as number) <= maxMoney;
}
function short(value: unknown, max: number): value is string {
  return typeof value === "string" && Array.from(value).length <= max && !/[\p{C}\p{Zl}\p{Zp}]/u.test(value);
}
function nullableText(value: unknown, max: number): value is string | null {
  return value === null || (short(value, max) && value !== "");
}
function flag(value: unknown): value is boolean {
  return typeof value === "boolean";
}

function parseConsents(value: unknown): Consents {
  const v = object(value, ["marketing_messages", "ads_personalization"]);
  if (!flag(v.marketing_messages) || !flag(v.ads_personalization)) throw new Error("unavailable");
  return v as Consents;
}
const customerKeys = [
  "customer_id", "first_seen_at", "last_activity_at", "display_name", "phone_last3", "orders_count",
  "paid_orders_count", "captured_minor", "refunded_minor", "currency", "claims_count", "platforms", "consents", "active",
];
function customerFrom(v: Record<string, unknown>): Customer {
  if (typeof v.customer_id !== "string" || !canonicalUUID.test(v.customer_id) ||
    !isInstant(v.first_seen_at) || !isInstant(v.last_activity_at) ||
    !nullableText(v.display_name, 120) || !(v.phone_last3 === null || /^[0-9]{3}$/.test(String(v.phone_last3))) ||
    !count(v.orders_count) || !count(v.paid_orders_count) || v.paid_orders_count > v.orders_count ||
    !count(v.captured_minor) || !count(v.refunded_minor) || v.refunded_minor > v.captured_minor ||
    !(v.currency === null || /^[A-Z]{3}$/.test(String(v.currency))) || !count(v.claims_count) ||
    !Array.isArray(v.platforms) || v.platforms.length > 8 ||
    !v.platforms.every((platform) => typeof platform === "string" && token.test(platform)) ||
    new Set(v.platforms).size !== v.platforms.length || !flag(v.active))
    throw new Error("unavailable");
  // An erased owner (active=false) never shows a granted consent: consent_allows is false for it (CD4/CD7).
  const consents = parseConsents(v.consents);
  if (!v.active && (consents.marketing_messages || consents.ads_personalization)) throw new Error("unavailable");
  return { ...(v as Omit<Customer, "consents">), consents };
}
export function parseCustomer(value: unknown): Customer {
  return customerFrom(object(value, customerKeys));
}

export function parseCustomerList(value: unknown): CustomerList {
  const v = object(value, ["items", "next_cursor"]);
  if (!Array.isArray(v.items) || v.items.length > maxListItems || typeof v.next_cursor !== "string" ||
    (v.next_cursor !== "" && !canonicalCursor.test(v.next_cursor))) throw new Error("unavailable");
  const items = v.items.map(parseCustomer);
  if (new Set(items.map((item) => item.customer_id)).size !== items.length) throw new Error("unavailable");
  return { items, next_cursor: v.next_cursor };
}

function parseConsentEvent(value: unknown): ConsentEvent {
  const v = object(value, ["purpose", "channel", "granted", "source", "policy_version", "occurred_at"]);
  if (!consentPairs.some((pair) => pair.purpose === v.purpose && pair.channel === v.channel) ||
    !flag(v.granted) || typeof v.source !== "string" || !token.test(v.source) ||
    !short(v.policy_version, 32) || !isInstant(v.occurred_at)) throw new Error("unavailable");
  return v as ConsentEvent;
}
function parseClaim(value: unknown): ClaimSummary {
  const v = object(value, ["session_id", "platform", "bound_at", "line_count"]);
  if (typeof v.session_id !== "string" || !canonicalUUID.test(v.session_id) || typeof v.platform !== "string" ||
    !token.test(v.platform) || !isInstant(v.bound_at) || !count(v.line_count)) throw new Error("unavailable");
  return v as ClaimSummary;
}
function parsePrivacyAction(value: unknown): PrivacyAction {
  const v = object(value, ["kind", "via", "completed_at", "summary"]);
  // Exact Go enums (internal/customers validPrivacyAction): kind EXPORT|ERASURE, via buyer|merchant|restore.
  if (typeof v.kind !== "string" || !privacyKinds.includes(v.kind) || typeof v.via !== "string" || !privacyVia.includes(v.via) ||
    !isInstant(v.completed_at)) throw new Error("unavailable");
  const summary = v.summary;
  if (summary !== null && (typeof summary !== "object" || Array.isArray(summary))) throw new Error("unavailable");
  return { kind: v.kind, via: v.via, completed_at: v.completed_at, summary: summary as Record<string, unknown> | null };
}

export function parseCustomerDetail(value: unknown, requestedID: string): CustomerDetail {
  const v = object(value, [...customerKeys, "orders", "claims", "consent_history", "privacy_actions"]);
  const head = customerFrom(Object.fromEntries(customerKeys.map((key) => [key, v[key]])));
  // Bounds: newest 50 orders (§7); the other lists are bounded by what one owner can hold in v1.
  if (head.customer_id !== requestedID || !Array.isArray(v.orders) || v.orders.length > 50 ||
    !Array.isArray(v.claims) || v.claims.length > 500 || !Array.isArray(v.consent_history) ||
    v.consent_history.length > 1000 || !Array.isArray(v.privacy_actions) || v.privacy_actions.length > 100)
    throw new Error("unavailable");
  const orders = v.orders.map(parseOrderSummary);
  if (new Set(orders.map((order) => order.order_id)).size !== orders.length) throw new Error("unavailable");
  return {
    ...head,
    orders,
    claims: v.claims.map(parseClaim),
    consent_history: v.consent_history.map(parseConsentEvent),
    privacy_actions: v.privacy_actions.map(parsePrivacyAction),
  };
}

export function parseErasureSummary(value: unknown): ErasureSummary {
  const v = object(value, ["consents_withdrawn", "sessions_revoked", "snapshots_redacted", "bundles_relabelled"]);
  if (![v.consents_withdrawn, v.sessions_revoked, v.snapshots_redacted, v.bundles_relabelled].every(count))
    throw new Error("unavailable");
  return v as ErasureSummary;
}

const rowKeys = ["day", "currency", "environment", "captured_count", "captured_minor", "refunded_minor", "net_minor"];
function parseFinanceRow(value: unknown, total: boolean): FinanceRow {
  const v = object(value, rowKeys);
  if (!(total ? v.day === "" : typeof v.day === "string" && /^\d{4}-\d{2}-\d{2}$/.test(v.day)) ||
    typeof v.currency !== "string" || !/^[A-Z]{3}$/.test(v.currency) ||
    typeof v.environment !== "string" || !/^[A-Z_]{1,16}$/.test(v.environment) ||
    !count(v.captured_count) || !count(v.captured_minor) || !count(v.refunded_minor) ||
    !Number.isSafeInteger(v.net_minor) || Math.abs(v.net_minor as number) > maxMoney ||
    // I05: net is exactly captured minus refunded; a report that disagrees with itself is refused, not shown.
    v.net_minor !== (v.captured_minor as number) - (v.refunded_minor as number)) throw new Error("unavailable");
  return v as FinanceRow;
}
export function parseFinanceSummary(value: unknown): FinanceSummary {
  const v = object(value, ["from", "to", "timezone", "rows", "totals"]);
  if (typeof v.from !== "string" || !/^\d{4}-\d{2}-\d{2}$/.test(v.from) ||
    typeof v.to !== "string" || !/^\d{4}-\d{2}-\d{2}$/.test(v.to) ||
    typeof v.timezone !== "string" || !short(v.timezone, 64) || v.timezone === "" ||
    !Array.isArray(v.rows) || v.rows.length > 2000 || !Array.isArray(v.totals) || v.totals.length > 20)
    throw new Error("unavailable");
  return {
    from: v.from,
    to: v.to,
    timezone: v.timezone,
    rows: v.rows.map((row) => parseFinanceRow(row, false)),
    totals: v.totals.map((row) => parseFinanceRow(row, true)),
  };
}

// Test-mode flag for the finance page: anything not LIVE is sandbox money (contract BD8, stripe-psp-v1 SANDBOX).
export const isSandbox = (row: FinanceRow) => row.environment !== "LIVE";
// Today's UTC+8 finance day (Q11) as YYYY-MM-DD, used only for the date inputs' defaults and max.
export function financeDay(now: Date, offsetDays = 0): string {
  const shifted = new Date(now.getTime() + 8 * 3_600_000 + offsetDays * 86_400_000);
  return shifted.toISOString().slice(0, 10);
}
// Fresh Idempotency-Key per dialog open / action (settings-client key alphabet).
export const newKey = (prefix: string) => `${prefix}-${crypto.randomUUID()}`;
