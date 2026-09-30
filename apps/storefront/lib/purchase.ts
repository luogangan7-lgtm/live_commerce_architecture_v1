import {
  buyerRequest,
  BuyerClientError,
  readBuyerSession,
  definiteError,
} from "./buyer-client.ts";
import {
  isCollectionState,
  isCvsKind,
  isPaymentMode,
  validBuyerCvsShipment,
  type BuyerCvsShipment,
  type CollectionState,
  type CvsKind,
  type PaymentMode,
} from "./cvs-contract.ts";

// Buyer purchase transport + strict validators for the BFF /api/buyer/* routes (cart, catalog,
// checkout-options, quotes, destination, checkout, orders). Owns: request journals/CAS and the exact wire shapes.
// It never decides money or eligibility (Go does). CVS additions follow contracts/taiwan-cvs-logistics-v1.md
// §5 and §16 (options rows, CVS destinations by pickup_id, Begin payment_mode, order CVS projection).
export type Item = { sku_id: string; quantity: number };
export type Cart = {
  id: string;
  currency: string;
  version: number;
  items: Item[];
};
export type Product = {
  product_id: string;
  sku_id: string;
  name: string;
  description: string;
  sku_code: string;
  currency: string;
  price_minor: number;
};
export type Option = {
  market_id: string;
  country: string;
  currency: string;
  method: string;
  delivery_kind: string;
  service_version: number;
  allocation_version: number;
  mode: "MANUAL" | "API";
  name_hans: string;
  name_hant: string;
  name_en: string;
  sort_order: number;
  // CVS rows only (taiwan-cvs-logistics-v1 §5.1): how the store is chosen and which payment modes exist.
  pickup_selection?: "ecpay_map" | "buyer_entered";
  payment_modes?: PaymentMode[];
  store_search_url?: string;
};
// A configured chain that cannot be sold yet (ECPay chain gate, §5.1): listed but disabled ("Coming soon").
// Go sends only the identity/label fields plus available:false and a reason; versions are not needed.
export type UnavailableOption = Pick<
  Option,
  | "market_id"
  | "country"
  | "currency"
  | "method"
  | "delivery_kind"
  | "name_hans"
  | "name_hant"
  | "name_en"
  | "sort_order"
> & { available: false; reason: "coming_soon" | "temporarily_unavailable" };
export type OptionRow = Option | UnavailableOption;
export const isUnavailable = (o: OptionRow): o is UnavailableOption =>
  (o as UnavailableOption).available === false;
export const optionKey = (value: Pick<Option, "market_id" | "country" | "method">) =>
  `${value.market_id}:${value.country}:${value.method}`;
export async function assertPurchaseContext(context: string) {
  const current = await readBuyerSession();
  if (current.state !== "active" || current.context !== context)
    throw new BuyerClientError("context_changed");
}
export type Quote = {
  id: string;
  cart_id: string;
  cart_version: number;
  market_id: string;
  country: string;
  method: string;
  currency: string;
  expires_at: string;
  lines: {
    sku_id: string;
    name: string;
    code: string;
    quantity: number;
    unit_price_minor: number;
  }[];
  amount: {
    subtotal_minor: number;
    discount_minor: number;
    shipping_minor: number;
    shipping_tax_minor: number;
    tax_minor: number;
    total_minor: number;
  };
};
type CartWrite = { expected_version: number; items: Item[] };
type QuoteWrite = {
  cart_version: number;
  market_id: string;
  country: string;
  method: string;
};
type Pending = { v: 1; context: string; key: string } & (
  | { kind: "cart"; body: CartWrite }
  | { kind: "next-cart"; body: CartWrite }
  | { kind: "quote"; body: QuoteWrite }
  | { kind: "destination"; body: DestinationMarker }
  | { kind: "checkout"; body: CheckoutWrite }
);
export type HomeAddress = {
  region: string;
  city: string;
  postal_code: string;
  line1: string;
  line2: string;
};
export type DestinationKind = "home" | CvsKind;
type DestinationMarker = {
  expected_version: number;
  cart_version: number;
  kind: DestinationKind;
  country: string;
};
// CVS destinations (§16.1/§5.2) carry the pickup_id from a verified map selection or a buyer-entered store,
// TW only, and an all-empty home_address; home destinations carry no pickup_id (Go validDestinationInput).
export type DestinationWrite = DestinationMarker & {
  recipient_name: string;
  phone: string;
  home_address: HomeAddress;
  pickup_id?: string;
};
type DestinationDetails = {
  kind: DestinationKind;
  country: string;
  recipient_name: string;
  phone: string;
  home_address: HomeAddress;
  pickup?: {
    id?: string;
    verification_kind?: string;
    kind: string;
    namespace: string;
    code: string;
    name: string;
    address: string;
    country: string;
  };
};
export type Destination = DestinationDetails & {
  id: string;
  version: number;
  cart_id: string;
  cart_version: number;
  selected_at: string;
  expires_at: string;
};
export type CheckoutWrite = {
  quote_id: string;
  destination_id: string;
  cart_version: number;
  service_version: number;
  allocation_version: number;
  // Only set for CVS options (§16.2); a home checkout body is unchanged and Go reads a missing mode as card.
  payment_mode?: PaymentMode;
};
// manual-fulfilment-v1 §3.1 carrier codes; a label, never an integration binding.
export const CARRIER_CODES = [
  "seven_eleven_cvs",
  "familymart_cvs",
  "hilife_cvs",
  "okmart_cvs",
  "sf_express",
  "chunghwa_post",
  "other",
] as const;
export type CarrierCode = (typeof CARRIER_CODES)[number];
// Buyer shipment (manual-fulfilment-v1 §5.2): the seller's attestation of dispatch, never
// in-transit or delivered evidence. Mirrors internal/checkout.BuyerShipment.
export type Shipment = {
  status: "SHIPPED";
  carrier_code: CarrierCode;
  carrier_name: string | null;
  tracking_number: string;
  tracking_url: string | null;
  recorded_at: string;
};
export type Order = {
  order_id: string;
  cart_id: string;
  cart_version: number;
  commercial_state: "DRAFT" | "AWAITING_PAYMENT" | "CONFIRMED" | "CANCELLED";
  fulfillment_state:
    | "MANUAL_UNASSIGNED"
    | "CANCELLED"
    | "PAID_ALLOCATION_FAILED"
    | "MERCHANT_SHIPPED"
    | "PROVIDER_LABEL_CREATED";
  // Go always emits it (null unless the head is SHIPPED); absent is read as null.
  shipment?: Shipment | null;
  // taiwan-cvs-logistics-v1 §16.2/§5.3: Go always emits all three; absent reads as card / null / null so
  // an order from before the CVS release still validates. pay_at_pickup <=> collection_state != null.
  payment_mode?: PaymentMode;
  collection_state?: CollectionState | null;
  cvs_shipment?: BuyerCvsShipment | null;
  hold_expires_at?: string;
  snapshot: {
    quote: Pick<Quote, "currency" | "lines" | "amount">;
    destination: DestinationDetails;
    service: {
      code: string;
      name_hans: string;
      name_hant: string;
      name_en: string;
      delivery_kind: string;
      mode: string;
    };
  };
};
export type OrderSummary = Pick<
  Order,
  | "order_id"
  | "cart_id"
  | "cart_version"
  | "commercial_state"
  | "fulfillment_state"
> & { created_at: string; currency: string; total_minor: number };
const UUID = /^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/;
const CONTEXT = /^[A-Za-z0-9_-]{43}$/;
const MAX_AMOUNT = 1_000_000_000_000;
const record = (v: unknown): v is Record<string, unknown> =>
  !!v && typeof v === "object" && !Array.isArray(v);
const integer = (
  v: unknown,
  min = 0,
  max = Number.MAX_SAFE_INTEGER,
): v is number =>
  typeof v === "number" && Number.isSafeInteger(v) && v >= min && v <= max;
const id = (v: unknown): v is string => typeof v === "string" && UUID.test(v);
const currency = (v: unknown): v is string =>
  typeof v === "string" && /^[A-Z]{3}$/.test(v);
const country = (v: unknown): v is string =>
  typeof v === "string" && /^[A-Z]{2}$/.test(v);
const deliveryMethod = (v: unknown): v is string =>
  typeof v === "string" && /^delivery:[a-z0-9_-]+$/.test(v);
const timestamp = (v: unknown): v is string =>
  typeof v === "string" && Number.isFinite(Date.parse(v));
const exact = (v: Record<string, unknown>, keys: string[]) =>
  Object.keys(v).sort().join() === keys.sort().join();
export const validItems = (v: unknown): v is Item[] =>
  Array.isArray(v) &&
  v.length <= 100 &&
  v.every(
    (x) =>
      record(x) &&
      exact(x, ["sku_id", "quantity"]) &&
      id(x.sku_id) &&
      integer(x.quantity, 1, 1_000_000_000),
  ) &&
  new Set(v.map((x) => x.sku_id)).size === v.length;
export const validCart = (v: unknown): v is Cart =>
  record(v) &&
  (v.id === "" || id(v.id)) &&
  currency(v.currency) &&
  integer(v.version) &&
  validItems(v.items);
export const validProduct = (v: unknown): v is Product =>
  record(v) &&
  id(v.product_id) &&
  id(v.sku_id) &&
  [v.name, v.description, v.sku_code].every((x) => typeof x === "string") &&
  currency(v.currency) &&
  integer(v.price_minor, 0, MAX_AMOUNT);
const HOME_AND_CVS = ["home", "cvs_711", "cvs_familymart", "cvs_hilife", "cvs_okmart"];
const optionLabels = (v: Record<string, unknown>) =>
  [v.name_hans, v.name_hant, v.name_en].every((x) => typeof x === "string") &&
  integer(v.sort_order, -2147483648, 2147483647);
// CVS-only fields (§5.1). Home rows carry none of them. store_search_url is informational: the UI links its
// own constants (cvs-contract.ts CVS_SEARCH_LINKS), never a server-supplied href.
const validCvsOptionFields = (v: Record<string, unknown>) =>
  (v.pickup_selection === "ecpay_map" || v.pickup_selection === "buyer_entered") &&
  Array.isArray(v.payment_modes) &&
  v.payment_modes.length >= 1 &&
  v.payment_modes.length <= 2 &&
  v.payment_modes.every(isPaymentMode) &&
  new Set(v.payment_modes).size === v.payment_modes.length &&
  v.payment_modes.includes("card") &&
  (v.pickup_selection === "buyer_entered"
    ? typeof v.store_search_url === "string" &&
      v.store_search_url.startsWith("https://") &&
      v.store_search_url.length <= 512
    : v.store_search_url === undefined);
export const validOption = (v: unknown): v is Option =>
  record(v) &&
  (v.available === undefined || v.available === true) &&
  v.reason === undefined &&
  id(v.market_id) &&
  currency(v.currency) &&
  country(v.country) &&
  deliveryMethod(v.method) &&
  integer(v.service_version, 1) &&
  integer(v.allocation_version, 1) &&
  ["MANUAL", "API"].includes(String(v.mode)) &&
  HOME_AND_CVS.includes(String(v.delivery_kind)) &&
  optionLabels(v) &&
  (v.delivery_kind === "home"
    ? v.pickup_selection === undefined &&
      v.payment_modes === undefined &&
      v.store_search_url === undefined
    : country(v.country) && v.country === "TW" && validCvsOptionFields(v));
// {available:false, reason} rows keep only identity + labels (a chain the store configured but ECPay gates).
const validUnavailableOption = (v: unknown): v is UnavailableOption =>
  record(v) &&
  v.available === false &&
  (v.reason === "coming_soon" || v.reason === "temporarily_unavailable") &&
  id(v.market_id) &&
  currency(v.currency) &&
  country(v.country) &&
  deliveryMethod(v.method) &&
  isCvsKind(v.delivery_kind) &&
  optionLabels(v);
export const validOptionRow = (v: unknown): v is OptionRow =>
  validOption(v) || validUnavailableOption(v);
const validQuoteSummary = (v: unknown): v is Order["snapshot"]["quote"] =>
  record(v) &&
  currency(v.currency) &&
  Array.isArray(v.lines) &&
  v.lines.length > 0 &&
  v.lines.every(
    (x) =>
      record(x) &&
      id(x.sku_id) &&
      typeof x.name === "string" &&
      typeof x.code === "string" &&
      integer(x.quantity, 1, 1_000_000_000) &&
      integer(x.unit_price_minor, 0, MAX_AMOUNT),
  ) &&
  record(v.amount) &&
  [
    "subtotal_minor",
    "discount_minor",
    "shipping_minor",
    "shipping_tax_minor",
    "tax_minor",
    "total_minor",
  ].every((k) =>
    integer(
      v.amount && (v.amount as Record<string, unknown>)[k],
      0,
      MAX_AMOUNT,
    ),
  );
export const validQuote = (v: unknown): v is Quote =>
  record(v) &&
  id(v.id) &&
  id(v.cart_id) &&
  id(v.market_id) &&
  country(v.country) &&
  deliveryMethod(v.method) &&
  integer(v.cart_version, 1) &&
  timestamp(v.expires_at) &&
  validQuoteSummary(v);

// The private API may return a previous CVS head even though the first inline
// address form writes only home destinations. Validate it, do not auto-confirm.
const validDestinationDetails = (v: unknown): v is DestinationDetails =>
  record(v) &&
  HOME_AND_CVS.includes(String(v.kind)) &&
  country(v.country) &&
  typeof v.recipient_name === "string" &&
  typeof v.phone === "string" &&
  record(v.home_address) &&
  ["region", "city", "postal_code", "line1", "line2"].every(
    (k) => typeof (v.home_address as Record<string, unknown>)[k] === "string",
  ) &&
  (v.kind === "home"
    ? v.pickup === undefined
    : record(v.pickup) &&
      ["kind", "namespace", "code", "name", "address", "country"].every(
        (k) => typeof (v.pickup as Record<string, unknown>)[k] === "string",
      ));
export const validDestination = (v: unknown): v is Destination =>
  record(v) &&
  id(v.id) &&
  integer(v.version, 1) &&
  id(v.cart_id) &&
  integer(v.cart_version, 1) &&
  timestamp(v.selected_at) &&
  timestamp(v.expires_at) &&
  validDestinationDetails(v);
const FULFILLMENT_STATES = [
  "MANUAL_UNASSIGNED",
  "CANCELLED",
  "PAID_ALLOCATION_FAILED",
  "MERCHANT_SHIPPED",
  // taiwan-cvs-logistics-v1 §6: an ECPay label exists (order stays confirmed, never "delivered").
  "PROVIDER_LABEL_CREATED",
];
// The link is rendered as an href, so it is checked at this trust boundary exactly like Go's
// canonical rule (§3.2): https, dotted host, no userinfo/port/fragment/whitespace, <=512 bytes.
export function validTrackingURL(v: unknown): v is string {
  if (
    typeof v !== "string" ||
    !v.startsWith("https://") ||
    new TextEncoder().encode(v).length > 512 ||
    /[\s\u0000-\u001f\u007f-\u009f#]/u.test(v)
  )
    return false;
  try {
    const u = new URL(v);
    return (
      u.protocol === "https:" &&
      u.username === "" &&
      u.password === "" &&
      u.port === "" &&
      u.hash === "" &&
      u.hostname.includes(".")
    );
  } catch {
    return false;
  }
}
// MERCHANT_SHIPPED <=> a SHIPPED head (Go rejects any disagreement as drift).
const validShipment = (v: unknown, state: unknown): boolean => {
  if (v === undefined || v === null) return state !== "MERCHANT_SHIPPED";
  if (state !== "MERCHANT_SHIPPED") return false;
  if (
    !record(v) ||
    !exact(v, [
      "status",
      "carrier_code",
      "carrier_name",
      "tracking_number",
      "tracking_url",
      "recorded_at",
    ]) ||
    v.status !== "SHIPPED" ||
    !(CARRIER_CODES as readonly string[]).includes(String(v.carrier_code)) ||
    !timestamp(v.recorded_at)
  )
    return false;
  const name = v.carrier_name;
  if (
    name === null
      ? v.carrier_code === "other"
      : typeof name !== "string" ||
        [...name].length < 1 ||
        [...name].length > 80 ||
        /\p{Cc}/u.test(name)
  )
    return false;
  return (
    typeof v.tracking_number === "string" &&
    /^[A-Za-z0-9][A-Za-z0-9 -]{0,63}$/.test(v.tracking_number) &&
    !v.tracking_number.endsWith(" ") &&
    (v.tracking_url === null || validTrackingURL(v.tracking_url))
  );
};
// §16.2: payment_mode pay_at_pickup <=> collection_state set (schema CHECK). §5.3: cvs_shipment only for a
// CVS destination. Absent keys read as card / null / null (an order written before the CVS release).
function validOrderCvs(v: Record<string, unknown>): boolean {
  const mode = v.payment_mode ?? "card";
  const collection = v.collection_state ?? null;
  const shipment = v.cvs_shipment ?? null;
  if (!isPaymentMode(mode)) return false;
  if (!(collection === null || isCollectionState(collection))) return false;
  if ((mode === "pay_at_pickup") !== (collection !== null)) return false;
  if (shipment !== null && !validBuyerCvsShipment(shipment)) return false;
  const kind =
    record(v.snapshot) && record(v.snapshot.destination)
      ? v.snapshot.destination.kind
      : undefined;
  return shipment === null || (isCvsKind(kind) && shipment.chain === kind);
}
export const validOrder = (v: unknown): v is Order =>
  record(v) &&
  id(v.order_id) &&
  id(v.cart_id) &&
  integer(v.cart_version, 1) &&
  ["DRAFT", "AWAITING_PAYMENT", "CONFIRMED", "CANCELLED"].includes(
    String(v.commercial_state),
  ) &&
  FULFILLMENT_STATES.includes(String(v.fulfillment_state)) &&
  validShipment(v.shipment, v.fulfillment_state) &&
  validOrderCvs(v) &&
  (v.commercial_state === "DRAFT"
    ? timestamp(v.hold_expires_at)
    : v.hold_expires_at === undefined) &&
  record(v.snapshot) &&
  validQuoteSummary(v.snapshot.quote) &&
  validDestinationDetails(v.snapshot.destination) &&
  record(v.snapshot.service) &&
  ["code", "name_hans", "name_hant", "name_en", "delivery_kind", "mode"].every(
    (k) =>
      typeof (v.snapshot as Order["snapshot"]).service[
        k as keyof Order["snapshot"]["service"]
      ] === "string",
  );
export const validOrderSummary = (v: unknown): v is OrderSummary =>
  record(v) &&
  exact(v, [
    "order_id",
    "cart_id",
    "cart_version",
    "commercial_state",
    "fulfillment_state",
    "created_at",
    "currency",
    "total_minor",
  ]) &&
  id(v.order_id) &&
  id(v.cart_id) &&
  integer(v.cart_version, 1) &&
  ["DRAFT", "AWAITING_PAYMENT", "CONFIRMED", "CANCELLED"].includes(
    String(v.commercial_state),
  ) &&
  FULFILLMENT_STATES.includes(String(v.fulfillment_state)) &&
  timestamp(v.created_at) &&
  typeof v.currency === "string" &&
  /^[A-Z]{3}$/.test(v.currency) &&
  integer(v.total_minor, 0, MAX_AMOUNT);
const validDestinationMarker = (v: unknown): v is DestinationMarker =>
  record(v) &&
  exact(v, ["expected_version", "cart_version", "kind", "country"]) &&
  integer(v.expected_version, 0, Number.MAX_SAFE_INTEGER - 1) &&
  integer(v.cart_version, 1) &&
  (v.kind === "home" || isCvsKind(v.kind)) &&
  country(v.country);
const destinationMarker = (v: DestinationWrite): DestinationMarker => ({
  expected_version: v.expected_version,
  cart_version: v.cart_version,
  kind: v.kind,
  country: v.country,
});
// Match Go's printable Unicode policy; lengths count Unicode code points,
// not UTF-16 units. This is input feedback, never a substitute for server rules.
const destinationText = (
  v: unknown,
  max: number,
  required = false,
): v is string =>
  typeof v === "string" &&
  v === v.trim() &&
  [...v].length <= max &&
  (!required || v.length > 0) &&
  /^[\p{L}\p{M}\p{N}\p{P}\p{S}\x20]*$/u.test(v);
const HOME_KEYS = [
  "expected_version",
  "cart_version",
  "kind",
  "country",
  "recipient_name",
  "phone",
  "home_address",
];
export const validDestinationWrite = (v: unknown): v is DestinationWrite =>
  record(v) &&
  exact(v, v.kind === "home" ? HOME_KEYS : [...HOME_KEYS, "pickup_id"]) &&
  validDestinationMarker(destinationMarker(v as DestinationWrite)) &&
  destinationText(v.recipient_name, 120, true) &&
  typeof v.phone === "string" &&
  v.phone === v.phone.trim() &&
  /^[0-9+() -]{6,32}$/.test(v.phone) &&
  /^[0-9]{6,20}$/.test(v.phone.replace(/\D/g, "")) &&
  record(v.home_address) &&
  exact(v.home_address, ["region", "city", "postal_code", "line1", "line2"]) &&
  (v.kind === "home"
    ? destinationText(v.home_address.region, 100) &&
      destinationText(v.home_address.city, 100, true) &&
      destinationText(v.home_address.postal_code, 20) &&
      destinationText(v.home_address.line1, 200, true) &&
      destinationText(v.home_address.line2, 200)
    : // CVS: TW only, a pickup_id from a verified selection / buyer-entered store, no home address (Go rule).
      v.country === "TW" &&
      id(v.pickup_id) &&
      Object.values(v.home_address).every((x) => x === ""));
const validCheckoutWrite = (v: unknown): v is CheckoutWrite =>
  record(v) &&
  exact(v, [
    "quote_id",
    "destination_id",
    "cart_version",
    "service_version",
    "allocation_version",
    ...(v.payment_mode === undefined ? [] : ["payment_mode"]),
  ]) &&
  (v.payment_mode === undefined || isPaymentMode(v.payment_mode)) &&
  id(v.quote_id) &&
  id(v.destination_id) &&
  integer(v.cart_version, 1) &&
  integer(v.service_version, 1) &&
  integer(v.allocation_version, 1);

export function checkoutInput(
  quote: Quote,
  option: Option,
  cart: Cart,
  destination: Destination,
  now = Date.now(),
  paymentMode?: PaymentMode,
): CheckoutWrite {
  const cvs = option.delivery_kind !== "home";
  if (
    !validQuote(quote) ||
    !validOption(option) ||
    !validCart(cart) ||
    !validDestination(destination) ||
    quote.cart_id !== cart.id ||
    quote.cart_version !== cart.version ||
    quote.market_id !== option.market_id ||
    quote.country !== option.country ||
    quote.method !== option.method ||
    quote.currency !== option.currency ||
    quote.currency !== cart.currency ||
    // Home stays MANUAL-only; a CVS row may be MANUAL (buyer_entered) or API (ecpay_map), Go re-checks.
    (!cvs && option.mode !== "MANUAL") ||
    destination.kind !== option.delivery_kind ||
    (cvs && destination.pickup?.kind !== option.delivery_kind) ||
    // payment_mode belongs to CVS rows only and must be one the row offers (§16.2, §5.1).
    (cvs
      ? !(option.payment_modes ?? []).includes(paymentMode ?? "card")
      : paymentMode !== undefined) ||
    destination.cart_id !== cart.id ||
    destination.cart_version !== cart.version ||
    destination.country !== quote.country ||
    !Number.isFinite(now) ||
    Date.parse(quote.expires_at) <= now ||
    Date.parse(destination.expires_at) <= now
  )
    throw new BuyerClientError("request_failed");
  return {
    quote_id: quote.id,
    destination_id: destination.id,
    cart_version: cart.version,
    service_version: option.service_version,
    allocation_version: option.allocation_version,
    ...(cvs ? { payment_mode: paymentMode ?? "card" } : {}),
  };
}

export function lineSubtotal(price: number, quantity: number): number | null {
  if (
    !integer(price, 0, MAX_AMOUNT) ||
    !integer(quantity, 1, 1_000_000_000) ||
    (price > 0 && quantity > Math.floor(MAX_AMOUNT / price))
  )
    return null;
  return price * quantity;
}

// SetCart replaces the complete cart. Preserve all unrelated SKUs, use the
// version actually shown to the buyer, and let the server reject a stale CAS.
export function cartSelection(
  cart: Cart,
  sku: string,
  quantity: number,
): CartWrite {
  if (!validCart(cart) || !id(sku) || !integer(quantity, 1, 1_000_000_000))
    throw new BuyerClientError("invalid_response");
  const items = [
    ...cart.items.filter((x) => x.sku_id !== sku),
    { sku_id: sku, quantity },
  ].sort((a, b) => a.sku_id.localeCompare(b.sku_id));
  if (!validItems(items)) throw new BuyerClientError("request_failed");
  return { expected_version: cart.version, items };
}

export async function readPurchase<T>(
  suffix: string,
  context: string,
  valid: (value: unknown) => value is T,
): Promise<T> {
  const response = await buyerRequest("GET", suffix, context);
  if (!response.ok)
    throw new BuyerClientError("request_failed", response.status);
  const value: unknown = await response.json();
  if (!valid(value)) throw new BuyerClientError("invalid_response");
  await assertPurchaseContext(context);
  return value;
}

export async function purchasePage<T>(
  suffix: string,
  context: string,
  valid: (value: unknown) => value is T,
): Promise<{ items: T[]; next_cursor: string }> {
  return readPurchase(
    suffix,
    context,
    (v): v is { items: T[]; next_cursor: string } =>
      record(v) &&
      Array.isArray(v.items) &&
      v.items.length <= 100 &&
      v.items.every(valid) &&
      typeof v.next_cursor === "string" &&
      v.next_cursor.length <= 2048,
  );
}

function storageKey(context: string) {
  if (!CONTEXT.test(context)) throw new BuyerClientError("context_changed");
  return `commerce-purchase-pending-v1:${context}`;
}

export function parsePending(raw: string, context: string): Pending {
  try {
    if (raw.length > 24000) throw new Error();
    const v: unknown = JSON.parse(raw);
    if (
      !record(v) ||
      !exact(v, ["v", "context", "key", "kind", "body"]) ||
      v.v !== 1 ||
      v.context !== context ||
      !id(v.key) ||
      !record(v.body)
    )
      throw new Error();
    const b = v.body;
    if (
      (v.kind === "cart" || v.kind === "next-cart") &&
      exact(b, ["expected_version", "items"]) &&
      integer(b.expected_version) &&
      validItems(b.items) &&
      (v.kind !== "next-cart" || (b.items as Item[]).length === 0)
    )
      return v as Pending;
    if (
      v.kind === "quote" &&
      exact(b, ["cart_version", "market_id", "country", "method"]) &&
      integer(b.cart_version, 1) &&
      id(b.market_id) &&
      typeof b.country === "string" &&
      /^[A-Z]{2}$/.test(b.country) &&
      typeof b.method === "string" &&
      /^delivery:[a-z0-9_-]+$/.test(b.method)
    )
      return v as Pending;
    if (v.kind === "destination" && validDestinationMarker(b))
      return v as Pending;
    if (v.kind === "checkout" && validCheckoutWrite(b)) return v as Pending;
    throw new Error();
  } catch {
    throw new BuyerClientError("uncertain");
  }
}

export function pendingPurchase(context: string): Pending | null {
  try {
    const raw = localStorage.getItem(storageKey(context));
    return raw === null ? null : parsePending(raw, context);
  } catch (error) {
    if (error instanceof BuyerClientError) throw error;
    throw new BuyerClientError("unavailable");
  }
}

function persistPending(pending: Pending) {
  const raw = JSON.stringify(pending);
  parsePending(raw, pending.context); // Reject an accidental PII field before storage.
  try {
    localStorage.setItem(storageKey(pending.context), raw);
    if (localStorage.getItem(storageKey(pending.context)) !== raw)
      throw new Error();
  } catch {
    throw new BuyerClientError("unavailable");
  }
}

function clearPending(pending: Pending) {
  if (pendingPurchase(pending.context)?.key !== pending.key)
    throw new BuyerClientError("uncertain");
  try {
    localStorage.removeItem(storageKey(pending.context));
    if (localStorage.getItem(storageKey(pending.context)) !== null)
      throw new Error();
  } catch {
    throw new BuyerClientError("unavailable");
  }
}

function orderKey(context: string) {
  storageKey(context); // Same strict context validation, no foreign-context lookup.
  return `commerce-purchase-order-v1:${context}`;
}

export function knownOrderID(context: string): string | null {
  let raw: string | null;
  try {
    raw = localStorage.getItem(orderKey(context));
  } catch {
    throw new BuyerClientError("unavailable");
  }
  if (raw === null) return null;
  try {
    if (raw.length > 250) throw new Error();
    const v: unknown = JSON.parse(raw);
    if (
      !record(v) ||
      !exact(v, ["v", "context", "order_id"]) ||
      v.v !== 1 ||
      v.context !== context ||
      !id(v.order_id)
    )
      throw new Error();
    return v.order_id;
  } catch {
    throw new BuyerClientError("uncertain");
  }
}

// Call before offering a session reset. Losing access to an order is not proof
// it failed. The UI must retain its old-context recovery record, not place anew.
export function orderRecoveryRequired(context: string): boolean {
  return (
    ["checkout", "next-cart"].includes(pendingPurchase(context)?.kind ?? "") ||
    knownOrderID(context) !== null
  );
}

function assertNoOrder(context: string) {
  if (orderRecoveryRequired(context)) throw new BuyerClientError("uncertain");
}

// Destination PII is deliberately absent from this persisted journal. Its
// metadata marker and checkout IDs share the SAME lock with cart/quote writes.
export async function writePurchase(
  context: string,
  input?:
    { kind: "cart"; body: CartWrite } | { kind: "quote"; body: QuoteWrite },
): Promise<{ kind: "cart"; value: Cart } | { kind: "quote"; value: Quote }> {
  if (!navigator.locks) throw new BuyerClientError("unavailable");
  return navigator.locks.request("commerce-purchase-write-v1", async () => {
    assertNoOrder(context);
    let pending = pendingPurchase(context);
    if (pending && pending.kind !== "cart" && pending.kind !== "quote")
      throw new BuyerClientError("uncertain");
    if (
      pending &&
      input &&
      (pending.kind !== input.kind ||
        JSON.stringify(pending.body) !== JSON.stringify(input.body))
    )
      throw new BuyerClientError("uncertain");
    if (!pending) {
      if (!input) throw new BuyerClientError("request_failed");
      const raw = JSON.stringify({
        ...input,
        v: 1,
        context,
        key: crypto.randomUUID(),
      });
      pending = parsePending(raw, context);
      persistPending(pending);
    }
    if (pending.kind !== "cart" && pending.kind !== "quote")
      throw new BuyerClientError("uncertain");
    const response = await buyerRequest(
      pending.kind === "cart" ? "PUT" : "POST",
      pending.kind === "cart" ? "cart" : "quotes",
      context,
      pending.body,
      pending.key,
    );
    const conclusive =
      !response.ok &&
      response.status < 500 &&
      (await definiteError(response.clone()));
    let value: unknown;
    try {
      value = await response.json();
    } catch {
      throw new BuyerClientError("uncertain");
    }
    if (!response.ok) {
      // A parsed non-retryable API denial is conclusive; HTML/5xx/network loss
      // keeps the journal and blocks a new mutation until explicit recovery.
      if (
        conclusive &&
        record(value) &&
        value.retryable === false &&
        typeof value.code === "string"
      ) {
        clearPending(pending);
        throw new BuyerClientError("request_failed", response.status);
      }
      throw new BuyerClientError("uncertain", response.status);
    }
    if (
      (pending.kind === "cart" && !validCart(value)) ||
      (pending.kind === "quote" && !validQuote(value))
    )
      throw new BuyerClientError("uncertain");
    await assertPurchaseContext(context);
    // A replay receipt is historical. Read the current cart, not its old version,
    // before allowing a follow-up quote or another CAS mutation.
    if (pending.kind === "cart")
      value = await readPurchase("cart", context, validCart);
    // Persist only a result ID before clearing the request. Reload can then GET
    // the quote, including its expired state, without silently creating another.
    if (pending.kind === "quote")
      sessionStorage.setItem(
        `commerce-purchase-quote-v1:${context}`,
        (value as Quote).id,
      );
    clearPending(pending);
    return pending.kind === "cart"
      ? { kind: "cart", value: value as Cart }
      : { kind: "quote", value: value as Quote };
  });
}

export async function currentDestination(
  context: string,
): Promise<Destination | null> {
  const result = await readPurchase(
    "destination",
    context,
    (v): v is { destination: Destination | null } =>
      record(v) && (v.destination === null || validDestination(v.destination)),
  );
  return result.destination;
}

// One tab's exact address attempt may be retained ONLY in memory for replay.
// A reload/other tab cannot reconstruct it and must confirm a new CAS intent.
let addressAttempt: {
  context: string;
  key: string;
  body: DestinationWrite;
} | null = null;
export function forgetAddressAttempt() {
  addressAttempt = null;
}

async function commandResponse(
  response: Response,
  pending: Pending,
  recovering: boolean,
): Promise<unknown> {
  if (!response.ok) {
    const definitive =
      response.status < 500 && (await definiteError(response.clone()));
    const value: unknown = await response.json().catch(() => null);
    // A denial during replay cannot exclude a commit before access was revoked.
    if (
      !recovering &&
      definitive &&
      record(value) &&
      value.retryable === false
    ) {
      clearPending(pending);
      throw new BuyerClientError(
        "request_failed",
        response.status,
        typeof value.code === "string" ? value.code : undefined,
      );
    }
    throw new BuyerClientError("uncertain", response.status);
  }
  try {
    return await response.json();
  } catch {
    throw new BuyerClientError("uncertain");
  }
}

export async function writeDestination(
  context: string,
  input: DestinationWrite,
  replacePendingKey?: string,
): Promise<Destination> {
  if (!validDestinationWrite(input))
    throw new BuyerClientError("request_failed");
  // Freeze intent before waiting for the lock; a form edit must not mutate a
  // queued request behind an already-recorded idempotency key.
  const body: DestinationWrite = structuredClone(input);
  if (!navigator.locks) throw new BuyerClientError("unavailable");
  return navigator.locks.request("commerce-purchase-write-v1", async () => {
    assertNoOrder(context);
    let pending = pendingPurchase(context);
    if (pending && pending.kind !== "destination")
      throw new BuyerClientError("uncertain");
    if (replacePendingKey !== undefined) {
      if (
        !pending ||
        pending.key !== replacePendingKey ||
        body.expected_version < pending.body.expected_version
      )
        throw new BuyerClientError("uncertain");
      const current = await currentDestination(context);
      if ((current?.version ?? 0) !== body.expected_version)
        throw new BuyerClientError("request_failed", 409);
      // Caller explicitly reconfirmed the displayed current head. Do not clear
      // first: replace and read back the marker before sending this NEW intent.
      pending = null;
    }
    const recovering = pending !== null;
    if (pending) {
      if (
        addressAttempt?.context !== context ||
        addressAttempt.key !== pending.key ||
        JSON.stringify(addressAttempt.body) !== JSON.stringify(body)
      )
        throw new BuyerClientError("uncertain");
    } else {
      pending = {
        v: 1,
        context,
        key: crypto.randomUUID(),
        kind: "destination",
        body: destinationMarker(body),
      };
      persistPending(pending);
      addressAttempt = { context, key: pending.key, body };
    }
    const response = await buyerRequest(
      "PUT",
      "destination",
      context,
      body,
      pending.key,
    );
    const receipt = await commandResponse(response, pending, recovering);
    if (!validDestination(receipt)) throw new BuyerClientError("uncertain");
    const current = await currentDestination(context);
    // Receipt may be historical. Compare both its identity and the exact PII
    // in memory to the current head before handing a confirmed snapshot to UI.
    const matches =
      current?.id === receipt.id &&
      current.kind === body.kind &&
      current.country === body.country &&
      current.cart_version === body.cart_version &&
      current.version === body.expected_version + 1 &&
      current.recipient_name === body.recipient_name &&
      current.phone === body.phone &&
      // CVS: the head must point at the pickup we submitted (Go returns it as pickup.id).
      (body.pickup_id === undefined || current.pickup?.id === body.pickup_id) &&
      Object.keys(body.home_address).every(
        (k) =>
          current.home_address[k as keyof HomeAddress] ===
          body.home_address[k as keyof HomeAddress],
      );
    clearPending(pending);
    forgetAddressAttempt();
    if (!matches || !current || Date.parse(current.expires_at) <= Date.now())
      throw new BuyerClientError("request_failed", 409);
    return current;
  });
}

export async function readOrder(
  context: string,
  orderID: string,
): Promise<Order> {
  if (!id(orderID)) throw new BuyerClientError("invalid_response");
  const order = await readPurchase(`orders/${orderID}`, context, validOrder);
  if (order.order_id !== orderID)
    throw new BuyerClientError("invalid_response");
  return order;
}

// Starting a new purchase never cancels the previous order/hold. The permanent
// cart.set receipt and CAS are the existing server authority; local storage is
// only a crash-safe intent journal. History is queried from the owned SQL list.
export async function continueShopping(
  context: string,
  expectedOrderID?: string,
): Promise<Cart> {
  if (!navigator.locks) throw new BuyerClientError("unavailable");
  return navigator.locks.request("commerce-purchase-write-v1", async () => {
    await assertPurchaseContext(context);
    let pending = pendingPurchase(context);
    if (pending && pending.kind !== "next-cart")
      throw new BuyerClientError("uncertain");
    const existing = knownOrderID(context);
    if (expectedOrderID !== undefined && existing !== expectedOrderID)
      throw new BuyerClientError("uncertain");
    let current: Cart;
    if (!pending) {
      if (!existing) throw new BuyerClientError("request_failed");
      const previous = await readOrder(context, existing);
      current = await readPurchase("cart", context, validCart);
      if (
        current.id !== previous.cart_id ||
        current.version < previous.cart_version
      )
        throw new BuyerClientError("uncertain");
      // Never clear a cart already advanced by another authorized actor.
      if (current.version === previous.cart_version) {
        pending = {
          v: 1,
          context,
          key: crypto.randomUUID(),
          kind: "next-cart",
          body: { expected_version: current.version, items: [] },
        };
        persistPending(pending);
      }
    }
    if (pending) {
      const response = await buyerRequest(
        "PUT",
        "cart",
        context,
        pending.body,
        pending.key,
      );
      if (
        !response.ok &&
        !(response.status === 409 && (await definiteError(response.clone())))
      )
        throw new BuyerClientError("uncertain", response.status);
      if (response.ok) {
        let receipt: unknown;
        try {
          receipt = await response.json();
        } catch {
          throw new BuyerClientError("uncertain");
        }
        if (
          !validCart(receipt) ||
          receipt.version !== pending.body.expected_version + 1 ||
          receipt.items.length
        )
          throw new BuyerClientError("uncertain");
      }
      current = await readPurchase("cart", context, validCart);
      if (current.version <= pending.body.expected_version)
        throw new BuyerClientError("uncertain");
    }
    await assertPurchaseContext(context);
    // Clear the navigation hint only after a newer cart is authoritative. Keep
    // the journal until both removals read back, so a partial storage failure
    // cannot masquerade as a fresh checkout on reload.
    try {
      if (knownOrderID(context) !== existing) throw new Error();
      sessionStorage.removeItem(`commerce-purchase-quote-v1:${context}`);
      if (sessionStorage.getItem(`commerce-purchase-quote-v1:${context}`) !== null)
        throw new Error();
      localStorage.removeItem(orderKey(context));
      if (knownOrderID(context) !== null) throw new Error();
    } catch {
      throw new BuyerClientError("unavailable");
    }
    if (pending) clearPending(pending);
    forgetAddressAttempt();
    return current!;
  });
}

// Begin receipt {order_id, hold_expires_at} (+ payment_mode/commercial_state since taiwan-cvs §16.2). A
// pay_at_pickup order is CONFIRMED at placement, so its receipt need not carry a hold time; a card order
// must. When the request named a mode, an echoed mode must agree with it.
function validCheckoutReceipt(
  receipt: unknown,
  requested?: PaymentMode,
): receipt is { order_id: string } {
  if (!record(receipt) || !id(receipt.order_id)) return false;
  const mode = receipt.payment_mode;
  if (mode !== undefined && (!isPaymentMode(mode) || mode !== (requested ?? "card")))
    return false;
  if (
    receipt.commercial_state !== undefined &&
    !["DRAFT", "AWAITING_PAYMENT", "CONFIRMED", "CANCELLED"].includes(
      String(receipt.commercial_state),
    )
  )
    return false;
  return (
    timestamp(receipt.hold_expires_at) ||
    ((requested ?? "card") === "pay_at_pickup" &&
      receipt.hold_expires_at === undefined)
  );
}

export async function writeCheckout(
  context: string,
  input?: CheckoutWrite,
): Promise<Order> {
  if (input !== undefined && !validCheckoutWrite(input))
    throw new BuyerClientError("request_failed");
  const body = input === undefined ? undefined : structuredClone(input);
  if (!navigator.locks) throw new BuyerClientError("unavailable");
  return navigator.locks.request("commerce-purchase-write-v1", async () => {
    let pending = pendingPurchase(context);
    const existingID = knownOrderID(context);
    if (pending && pending.kind !== "checkout")
      throw new BuyerClientError("uncertain");
    if (!pending && existingID) return readOrder(context, existingID);
    const recovering = pending !== null;
    if (
      pending &&
      body &&
      JSON.stringify(pending.body) !== JSON.stringify(body)
    )
      throw new BuyerClientError("uncertain");
    if (!pending) {
      if (!body) throw new BuyerClientError("request_failed");
      pending = {
        v: 1,
        context,
        key: crypto.randomUUID(),
        kind: "checkout",
        body,
      };
      persistPending(pending);
    }
    const response = await buyerRequest(
      "POST",
      "checkout",
      context,
      pending.body,
      pending.key,
    );
    const receipt = await commandResponse(response, pending, recovering);
    if (
      !validCheckoutReceipt(receipt, pending.body.payment_mode) ||
      (existingID !== null && existingID !== receipt.order_id)
    )
      throw new BuyerClientError("uncertain");
    await assertPurchaseContext(context);
    try {
      localStorage.setItem(
        orderKey(context),
        JSON.stringify({ v: 1, context, order_id: receipt.order_id }),
      );
      if (knownOrderID(context) !== receipt.order_id) throw new Error();
    } catch {
      throw new BuyerClientError("unavailable");
    }
    clearPending(pending);
    // Server GET, not the historical checkout receipt or elapsed browser timer,
    // decides whether this order is now DRAFT, cancelled, pending or confirmed.
    return readOrder(context, receipt.order_id);
  });
}
