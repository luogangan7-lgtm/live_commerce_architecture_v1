// Admin order model: strict parsers for the merchant-orders DTOs (BFF `/api/stores/{store}/orders*`
// -> Go `internal/merchantorders`, `internal/httpapi/{orders,refunds,shipments}.go`).
// Invariants mirror merchant-orders-v1 as amended by stripe-refund-v1 §7.1 and manual-fulfilment-v1 §5.1;
// the server stays the authority for every write, these parsers only refuse malformed reads.
// CVS (contracts/taiwan-cvs-logistics-v1.md §16, cvs-core C4): summary/detail add pickup_source, payment_mode and
// collection_state; a pay-at-pickup order is CONFIRMED with no payment (NOT_STARTED, no work item), so the payment
// invariants below are relaxed for exactly that mode and nowhere else.
const commercialStates = ["DRAFT", "AWAITING_PAYMENT", "CONFIRMED", "CANCELLED"] as const;
// `shipped`/`unshipped` are server-side list filters (manual-fulfilment-v1 §5.1), not order states.
export const orderStates = [
  "all",
  ...commercialStates,
  "shipped",
  "unshipped",
  // PROVIDER_LABEL_CREATED with a CREATED current attempt: the forwarder's daily drop list (§8).
  "cvs_pending",
] as const;
export type OrderFilter = (typeof orderStates)[number];
export type CommercialState = (typeof commercialStates)[number];
const fulfillmentStates = [
  "MANUAL_UNASSIGNED", "MERCHANT_SHIPPED", "CANCELLED", "PAID_ALLOCATION_FAILED", "PROVIDER_LABEL_CREATED",
] as const;
export type FulfillmentState = (typeof fulfillmentStates)[number];
const paymentStates = [
  "NOT_STARTED", "PENDING", "AUTHORIZED", "CAPTURED", "PARTIALLY_REFUNDED", "REFUNDED", "REVIEW_REQUIRED",
] as const;
export type PaymentState = (typeof paymentStates)[number];
const capturedStates: readonly PaymentState[] = ["CAPTURED", "PARTIALLY_REFUNDED", "REFUNDED", "REVIEW_REQUIRED"];
export type WorkState = "NONE" | "READY" | "REVIEW_REQUIRED";
// §16.1: where the pickup store came from (null for a home order).
export const pickupSources = ["ecpay_directory", "buyer_entered", "merchant_attested"] as const;
export type PickupSource = (typeof pickupSources)[number];
export const paymentModes = ["card", "pay_at_pickup"] as const;
export type PaymentMode = (typeof paymentModes)[number];
export const collectionStates = ["PENDING", "COLLECTED", "RETURNED", "REFUNDED_OFFLINE", "CANCELLED", "RESTOCKED"] as const;
export type CollectionState = (typeof collectionStates)[number];
const cvsKinds = ["cvs_711", "cvs_familymart", "cvs_hilife", "cvs_okmart"] as const;
export type CvsKind = (typeof cvsKinds)[number];
export const verificationKinds = ["MANUAL_ATTESTED", "PROVIDER_DIRECTORY_VERIFIED", "BUYER_ENTERED"] as const;
// The pickup_source shown to the merchant is derived from the pickup's verification kind (§16.1).
const sourceOf: Record<(typeof verificationKinds)[number], PickupSource> = {
  MANUAL_ATTESTED: "merchant_attested",
  PROVIDER_DIRECTORY_VERIFIED: "ecpay_directory",
  BUYER_ENTERED: "buyer_entered",
};

export type OrderSummary = {
  order_id: string;
  created_at: string;
  updated_at: string;
  currency: string;
  total_minor: number;
  commercial_state: CommercialState;
  fulfillment_state: FulfillmentState;
  payment_state: PaymentState;
  test_mode: boolean;
  work_state: WorkState;
  refunded_minor: number;
  refund_pending_minor: number;
  pickup_source: PickupSource | null;
  payment_mode: PaymentMode;
  collection_state: CollectionState | null;
};
export type OrderList = { items: OrderSummary[]; next_cursor: string };
export type LineAmount = {
  subtotal_minor: number;
  discount_minor: number;
  tax_minor: number;
  total_minor: number;
};
export type OrderItem = {
  sku_id: string;
  code: string;
  name: string;
  quantity: number;
  unit_price_minor: number;
  amount: LineAmount;
};
export type OrderTotals = {
  subtotal_minor: number;
  discount_minor: number;
  shipping_minor: number;
  shipping_tax_minor: number;
  tax_minor: number;
  total_minor: number;
};
export type HomeAddress = {
  region: string;
  city: string;
  postal_code: string;
  line1: string;
  line2: string;
};
export type Pickup = {
  kind: CvsKind;
  namespace: string;
  code: string;
  name: string;
  address: string;
  verification_kind: (typeof verificationKinds)[number];
};
export type Destination = {
  kind: "home" | CvsKind;
  country: string;
  recipient_name: string;
  phone: string;
  home_address: HomeAddress;
  pickup: Pickup | null;
};
export const carrierCodes = [
  "seven_eleven_cvs", "familymart_cvs", "hilife_cvs", "okmart_cvs", "sf_express", "chunghwa_post", "other",
] as const;
export type CarrierCode = (typeof carrierCodes)[number];
export const voidReasons = ["wrong_order", "wrong_tracking", "not_dispatched", "other"] as const;
export type VoidReason = (typeof voidReasons)[number];
export type Shipment = {
  version: number;
  status: "SHIPPED";
  carrier_code: CarrierCode;
  carrier_name: string | null;
  tracking_number: string;
  tracking_url: string | null;
  recorded_at: string;
};
export type ShipmentVersion = Omit<Shipment, "status"> & {
  status: "SHIPPED" | "VOIDED";
  note: string | null;
  void_reason: VoidReason | null;
  principal_id: string;
};
export type OrderDetail = OrderSummary & {
  country: string;
  service_code: string;
  items: OrderItem[];
  totals: OrderTotals;
  destination: Destination;
  shipment: Shipment | null;
};

export const canonicalUUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
export const canonicalCursor = /^[A-Za-z0-9_-]{1,1024}$/;
const timestamp = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{6}Z$/;
const maxMoney = 1_000_000_000_000;
const summaryKeys = [
  "order_id", "created_at", "updated_at", "currency", "total_minor",
  "commercial_state", "fulfillment_state", "payment_state", "test_mode", "work_state",
  "refunded_minor", "refund_pending_minor", "pickup_source", "payment_mode", "collection_state",
];

function object(value: unknown, keys: string[]): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("unavailable");
  const result = value as Record<string, unknown>;
  if (Object.keys(result).sort().join(",") !== [...keys].sort().join(",")) throw new Error("unavailable");
  return result;
}
function oneOf<T extends string>(value: unknown, values: readonly T[]): value is T {
  return typeof value === "string" && values.includes(value as T);
}
function pattern(value: unknown, regex: RegExp): value is string {
  return typeof value === "string" && regex.test(value);
}
function money(value: unknown): value is number {
  return Number.isSafeInteger(value) && (value as number) >= 0 && (value as number) <= maxMoney;
}
function text(value: unknown, max: number, required: boolean): value is string {
  return typeof value === "string" && (!required || value.length > 0) &&
    Array.from(value).length <= max && !/[\p{C}\p{Zl}\p{Zp}]/u.test(value) &&
    !/[\ud800-\udbff](?![\udc00-\udfff])|(?<![\ud800-\udbff])[\udc00-\udfff]/.test(value);
}
function date(value: unknown): value is string {
  if (typeof value !== "string" || !timestamp.test(value)) return false;
  const parsed = new Date(value);
  return Number.isFinite(parsed.getTime()) && parsed.toISOString().slice(0, 19) === value.slice(0, 19);
}
function add(a: number, b: number) {
  if (!money(a) || !money(b) || a > maxMoney - b) throw new Error("unavailable");
  return a + b;
}
function phone(value: unknown): value is string {
  if (typeof value !== "string" || value.length < 6 || value.length > 32 || value.trim() !== value ||
    !/^[0-9+() -]+$/.test(value)) return false;
  const digits = value.replace(/\D/g, "").length;
  return digits >= 6 && digits <= 20;
}

export function parseOrderSummary(value: unknown): OrderSummary {
  const v = object(value, summaryKeys);
  if (!pattern(v.order_id, canonicalUUID) || !date(v.created_at) || !date(v.updated_at) ||
    v.updated_at < v.created_at || !pattern(v.currency, /^[A-Z]{3}$/) || !money(v.total_minor) ||
    !oneOf(v.commercial_state, commercialStates) ||
    !oneOf(v.fulfillment_state, fulfillmentStates) ||
    !oneOf(v.payment_state, paymentStates) ||
    !oneOf(v.work_state, ["NONE", "READY", "REVIEW_REQUIRED"]) || typeof v.test_mode !== "boolean" ||
    !money(v.refunded_minor) || !money(v.refund_pending_minor) ||
    !(v.pickup_source === null || oneOf(v.pickup_source, pickupSources)) ||
    !oneOf(v.payment_mode, paymentModes) ||
    !(v.collection_state === null || oneOf(v.collection_state, collectionStates)))
    throw new Error("unavailable");
  const row = v as OrderSummary;
  // §16.2 schema CHECK: pay_at_pickup <=> collection_state set. Such an order never has a payment attempt, refund
  // or payment work item, so every payment-derived invariant is replaced by these (and only for this mode).
  const pap = row.payment_mode === "pay_at_pickup";
  if (pap !== (row.collection_state !== null)) throw new Error("unavailable");
  if (pap) {
    if (row.payment_state !== "NOT_STARTED" || row.test_mode || row.work_state === "REVIEW_REQUIRED" ||
      row.refunded_minor !== 0 || row.refund_pending_minor !== 0 || row.pickup_source === null ||
      (row.fulfillment_state === "CANCELLED") !== (row.commercial_state === "CANCELLED") ||
      (row.commercial_state === "CANCELLED" && row.collection_state !== "CANCELLED") ||
      (row.collection_state === "CANCELLED" && row.commercial_state !== "CANCELLED") ||
      (row.commercial_state !== "CANCELLED" && row.commercial_state !== "CONFIRMED") ||
      row.fulfillment_state === "PAID_ALLOCATION_FAILED" ||
      (["MERCHANT_SHIPPED", "PROVIDER_LABEL_CREATED"].includes(row.fulfillment_state) && row.commercial_state !== "CONFIRMED") ||
      (row.collection_state !== "PENDING" && row.collection_state !== "CANCELLED" &&
        !["MERCHANT_SHIPPED", "PROVIDER_LABEL_CREATED"].includes(row.fulfillment_state)))
      throw new Error("unavailable");
    return row;
  }
  if (row.pickup_source === "buyer_entered" && row.fulfillment_state === "PROVIDER_LABEL_CREATED") throw new Error("unavailable");
  if ((row.payment_state === "NOT_STARTED" && (row.test_mode || row.work_state !== "NONE")) ||
    (row.fulfillment_state === "CANCELLED" && row.commercial_state !== "CANCELLED") ||
    (row.fulfillment_state === "PAID_ALLOCATION_FAILED" &&
      (row.payment_state !== "REVIEW_REQUIRED" || row.work_state !== "REVIEW_REQUIRED")) ||
    (row.work_state === "READY" &&
      (!capturedStates.includes(row.payment_state) || row.commercial_state !== "CONFIRMED" ||
        (row.fulfillment_state !== "MANUAL_UNASSIGNED" && row.fulfillment_state !== "MERCHANT_SHIPPED" &&
          row.fulfillment_state !== "PROVIDER_LABEL_CREATED"))) ||
    (row.work_state === "REVIEW_REQUIRED" && row.payment_state !== "REVIEW_REQUIRED") ||
    ((row.fulfillment_state === "MERCHANT_SHIPPED" || row.fulfillment_state === "PROVIDER_LABEL_CREATED") &&
      (row.work_state !== "READY" || row.commercial_state !== "CONFIRMED")) ||
    (row.commercial_state === "CONFIRMED" &&
      (row.work_state === "NONE" || !capturedStates.includes(row.payment_state))))
    throw new Error("unavailable");
  return row;
}

export function parseOrderList(value: unknown): OrderList {
  const v = object(value, ["items", "next_cursor"]);
  if (!Array.isArray(v.items) || v.items.length > 10 || typeof v.next_cursor !== "string" ||
    (v.next_cursor !== "" && !canonicalCursor.test(v.next_cursor))) throw new Error("unavailable");
  const items = v.items.map(parseOrderSummary);
  if (new Set(items.map((item) => item.order_id)).size !== items.length) throw new Error("unavailable");
  return { items, next_cursor: v.next_cursor };
}

export function parseOrderDetail(value: unknown, requestedID: string): OrderDetail {
  const v = object(value, [...summaryKeys, "country", "service_code", "items", "totals", "destination", "shipment"]);
  const summary = parseOrderSummary(Object.fromEntries(summaryKeys.map((key) => [key, v[key]])));
  if (summary.order_id !== requestedID || !pattern(v.country, /^[A-Z]{2}$/) ||
    !pattern(v.service_code, /^[a-z][a-z0-9_-]{0,39}$/) ||
    !Array.isArray(v.items) || v.items.length < 1 || v.items.length > 50) throw new Error("unavailable");
  const t = object(v.totals, ["subtotal_minor", "discount_minor", "shipping_minor", "shipping_tax_minor", "tax_minor", "total_minor"]);
  if (![t.subtotal_minor, t.discount_minor, t.shipping_minor, t.shipping_tax_minor, t.tax_minor, t.total_minor].every(money) ||
    t.total_minor !== summary.total_minor) throw new Error("unavailable");
  const d = object(v.destination, ["kind", "country", "recipient_name", "phone", "home_address", "pickup"]);
  const h = object(d.home_address, ["region", "city", "postal_code", "line1", "line2"]);
  if (!oneOf(d.kind, ["home", ...cvsKinds]) || d.country !== v.country ||
    !text(d.recipient_name, 120, true) || !phone(d.phone) ||
    !text(h.region, 100, false) || !text(h.city, 100, false) || !text(h.postal_code, 20, false) ||
    !text(h.line1, 200, false) || !text(h.line2, 200, false)) throw new Error("unavailable");
  let pickup: Pickup | null = null;
  if (d.kind === "home") {
    if (d.pickup !== null || h.city === "" || h.line1 === "" || summary.pickup_source !== null) throw new Error("unavailable");
  } else {
    const p = object(d.pickup, ["kind", "namespace", "code", "name", "address", "verification_kind"]);
    if (d.country !== "TW" || p.kind !== d.kind || Object.values(h).some((field) => field !== "") ||
      !pattern(p.namespace, /^[a-z][a-z0-9_.:-]{0,63}$/) ||
      !pattern(p.code, /^[A-Za-z0-9_-]{1,32}$/) || !text(p.name, 120, true) ||
      !text(p.address, 400, true) || !oneOf(p.verification_kind, verificationKinds) ||
      // pickup_source is derived from the same verification kind; a disagreement is server drift.
      summary.pickup_source !== sourceOf[p.verification_kind]) throw new Error("unavailable");
    pickup = p as Pickup;
  }
  let subtotal = 0, discount = 0, tax = 0;
  const items = v.items.map((raw): OrderItem => {
    const item = object(raw, ["sku_id", "code", "name", "quantity", "unit_price_minor", "amount"]);
    const amount = object(item.amount, ["subtotal_minor", "discount_minor", "tax_minor", "total_minor"]);
    if (!pattern(item.sku_id, canonicalUUID) || !pattern(item.code, /^[A-Za-z0-9_.-]{1,64}$/) ||
      !text(item.name, 120, true) || !Number.isSafeInteger(item.quantity) ||
      (item.quantity as number) < 1 || (item.quantity as number) > 1_000_000_000 ||
      !money(item.unit_price_minor) || !Object.values(amount).every(money) ||
      (item.unit_price_minor as number) > maxMoney / (item.quantity as number) ||
      amount.subtotal_minor !== (item.unit_price_minor as number) * (item.quantity as number) ||
      (amount.discount_minor as number) > (amount.subtotal_minor as number)) throw new Error("unavailable");
    subtotal = add(subtotal, amount.subtotal_minor as number);
    discount = add(discount, amount.discount_minor as number);
    tax = add(tax, amount.tax_minor as number);
    return { ...item, amount } as OrderItem;
  });
  tax = add(tax, t.shipping_tax_minor as number);
  if (subtotal !== t.subtotal_minor || discount !== t.discount_minor || tax !== t.tax_minor) throw new Error("unavailable");
  const base = add(subtotal - discount, t.shipping_minor as number);
  const inclusive = items.every((item) => item.amount.total_minor === item.amount.subtotal_minor - item.amount.discount_minor);
  const exclusive = items.every((item) => item.amount.total_minor === item.amount.subtotal_minor - item.amount.discount_minor + item.amount.tax_minor);
  if (!((inclusive && summary.total_minor === base) ||
    (exclusive && tax <= maxMoney - base && summary.total_minor === base + tax))) throw new Error("unavailable");
  // manual-fulfilment-v1 §2: a SHIPPED head <=> MERCHANT_SHIPPED; a voided head reads as null.
  const shipment = v.shipment === null ? null : parseShipmentVersion(v.shipment, false) as Shipment;
  if ((shipment !== null) !== (summary.fulfillment_state === "MERCHANT_SHIPPED")) throw new Error("unavailable");
  return { ...summary, country: v.country as string, service_code: v.service_code as string,
    items, totals: t as OrderTotals, destination: { ...d, home_address: h, pickup } as Destination, shipment };
}

const shipmentKeys = ["version", "status", "carrier_code", "carrier_name", "tracking_number", "tracking_url", "recorded_at"];
// §3.2: 1..64 chars, no trailing space, stored exactly (leading zeroes kept).
export const trackingNumber = /^[A-Za-z0-9](?:[A-Za-z0-9 -]{0,62}[A-Za-z0-9-])?$/;
// Mirrors the Go URL rules of manual-fulfilment-v1 §3.2 that can be checked without re-serializing.
export function trackingURLHost(value: string): string | null {
  // URL.port hides an explicit default port (":443"), so the authority text is checked too.
  if (value.length > 512 || /[\u0000-\u0020\u007f-\u009f#]/.test(value) || !value.startsWith("https://") ||
    /^https:\/\/[^/?#]*[:@]/.test(value)) return null;
  try {
    const url = new URL(value);
    if (url.protocol !== "https:" || url.username || url.password || url.port || url.hash ||
      !url.hostname.includes(".")) return null;
    return url.hostname;
  } catch {
    return null;
  }
}
// detail.shipment carries the Shipment keys (head SHIPPED only); history rows add the merchant-only keys.
export function parseShipmentVersion(value: unknown, merchantFields: boolean): ShipmentVersion | Shipment {
  const v = object(value, merchantFields ? [...shipmentKeys, "note", "void_reason", "principal_id"] : shipmentKeys);
  const status = v.status;
  if (!Number.isSafeInteger(v.version) || (v.version as number) < 1 ||
    !(status === "SHIPPED" || (merchantFields && status === "VOIDED")) ||
    !oneOf(v.carrier_code, carrierCodes) ||
    !(v.carrier_name === null ? v.carrier_code !== "other" : text(v.carrier_name, 80, true)) ||
    !pattern(v.tracking_number, trackingNumber) || !date(v.recorded_at) ||
    !(v.tracking_url === null || (typeof v.tracking_url === "string" && trackingURLHost(v.tracking_url) !== null)))
    throw new Error("unavailable");
  if (merchantFields) {
    if (!(v.note === null || text(v.note, 200, false)) ||
      !(v.void_reason === null || oneOf(v.void_reason, voidReasons)) ||
      (status === "VOIDED") !== (v.void_reason !== null) || !pattern(v.principal_id, canonicalUUID))
      throw new Error("unavailable");
  }
  return v as ShipmentVersion | Shipment;
}
export function parseShipmentHistory(value: unknown): ShipmentVersion[] {
  const v = object(value, ["items"]);
  if (!Array.isArray(v.items) || v.items.length > 1000) throw new Error("unavailable");
  const items = v.items.map((item) => parseShipmentVersion(item, true) as ShipmentVersion);
  if (new Set(items.map((item) => item.version)).size !== items.length) throw new Error("unavailable");
  return items;
}

// stripe-refund-v1 §7.1 GET refunds. States/reasons are the closed sets of §5 and §4.2.
export const refundStates = ["REQUESTED", "SUBMITTING", "PENDING", "SUCCEEDED", "FAILED", "CANCELED", "REJECTED", "UNKNOWN"] as const;
export type RefundState = (typeof refundStates)[number];
export const refundReasons = ["requested_by_customer", "duplicate"] as const;
export type RefundReason = (typeof refundReasons)[number];
export const failureReasons = [
  "lost_or_stolen_card", "expired_or_canceled_card", "charge_for_pending_refund_disputed", "insufficient_funds",
  "declined", "merchant_request", "unknown", "first_send_rejected", "external_refund_detected", "send_window_closed",
] as const;
export type FailureReason = (typeof failureReasons)[number];
export type RefundItem = {
  refund_id: string;
  amount_minor: number;
  reason: RefundReason;
  state: RefundState;
  requested_at: string;
  updated_at: string;
  failure_reason?: FailureReason;
  stripe_refund_id: string | null;
};
export type RefundList = {
  captured_minor: number;
  refunded_minor: number;
  pending_minor: number;
  refundable_minor: number;
  currency: string;
  items: RefundItem[];
};
const refundItemKeys = ["refund_id", "amount_minor", "reason", "state", "requested_at", "updated_at", "stripe_refund_id"];
export function parseRefundList(value: unknown, currency: string): RefundList {
  const v = object(value, ["captured_minor", "refunded_minor", "pending_minor", "refundable_minor", "currency", "items"]);
  if (![v.captured_minor, v.refunded_minor, v.pending_minor, v.refundable_minor].every(money) ||
    v.currency !== currency || !Array.isArray(v.items) || v.items.length > 20) throw new Error("unavailable");
  // §4.4: expected_refundable = captured - held, held = succeeded + in flight.
  if (add(v.refunded_minor as number, v.pending_minor as number) + (v.refundable_minor as number) !== v.captured_minor)
    throw new Error("unavailable");
  const items = v.items.map((raw): RefundItem => {
    const withFailure = raw && typeof raw === "object" && "failure_reason" in raw;
    const item = object(raw, withFailure ? [...refundItemKeys, "failure_reason"] : refundItemKeys);
    if (!pattern(item.refund_id, canonicalUUID) || !money(item.amount_minor) || (item.amount_minor as number) < 1 ||
      !oneOf(item.reason, refundReasons) || !oneOf(item.state, refundStates) ||
      !date(item.requested_at) || !date(item.updated_at) || item.updated_at < item.requested_at ||
      (withFailure && !oneOf(item.failure_reason, failureReasons)) ||
      !(item.stripe_refund_id === null || pattern(item.stripe_refund_id, /^[A-Za-z0-9_]{1,255}$/)))
      throw new Error("unavailable");
    return item as RefundItem;
  });
  if (new Set(items.map((item) => item.refund_id)).size !== items.length) throw new Error("unavailable");
  return { ...(v as Omit<RefundList, "items">), items };
}

// manual-fulfilment-v1 E2: read-only permission probe; the server stays the authority on every write.
export type OrderActions = { refund: boolean; fulfillment_write: boolean; orders_export: boolean };
export function parseOrderActions(value: unknown): OrderActions {
  const v = object(value, ["refund", "fulfillment_write", "orders_export"]);
  if (typeof v.refund !== "boolean" || typeof v.fulfillment_write !== "boolean" || typeof v.orders_export !== "boolean")
    throw new Error("unavailable");
  return v as OrderActions;
}

// UTC display used by every orders surface (the page states the time zone in its column headers).
export function displayTime(locale: string, value: string) {
  return new Intl.DateTimeFormat(locale, {
    timeZone: "UTC", year: "numeric", month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", hour12: false,
  }).format(new Date(value));
}

// Refund amount entry. The server is the money authority (stripe-refund-v1 §4.2 step rule, I05); these helpers only
// turn typed major units into minor units and give an early hint. TWD refunds are whole dollars = multiples of 100 minor.
export function minorDigits(currency: string) {
  return new Intl.NumberFormat("en", { style: "currency", currency }).resolvedOptions().maximumFractionDigits ?? 2;
}
export const wholeOnly = (currency: string) => currency === "TWD";
export function amountToMinor(text: string, currency: string): number | null {
  const digits = minorDigits(currency);
  const match = new RegExp(digits === 0 ? "^(\\d{1,12})()$" : `^(\\d{1,12})(?:\\.(\\d{1,${digits}}))?$`).exec(text.trim());
  if (!match) return null;
  const minor = Number(match[1]) * 10 ** digits + Number((match[2] ?? "").padEnd(digits, "0") || 0);
  return Number.isSafeInteger(minor) && minor <= maxMoney ? minor : null;
}
export function minorToInput(minor: number, currency: string) {
  const digits = minorDigits(currency);
  const whole = Math.floor(minor / 10 ** digits);
  const fraction = String(minor % 10 ** digits).padStart(digits, "0");
  return digits === 0 || (wholeOnly(currency) && Number(fraction) === 0) ? String(whole) : `${whole}.${fraction}`;
}
export function refundAmountOK(minor: number | null, refundable: number, currency: string): minor is number {
  return minor !== null && minor >= 1 && minor <= refundable && (!wholeOnly(currency) || minor % 100 === 0);
}
