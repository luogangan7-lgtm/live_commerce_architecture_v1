import test from "node:test";
import assert from "node:assert/strict";
import { buyerDemoLabel } from "../lib/demo-label.ts";
import {
  cartSelection,
  lineSubtotal,
  parsePending,
  validCart,
  writePurchase,
  pendingPurchase,
  optionKey,
} from "../lib/purchase.ts";

const sku = "00000000-0000-0000-0000-000000000001";
const other = "00000000-0000-0000-0000-000000000002";
const context = "a".repeat(43);
const key = "00000000-0000-0000-0000-000000000003";
const cart = {
  id: key,
  currency: "TWD",
  version: 8,
  items: [
    { sku_id: other, quantity: 4 },
    { sku_id: sku, quantity: 1 },
  ],
};
test("demonstration disclosure is explicit and local-only", () => {
  assert.equal(
    buyerDemoLabel({
      COMMERCE_BUYER_DEMO_LABEL: "1",
      COMMERCE_BUYER_API_ORIGIN: "http://127.0.0.1:4321",
    }),
    true,
  );
  for (const env of [
    {},
    { COMMERCE_BUYER_API_ORIGIN: "http://127.0.0.1:4321" },
    {
      COMMERCE_BUYER_DEMO_LABEL: "1",
      COMMERCE_BUYER_API_ORIGIN: "https://api.example.com",
    },
  ])
    assert.equal(buyerDemoLabel(env), false);
});
test("cart selection preserves unrelated SKUs and the observed CAS version", () => {
  const next = cartSelection(cart, sku, 3);
  assert.equal(next.expected_version, 8);
  assert.deepEqual(next.items, [
    { sku_id: sku, quantity: 3 },
    { sku_id: other, quantity: 4 },
  ]);
  assert.equal(cart.items[1].quantity, 1);
  assert.equal(
    validCart({ ...cart, items: [...cart.items, cart.items[0]] }),
    false,
  );
  assert.throws(() => cartSelection(cart, sku, 0));
});
test("minor amounts are bounded before multiplication, including zero", () => {
  assert.equal(lineSubtotal(39000, 2), 78000);
  assert.equal(lineSubtotal(0, 1_000_000_000), 0);
  for (const [p, q] of [
    [1e12, 2],
    [1, 1.5],
    [-1, 1],
    [NaN, 1],
    [1, Infinity],
  ])
    assert.equal(lineSubtotal(p, q), null);
});
test("delivery identity includes country for one market and method", () => {
  assert.notEqual(
    optionKey({ market_id: key, country: "TW", method: "delivery:home" }),
    optionKey({ market_id: key, country: "HK", method: "delivery:home" }),
  );
});
test("journal is bound to context and rejects PII/unknown fields", () => {
  const value = {
    v: 1,
    kind: "cart",
    context,
    key,
    body: cartSelection(cart, sku, 3),
  };
  assert.deepEqual(parsePending(JSON.stringify(value), context), value);
  for (const bad of [
    { ...value, context: "b".repeat(43) },
    { ...value, body: { ...value.body, phone: "123456" } },
    { ...value, kind: "destination" },
    { ...value, token: "secret" },
    { ...value, key: "bad" },
  ])
    assert.throws(() => parsePending(JSON.stringify(bad), context));
});
test("lost response replays exact key/body, then clears only after parsed receipt", async () => {
  const oldFetch = globalThis.fetch;
  const makeStorage = () => {
    const data = new Map();
    return {
      getItem: (k) => data.get(k) ?? null,
      setItem: (k, v) => data.set(k, v),
      removeItem: (k) => data.delete(k),
    };
  };
  globalThis.localStorage = makeStorage();
  globalThis.sessionStorage = makeStorage();
  globalThis.window = { localStorage };
  const oldLocks = Object.getOwnPropertyDescriptor(navigator, "locks");
  Object.defineProperty(navigator, "locks", {
    configurable: true,
    value: { request: (...args) => args.at(-1)() },
  });
  const calls = [];
  globalThis.fetch = async (_url, init) => {
    if (_url.endsWith("/session"))
      return Response.json({
        state: "active",
        context,
        expires_at: new Date(Date.now() + 100000).toISOString(),
      });
    if (init.method === "GET") return Response.json({ ...cart, version: 10 });
    calls.push({
      body: init.body,
      key: new Headers(init.headers).get("Idempotency-Key"),
    });
    if (calls.length === 1) throw new Error("synthetic connection loss");
    return Response.json({ ...cart, version: 9 });
  };
  try {
    const input = { kind: "cart", body: cartSelection(cart, sku, 3) };
    await assert.rejects(writePurchase(context, input));
    assert.equal(pendingPurchase(context).kind, "cart");
    await assert.rejects(
      writePurchase(context, {
        kind: "cart",
        body: cartSelection(cart, sku, 7),
      }),
    );
    assert.equal(calls.length, 1);
    const result = await writePurchase(context);
    assert.equal(result.kind, "cart");
    assert.equal(result.value.version, 10);
    assert.deepEqual(calls[1], calls[0]);
    assert.equal(pendingPurchase(context), null);
  } finally {
    globalThis.fetch = oldFetch;
    if (oldLocks) Object.defineProperty(navigator, "locks", oldLocks);
    else delete navigator.locks;
    delete globalThis.window;
    delete globalThis.localStorage;
    delete globalThis.sessionStorage;
  }
});

// ---- Buyer shipment (manual-fulfilment-v1 §5.2/§3.2): mirrors internal/checkout.Get's drift rule.
import { validOrder, validOrderSummary, validTrackingURL } from "../lib/purchase.ts";
const shipOrder = (extra = {}) => ({
  order_id: key, cart_id: other, cart_version: 1, commercial_state: "CONFIRMED", fulfillment_state: "MERCHANT_SHIPPED",
  snapshot: {
    quote: { currency: "TWD", lines: [{ sku_id: sku, name: "Item", code: "S", quantity: 1, unit_price_minor: 2500 }],
      amount: { subtotal_minor: 2500, discount_minor: 0, shipping_minor: 0, shipping_tax_minor: 0, tax_minor: 0, total_minor: 2500 } },
    destination: { kind: "home", country: "TW", recipient_name: "R", phone: "1", home_address: { region: "", city: "C", postal_code: "", line1: "L", line2: "" } },
    service: { code: "home", name_hans: "a", name_hant: "b", name_en: "c", delivery_kind: "home", mode: "MANUAL" },
  },
  shipment: { status: "SHIPPED", carrier_code: "seven_eleven_cvs", carrier_name: null, tracking_number: "0012345678", tracking_url: "https://t.cat.com.tw/q?no=1", recorded_at: "2026-09-29T01:02:03Z" },
  ...extra,
});
const ship = (patch) => ({ ...shipOrder().shipment, ...patch });

test("RUI02 buyer order admits MERCHANT_SHIPPED only with a SHIPPED head; absent shipment ≡ null", () => {
  assert.equal(validOrder(shipOrder()), true);
  assert.equal(validOrder(shipOrder({ shipment: ship({ carrier_code: "other", carrier_name: "Local Courier", tracking_url: null }) })), true);
  assert.equal(validOrder(shipOrder({ fulfillment_state: "MANUAL_UNASSIGNED", shipment: null })), true);
  const { shipment, ...absent } = shipOrder({ fulfillment_state: "MANUAL_UNASSIGNED" });
  assert.equal(validOrder(absent), true);
  assert.equal(validOrder(shipOrder({ shipment: null })), false, "MERCHANT_SHIPPED without head");
  assert.equal(validOrder(shipOrder({ fulfillment_state: "MANUAL_UNASSIGNED" })), false, "head without MERCHANT_SHIPPED");
  assert.equal(validOrder({ ...absent, fulfillment_state: "MERCHANT_SHIPPED" }), false);
});

test("RUI02 shipment shape negatives (incl. unsafe links)", () => {
  for (const [name, patch] of [
    ["status VOIDED", { status: "VOIDED" }],
    ["unknown carrier", { carrier_code: "dhl" }],
    ["other without name", { carrier_code: "other", carrier_name: null }],
    ["control char in name", { carrier_name: "A\nB" }],
    ["name too long", { carrier_name: "x".repeat(81) }],
    ["empty tracking", { tracking_number: "" }],
    ["tracking with symbol", { tracking_number: "12;34" }],
    ["tracking trailing space", { tracking_number: "1234 " }],
    ["tracking 65 chars", { tracking_number: "1".repeat(65) }],
    ["javascript url", { tracking_url: "javascript:alert(1)" }],
    ["http url", { tracking_url: "http://t.cat.com.tw/q" }],
    ["userinfo", { tracking_url: "https://u:p@t.cat.com.tw/q" }],
    ["port", { tracking_url: "https://t.cat.com.tw:8443/q" }],
    ["fragment", { tracking_url: "https://t.cat.com.tw/q#x" }],
    ["dotless host", { tracking_url: "https://localhost/q" }],
    ["whitespace", { tracking_url: "https://t.cat.com.tw/a b" }],
    ["url over 512 bytes", { tracking_url: "https://t.cat.com.tw/" + "a".repeat(500) }],
    ["bad recorded_at", { recorded_at: "yesterday" }],
    ["extra merchant field (note)", { note: "internal" }],
    ["missing recorded_at", { recorded_at: undefined }],
  ]) {
    const s = ship(patch);
    if (patch.recorded_at === undefined && "recorded_at" in patch) delete s.recorded_at;
    assert.equal(validOrder(shipOrder({ shipment: s })), false, name);
  }
  assert.equal(validTrackingURL("https://t.cat.com.tw/q?no=1"), true);
});

test("RUI02 history summary admits MERCHANT_SHIPPED (a shipped order must not break the list)", () => {
  const summary = { order_id: key, cart_id: other, cart_version: 1, commercial_state: "CONFIRMED", fulfillment_state: "MERCHANT_SHIPPED", created_at: "2026-09-29T01:02:03Z", currency: "TWD", total_minor: 2500 };
  assert.equal(validOrderSummary(summary), true);
  assert.equal(validOrderSummary({ ...summary, fulfillment_state: "DELIVERED" }), false);
});

// ---- CVS pickup / pay-at-pickup validators (taiwan-cvs-logistics-v1 §5.1, §5.3, §16.1-§16.2) ----------------
import {
  checkoutInput,
  isUnavailable,
  validDestination,
  validDestinationWrite,
  validOption,
  validOptionRow,
  validQuote,
} from "../lib/purchase.ts";
const cu = (n) => `00000000-0000-0000-0000-${String(n).padStart(12, "0")}`;
const soon = new Date(Date.now() + 3_600_000).toISOString();
const homeOption = {
  market_id: cu(3), country: "TW", currency: "TWD", method: "delivery:home", delivery_kind: "home", mode: "MANUAL",
  service_version: 4, allocation_version: 5, name_hans: "a", name_hant: "b", name_en: "c", sort_order: 1,
};
const mapOption = {
  ...homeOption, method: "delivery:cvs-711", delivery_kind: "cvs_711", mode: "API",
  pickup_selection: "ecpay_map", payment_modes: ["card", "pay_at_pickup"],
};
const enteredOption = {
  ...homeOption, method: "delivery:cvs-ok", delivery_kind: "cvs_okmart",
  pickup_selection: "buyer_entered", payment_modes: ["card"], store_search_url: "https://www.okmart.com.tw/convenient_shopSearch",
};
const soonRow = {
  market_id: cu(3), country: "TW", currency: "TWD", method: "delivery:cvs-hilife", delivery_kind: "cvs_hilife",
  name_hans: "a", name_hant: "b", name_en: "c", sort_order: 3, available: false, reason: "coming_soon",
};

test("CVSP01 options: CVS rows carry pickup_selection/payment_modes; unavailable rows are a separate shape", () => {
  assert.equal(validOption(homeOption), true);
  assert.equal(validOption(mapOption), true);
  assert.equal(validOption(enteredOption), true);
  assert.equal(validOption({ ...mapOption, available: true }), true);
  assert.equal(validOptionRow(soonRow), true);
  assert.equal(isUnavailable(soonRow), true);
  assert.equal(isUnavailable(mapOption), false);
  assert.equal(validOptionRow({ ...soonRow, reason: "temporarily_unavailable" }), true);
  for (const [name, bad] of [
    ["CVS row without pickup_selection", (({ pickup_selection, ...r }) => r)(mapOption)],
    ["CVS row without payment_modes", (({ payment_modes, ...r }) => r)(mapOption)],
    ["unknown selection mode", { ...mapOption, pickup_selection: "merchant" }],
    ["payment_modes without card", { ...mapOption, payment_modes: ["pay_at_pickup"] }],
    ["payment_modes duplicate", { ...mapOption, payment_modes: ["card", "card"] }],
    ["unknown payment mode", { ...mapOption, payment_modes: ["card", "cash"] }],
    ["entered without search url", (({ store_search_url, ...r }) => r)(enteredOption)],
    ["entered with http url", { ...enteredOption, store_search_url: "http://www.okmart.com.tw/" }],
    ["map with search url", { ...mapOption, store_search_url: "https://x.test/" }],
    ["home with pickup_selection", { ...homeOption, pickup_selection: "ecpay_map" }],
    ["home with payment_modes", { ...homeOption, payment_modes: ["card"] }],
    ["CVS outside TW", { ...mapOption, country: "HK" }],
    ["available:false via validOption", soonRow],
    ["unknown kind", { ...homeOption, delivery_kind: "cvs_seven" }],
  ])
    assert.equal(validOption(bad), false, name);
  for (const [name, bad] of [
    ["unavailable home row", { ...soonRow, delivery_kind: "home" }],
    ["unknown reason", { ...soonRow, reason: "later" }],
    ["reason without available:false", (({ available, ...r }) => r)(soonRow)],
    ["unavailable without labels", (({ name_en, ...r }) => r)(soonRow)],
  ])
    assert.equal(validOptionRow(bad), false, name);
});

const cq = {
  id: cu(4), cart_id: cu(1), cart_version: 2, market_id: cu(3), country: "TW", method: mapOption.method, currency: "TWD",
  expires_at: soon,
  lines: [{ sku_id: cu(2), name: "N", code: "C", quantity: 1, unit_price_minor: 50000 }],
  amount: { subtotal_minor: 50000, discount_minor: 0, shipping_minor: 0, shipping_tax_minor: 0, tax_minor: 0, total_minor: 50000 },
};
const ccart = { id: cu(1), version: 2, currency: "TWD", items: [{ sku_id: cu(2), quantity: 1 }] };
const cvsWrite = {
  expected_version: 0, cart_version: 2, kind: "cvs_711", country: "TW", recipient_name: "王小明", phone: "0912345678",
  home_address: { region: "", city: "", postal_code: "", line1: "", line2: "" }, pickup_id: cu(50),
};
const cvsHead = (over = {}) => ({
  ...(({ expected_version, pickup_id, ...d }) => d)(cvsWrite),
  id: cu(60), version: 1, cart_id: cu(1), selected_at: new Date().toISOString(), expires_at: soon,
  pickup: { id: cu(50), kind: "cvs_711", namespace: "ecpay.UNIMARTC2C", code: "123456", name: "S", address: "Synthetic Address", country: "TW", verification_kind: "PROVIDER_DIRECTORY_VERIFIED" },
  ...over,
});

test("CVSP02 CVS destination write: TW only, pickup_id required, empty home address; home unchanged", () => {
  assert.equal(validDestinationWrite(cvsWrite), true);
  assert.equal(validDestinationWrite({ ...cvsWrite, kind: "cvs_hilife" }), true);
  for (const [name, bad] of [
    ["no pickup_id", (({ pickup_id, ...r }) => r)(cvsWrite)],
    ["bad pickup_id", { ...cvsWrite, pickup_id: "x" }],
    ["home address filled", { ...cvsWrite, home_address: { ...cvsWrite.home_address, city: "C" } }],
    ["not TW", { ...cvsWrite, country: "HK" }],
    ["unknown kind", { ...cvsWrite, kind: "cvs_seven" }],
    ["untrimmed phone", { ...cvsWrite, phone: " 0912345678" }],
    ["extra key", { ...cvsWrite, store_code: "123456" }],
  ])
    assert.equal(validDestinationWrite(bad), false, name);
  const home = { ...cvsWrite, kind: "home", home_address: { region: "", city: "C", postal_code: "", line1: "L", line2: "" } };
  assert.equal(validDestinationWrite(home), false, "home write must not carry pickup_id");
  const { pickup_id, ...homeOnly } = home;
  assert.equal(validDestinationWrite(homeOnly), true);
  assert.equal(validDestination(cvsHead()), true);
  assert.equal(validDestination(cvsHead({ kind: "cvs_okmart" })), true);
  assert.equal(validDestination(cvsHead({ pickup: undefined })), false, "CVS head needs its pickup");
});

test("CVSP03 checkoutInput: payment_mode only for CVS rows and only a mode the row offers", () => {
  const head = cvsHead();
  assert.deepEqual(checkoutInput(cq, mapOption, ccart, head), {
    quote_id: cq.id, destination_id: head.id, cart_version: 2, service_version: 4, allocation_version: 5, payment_mode: "card",
  });
  assert.equal(checkoutInput(cq, mapOption, ccart, head, Date.now(), "pay_at_pickup").payment_mode, "pay_at_pickup");
  assert.throws(() => checkoutInput(cq, { ...mapOption, payment_modes: ["card"] }, ccart, head, Date.now(), "pay_at_pickup"), "mode not offered");
  assert.throws(() => checkoutInput(cq, mapOption, ccart, cvsHead({ kind: "cvs_okmart", pickup: { ...head.pickup, kind: "cvs_okmart" } })), "destination kind differs from option");
  assert.throws(() => checkoutInput(cq, mapOption, ccart, cvsHead({ pickup: { ...head.pickup, kind: "cvs_okmart" } })), "pickup kind differs from option");
  const homeQuote = { ...cq, method: homeOption.method };
  const homeHead = {
    ...(({ pickup_id, expected_version, ...d }) => d)(cvsWrite), kind: "home", id: cu(61), version: 1, cart_id: cu(1),
    home_address: { region: "", city: "C", postal_code: "", line1: "L", line2: "" }, selected_at: new Date().toISOString(), expires_at: soon,
  };
  assert.equal("payment_mode" in checkoutInput(homeQuote, homeOption, ccart, homeHead), false, "home body is unchanged");
  assert.throws(() => checkoutInput(homeQuote, homeOption, ccart, homeHead, Date.now(), "card"), "home never sends a mode");
  assert.throws(() => checkoutInput(cq, soonRow, ccart, head), "unavailable row is not checkout-able");
});

const cvsOrder = (extra = {}, dest = {}) => ({
  order_id: cu(6), cart_id: cu(1), cart_version: 2, commercial_state: "CONFIRMED", fulfillment_state: "MANUAL_UNASSIGNED",
  snapshot: {
    quote: { currency: "TWD", lines: cq.lines, amount: cq.amount },
    destination: { kind: "cvs_711", country: "TW", recipient_name: "R", phone: "0912345678", home_address: { region: "", city: "", postal_code: "", line1: "", line2: "" },
      pickup: { kind: "cvs_711", namespace: "buyer.x", code: "123456", name: "S", address: "A", country: "TW" }, ...dest },
    service: { code: "cvs-711", name_hans: "a", name_hant: "b", name_en: "c", delivery_kind: "cvs_711", mode: "MANUAL" },
  },
  shipment: null,
  ...extra,
});
const cvsShipment = { state: "CREATED", chain: "cvs_711", store_name: "S", store_code: "123456", updated_at: "2026-09-30T01:02:03Z" };

test("CVSP04 buyer order: pay_at_pickup <=> collection_state; cvs_shipment only on a matching CVS destination", () => {
  assert.equal(validOrder(cvsOrder({ payment_mode: "pay_at_pickup", collection_state: "PENDING", cvs_shipment: null })), true);
  assert.equal(validOrder(cvsOrder({ payment_mode: "card", collection_state: null, cvs_shipment: cvsShipment, fulfillment_state: "PROVIDER_LABEL_CREATED" })), true);
  assert.equal(validOrder(cvsOrder()), true, "absent keys read as card/null/null");
  for (const state of ["COLLECTED", "RETURNED", "REFUNDED_OFFLINE", "CANCELLED", "RESTOCKED"])
    assert.equal(validOrder(cvsOrder({ payment_mode: "pay_at_pickup", collection_state: state })), true, state);
  for (const [name, patch] of [
    ["pay_at_pickup without collection_state", { payment_mode: "pay_at_pickup", collection_state: null }],
    ["card with collection_state", { payment_mode: "card", collection_state: "PENDING" }],
    ["unknown payment mode", { payment_mode: "cash" }],
    ["unknown collection state", { payment_mode: "pay_at_pickup", collection_state: "PAID" }],
    ["shipment extra key", { cvs_shipment: { ...cvsShipment, provider_logistics_id: "x" } }],
    ["shipment unknown state", { cvs_shipment: { ...cvsShipment, state: "DELIVERED" } }],
    ["shipment chain differs from destination", { cvs_shipment: { ...cvsShipment, chain: "cvs_okmart" } }],
  ])
    assert.equal(validOrder(cvsOrder(patch)), false, name);
  assert.equal(
    validOrder(cvsOrder({ cvs_shipment: cvsShipment }, { kind: "home", pickup: undefined, home_address: { region: "", city: "C", postal_code: "", line1: "L", line2: "" } })),
    false,
    "home destination cannot have a CVS shipment",
  );
});

test("CVSP05 journal accepts a CVS destination marker and a checkout body with payment_mode, nothing else", () => {
  const ctx = "a".repeat(43);
  const marker = { v: 1, context: ctx, key: cu(9), kind: "destination", body: { expected_version: 0, cart_version: 2, kind: "cvs_711", country: "TW" } };
  assert.deepEqual(parsePending(JSON.stringify(marker), ctx), marker);
  assert.throws(() => parsePending(JSON.stringify({ ...marker, body: { ...marker.body, pickup_id: cu(50) } }), ctx));
  const checkout = { v: 1, context: ctx, key: cu(9), kind: "checkout",
    body: { quote_id: cu(4), destination_id: cu(60), cart_version: 2, service_version: 4, allocation_version: 5, payment_mode: "pay_at_pickup" } };
  assert.deepEqual(parsePending(JSON.stringify(checkout), ctx), checkout);
  assert.throws(() => parsePending(JSON.stringify({ ...checkout, body: { ...checkout.body, payment_mode: "cash" } }), ctx));
  assert.throws(() => parsePending(JSON.stringify({ ...checkout, body: { ...checkout.body, recipient_name: "x" } }), ctx));
});
