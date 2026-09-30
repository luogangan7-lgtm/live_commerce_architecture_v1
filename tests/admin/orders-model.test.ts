import assert from "node:assert/strict";
import { test } from "node:test";
import {
  amountToMinor,
  minorToInput,
  parseOrderActions,
  parseOrderSummary,
  parseOrderList,
  parseOrderDetail,
  parseRefundList,
  parseShipmentHistory,
  refundAmountOK,
  trackingURLHost,
} from "../../apps/admin/lib/orders-model.ts";

const id = "11111111-1111-4111-8111-111111111111";
const sku = "22222222-2222-4222-8222-222222222222";
const summary = {
  order_id: id,
  created_at: "2026-09-27T00:00:00.000000Z",
  updated_at: "2026-09-27T00:00:00.000000Z",
  currency: "TWD",
  total_minor: 1250,
  commercial_state: "DRAFT",
  fulfillment_state: "MANUAL_UNASSIGNED",
  payment_state: "NOT_STARTED",
  test_mode: false,
  work_state: "NONE",
  refunded_minor: 0,
  refund_pending_minor: 0,
  // taiwan-cvs-logistics-v1 C4 keys (frozen 2026-09-30): a home card order carries these neutral values.
  pickup_source: null,
  payment_mode: "card",
  collection_state: null,
};
const detail = {
  ...summary,
  shipment: null,
  country: "TW",
  service_code: "home",
  items: [
    {
      sku_id: sku,
      code: "FROZEN-CODE",
      name: "Frozen item",
      quantity: 1,
      unit_price_minor: 1250,
      amount: {
        subtotal_minor: 1250,
        discount_minor: 0,
        tax_minor: 0,
        total_minor: 1250,
      },
    },
  ],
  totals: {
    subtotal_minor: 1250,
    discount_minor: 0,
    shipping_minor: 0,
    shipping_tax_minor: 0,
    tax_minor: 0,
    total_minor: 1250,
  },
  destination: {
    kind: "home",
    country: "TW",
    recipient_name: "Synthetic Buyer",
    phone: "+886900000001",
    home_address: {
      region: "",
      city: "Synthetic city",
      postal_code: "",
      line1: "Synthetic home address",
      line2: "",
    },
    pickup: null,
  },
};

test("MOU parser accepts exact frozen DTO and rejects malformed identity, money and state", () => {
  assert.equal(parseOrderSummary(summary).order_id, id);
  assert.equal(
    parseOrderList({ items: [summary], next_cursor: "" }).items.length,
    1,
  );
  assert.equal(
    parseOrderDetail(detail, id).destination.recipient_name,
    "Synthetic Buyer",
  );
  const bad = [
    { ...summary, order_id: [id] }, // String([id]) must not become authority.
    { ...summary, currency: ["TWD"] },
    { ...summary, currency: 123 }, // String(123) matches three letters only if coercion is loose.
    { ...summary, total_minor: Number.MAX_SAFE_INTEGER + 1 },
    { ...summary, work_state: "READY", payment_state: "PENDING" },
    { ...summary, test_mode: "false" },
    { ...summary, created_at: "2026-02-30T00:00:00.000000Z" },
  ];
  for (const row of bad)
    assert.throws(
      () => parseOrderSummary(row),
      /unavailable/,
      JSON.stringify(row),
    );
  assert.throws(
    () => parseOrderList({ items: [summary, summary], next_cursor: "" }),
    /unavailable/,
  );
  assert.throws(
    () => parseOrderList({ items: [summary], next_cursor: "x=" }),
    /unavailable/,
  );
});

test("MOU detail parser rejects spoofed recipient, pickup and mismatched money", () => {
  assert.throws(() => parseOrderDetail(detail, sku), /unavailable/);
  for (const change of [
    { country: ["TW"] },
    { service_code: ["home"] },
    { items: [{ ...detail.items[0], sku_id: [sku] }] },
    { items: [{ ...detail.items[0], code: ["FROZEN-CODE"] }] },
    { totals: { ...detail.totals, total_minor: 0 } },
    { destination: { ...detail.destination, phone: "javascript:alert(1)" } },
  ])
    assert.throws(
      () => parseOrderDetail({ ...detail, ...change }, id),
      /unavailable/,
    );
  const pickup = {
    ...detail,
    pickup_source: "merchant_attested",
    destination: {
      kind: "cvs_familymart",
      country: "TW",
      recipient_name: "Synthetic Recipient",
      phone: "+886900000002",
      home_address: {
        region: "",
        city: "",
        postal_code: "",
        line1: "",
        line2: "",
      },
      pickup: {
        kind: "cvs_familymart",
        namespace: "fixture.case",
        code: "017888",
        name: "Synthetic pickup",
        address: "Synthetic address",
        verification_kind: "MANUAL_ATTESTED",
      },
    },
  };
  assert.equal(parseOrderDetail(pickup, id).destination.pickup?.code, "017888");
  assert.throws(
    () =>
      parseOrderDetail(
        {
          ...pickup,
          destination: {
            ...pickup.destination,
            pickup: { ...pickup.destination.pickup, code: ["017888"] },
          },
        },
        id,
      ),
    /unavailable/,
  );
});

// --- refund-fulfilment-ui: stripe-refund-v1 §7.1 / manual-fulfilment-v1 §5.1 invariants -------------------------
const paid = { ...summary, commercial_state: "CONFIRMED", payment_state: "CAPTURED", work_state: "READY" };
const shipment = {
  version: 1, status: "SHIPPED", carrier_code: "seven_eleven_cvs", carrier_name: null,
  tracking_number: "0012345678", tracking_url: "https://t.example.tw/track", recorded_at: "2026-09-29T01:02:03.000000Z",
};

test("summary admits the refunded and shipped states with the amended invariants", () => {
  for (const row of [
    { ...paid, payment_state: "PARTIALLY_REFUNDED", refunded_minor: 500 },
    { ...paid, payment_state: "REFUNDED", refunded_minor: 1250 },
    { ...paid, fulfillment_state: "MERCHANT_SHIPPED" },
    { ...paid, payment_state: "REFUNDED", refunded_minor: 1250, fulfillment_state: "MERCHANT_SHIPPED" },
    { ...paid, payment_state: "REVIEW_REQUIRED" }, // READY work with a later review
  ])
    assert.equal(parseOrderSummary(row).order_id, id, JSON.stringify(row));
});

test("summary rejects the shipped/refunded combinations the contract forbids", () => {
  for (const row of [
    { ...paid, fulfillment_state: "MERCHANT_SHIPPED", work_state: "NONE" }, // MERCHANT_SHIPPED => READY
    { ...paid, commercial_state: "CANCELLED", fulfillment_state: "MERCHANT_SHIPPED" },
    { ...paid, payment_state: "REVIEW_REQUIRED", work_state: "REVIEW_REQUIRED", fulfillment_state: "MERCHANT_SHIPPED" }, // only the shipped rule rejects this
    { ...paid, payment_state: "PENDING" }, // READY => captured family
    { ...paid, work_state: "NONE" }, // CONFIRMED => work != NONE
    { ...paid, refunded_minor: -1 },
    { ...paid, refund_pending_minor: 1.5 },
    { ...paid, refunded_minor: undefined },
    (({ refunded_minor: _, ...rest }) => rest)(paid), // missing key
    { ...paid, extra: 1 },
    { ...paid, payment_state: "PARTIALLY_REFUNDED", work_state: "REVIEW_REQUIRED" }, // REVIEW_REQUIRED work => REVIEW payment
  ])
    assert.throws(() => parseOrderSummary(row), /unavailable/, JSON.stringify(row));
});

test("order filter values and detail shipment stay in step with MERCHANT_SHIPPED", () => {
  const shipped = { ...detail, ...paid, fulfillment_state: "MERCHANT_SHIPPED", shipment };
  assert.equal(parseOrderDetail(shipped, id).shipment?.tracking_number, "0012345678"); // leading zeroes kept
  assert.equal(parseOrderDetail({ ...detail, ...paid }, id).shipment, null);
  for (const change of [
    { shipment }, // head SHIPPED but state MANUAL_UNASSIGNED
    { fulfillment_state: "MERCHANT_SHIPPED", shipment: null },
    { fulfillment_state: "MERCHANT_SHIPPED", shipment: { ...shipment, status: "VOIDED" } },
    { fulfillment_state: "MERCHANT_SHIPPED", shipment: { ...shipment, carrier_code: "dhl" } },
    { fulfillment_state: "MERCHANT_SHIPPED", shipment: { ...shipment, carrier_code: "other" } }, // other needs a name
    { fulfillment_state: "MERCHANT_SHIPPED", shipment: { ...shipment, tracking_number: "12 " } },
    { fulfillment_state: "MERCHANT_SHIPPED", shipment: { ...shipment, tracking_url: "http://t.example.tw/x" } },
    { fulfillment_state: "MERCHANT_SHIPPED", shipment: { ...shipment, tracking_url: "javascript:alert(1)" } },
    { fulfillment_state: "MERCHANT_SHIPPED", shipment: { ...shipment, note: "merchant-only" } }, // note never in detail
    { fulfillment_state: "MERCHANT_SHIPPED", shipment: { ...shipment, version: 0 } },
  ])
    assert.throws(() => parseOrderDetail({ ...detail, ...paid, ...change }, id), /unavailable/, JSON.stringify(change));
  assert.throws(() => parseOrderDetail((({ shipment: _, ...rest }) => rest)(detail), id), /unavailable/);
});

test("tracking url rules: https only, dotted host, no userinfo, port, fragment or control characters", () => {
  assert.equal(trackingURLHost("https://t.cat.com.tw/a?b=1"), "t.cat.com.tw");
  for (const bad of [
    "http://t.cat.com.tw", "https://localhost/a", "https://u:p@t.cat.com.tw", "https://t.cat.com.tw:443/a",
    "https://t.cat.com.tw:8443/a", "https://t.cat.com.tw/a#x", "https://t.cat.com.tw/a b", "https://t.cat.com.tw/\u0000",
    "ftp://t.cat.com.tw", "//t.cat.com.tw", `https://t.cat.com.tw/${"a".repeat(512)}`,
  ])
    assert.equal(trackingURLHost(bad), null, bad);
});

const version = (over: object) => ({
  ...shipment, note: null, void_reason: null, principal_id: "33333333-3333-4333-8333-333333333333", ...over,
});
test("shipment history carries merchant-only fields and stays consistent", () => {
  const ok = parseShipmentHistory({
    items: [version({}), version({ version: 2, status: "VOIDED", void_reason: "wrong_tracking", note: "typo" })],
  });
  assert.equal(ok.length, 2);
  for (const items of [
    [version({ status: "VOIDED" })], // VOIDED needs a reason
    [version({ void_reason: "other" })], // SHIPPED must not carry one
    [version({}), version({})], // duplicate version
    [version({ principal_id: "not-a-uuid" })],
    [version({ note: "x".repeat(201) })],
    [{ ...version({}), extra: 1 }],
  ])
    assert.throws(() => parseShipmentHistory({ items }), /unavailable/, JSON.stringify(items));
  assert.throws(() => parseShipmentHistory({ items: [], extra: 1 }), /unavailable/);
});

const refundItem = (over: object) => ({
  refund_id: "44444444-4444-4444-8444-444444444444", amount_minor: 500, reason: "requested_by_customer", state: "PENDING",
  requested_at: "2026-09-29T01:00:00.000000Z", updated_at: "2026-09-29T01:00:05.000000Z", stripe_refund_id: null, ...over,
});
const refunds = (over: object = {}, items: object[] = []) => ({
  captured_minor: 1250, refunded_minor: 500, pending_minor: 250, refundable_minor: 500, currency: "TWD", items, ...over,
});
test("refund list parser checks capacity arithmetic, closed enums and optional failure_reason", () => {
  const ok = parseRefundList(
    refunds({}, [
      refundItem({}),
      refundItem({ refund_id: "55555555-5555-4555-8555-555555555555", state: "FAILED", failure_reason: "declined", stripe_refund_id: "re_3Abc" }),
    ]),
    "TWD",
  );
  assert.equal(ok.items[1].failure_reason, "declined");
  assert.equal(ok.items[0].failure_reason, undefined);
  for (const [value, currency] of [
    [refunds({ refundable_minor: 501 }), "TWD"], // captured != refunded + pending + refundable
    [refunds(), "USD"], // wrong currency
    [refunds({ extra: 1 }), "TWD"],
    [refunds({}, [refundItem({ state: "DONE" })]), "TWD"],
    [refunds({}, [refundItem({ reason: "fraudulent" })]), "TWD"],
    [refunds({}, [refundItem({ failure_reason: "made_up" })]), "TWD"],
    [refunds({}, [refundItem({ stripe_refund_id: "re bad" })]), "TWD"],
    [refunds({}, [refundItem({ amount_minor: 0 })]), "TWD"],
    [refunds({}, [refundItem({ updated_at: "2026-09-29T00:59:00.000000Z" })]), "TWD"],
    [refunds({}, [refundItem({}), refundItem({})]), "TWD"], // duplicate refund_id
    [refunds({}, Array.from({ length: 21 }, (_, n) => refundItem({ refund_id: `66666666-6666-4666-8666-${String(n).padStart(12, "0")}` }))), "TWD"],
  ] as const)
    assert.throws(() => parseRefundList(value, currency), /unavailable/, JSON.stringify(value));
});

test("order-actions is exactly three booleans", () => {
  assert.deepEqual(parseOrderActions({ refund: true, fulfillment_write: false, orders_export: true }),
    { refund: true, fulfillment_write: false, orders_export: true });
  for (const bad of [{ refund: true }, { refund: 1, fulfillment_write: false, orders_export: false },
    { refund: true, fulfillment_write: false, orders_export: false, x: true }])
    assert.throws(() => parseOrderActions(bad), /unavailable/);
});

test("refund amount entry converts exactly and enforces the TWD whole-dollar step as a hint", () => {
  assert.equal(amountToMinor("12.5", "USD"), 1250);
  assert.equal(amountToMinor("12.50", "TWD"), 1250);
  assert.equal(amountToMinor("0.1", "USD"), 10);
  assert.equal(amountToMinor("12.505", "USD"), null);
  assert.equal(amountToMinor("1e3", "USD"), null);
  assert.equal(amountToMinor("-1", "USD"), null);
  assert.equal(amountToMinor("", "USD"), null);
  assert.equal(amountToMinor("10", "JPY"), 10); // zero-digit currency: minor == major
  assert.equal(amountToMinor("10.5", "JPY"), null);
  assert.equal(minorToInput(250000, "TWD"), "2500");
  assert.equal(minorToInput(250050, "TWD"), "2500.50");
  assert.equal(minorToInput(1250, "USD"), "12.50");
  assert.equal(refundAmountOK(250000, 250000, "TWD"), true);
  assert.equal(refundAmountOK(250050, 250050, "TWD"), false); // not a multiple of 100
  assert.equal(refundAmountOK(250100, 250000, "TWD"), false); // above refundable
  assert.equal(refundAmountOK(0, 250000, "TWD"), false);
  assert.equal(refundAmountOK(null, 250000, "TWD"), false);
  assert.equal(refundAmountOK(1, 1250, "USD"), true);
});

// --- cvs-ui: taiwan-cvs-logistics-v1 §16 (pay-at-pickup, pickup_source, PROVIDER_LABEL_CREATED, cvs_pending) -----------
import { orderStates } from "../../apps/admin/lib/orders-model.ts";
// A pay-at-pickup order: CONFIRMED at placement, no payment attempt/work item/refund (payment NOT_STARTED, work NONE).
const pap = { ...summary, commercial_state: "CONFIRMED", payment_mode: "pay_at_pickup", collection_state: "PENDING", pickup_source: "buyer_entered" };
const cvsDest = (kind: string, verification_kind: string) => ({
  kind, country: "TW", recipient_name: "Synthetic Recipient", phone: "0912345678",
  home_address: { region: "", city: "", postal_code: "", line1: "", line2: "" },
  pickup: { kind, namespace: "buyer.11111111-1111-4111-8111-111111111111", code: "0012", name: "Synthetic store", address: "Synthetic address 1", verification_kind },
});

test("CVSA01 pay-at-pickup summary: CONFIRMED without payment is valid only in that mode", () => {
  for (const row of [
    pap,
    { ...pap, fulfillment_state: "MERCHANT_SHIPPED" },
    { ...pap, fulfillment_state: "MERCHANT_SHIPPED", collection_state: "COLLECTED" },
    { ...pap, fulfillment_state: "MERCHANT_SHIPPED", collection_state: "RETURNED" },
    { ...pap, fulfillment_state: "PROVIDER_LABEL_CREATED", pickup_source: "ecpay_directory" },
    { ...pap, fulfillment_state: "MERCHANT_SHIPPED", collection_state: "REFUNDED_OFFLINE" },
    { ...pap, fulfillment_state: "MERCHANT_SHIPPED", collection_state: "RESTOCKED" },
    { ...pap, commercial_state: "CANCELLED", fulfillment_state: "CANCELLED", collection_state: "CANCELLED" },
  ])
    assert.equal(parseOrderSummary(row).order_id, id, JSON.stringify(row));
  // the same numbers as a CARD order stay rejected (CONFIRMED needs captured payment + work)
  assert.throws(() => parseOrderSummary({ ...pap, payment_mode: "card", collection_state: null, pickup_source: null }), /unavailable/);
  for (const [name, row] of [
    ["pay_at_pickup without collection_state", { ...pap, collection_state: null }],
    ["card with collection_state", { ...summary, collection_state: "PENDING" }],
    ["unknown payment_mode", { ...pap, payment_mode: "cod" }],
    ["unknown collection_state", { ...pap, collection_state: "PAID" }],
    ["pickup_source outside the enum", { ...pap, pickup_source: "provider" }],
    ["pay_at_pickup with a payment attempt", { ...pap, payment_state: "CAPTURED", work_state: "READY" }],
    ["pay_at_pickup with a refund", { ...pap, refunded_minor: 500 }],
    ["pay_at_pickup test_mode", { ...pap, test_mode: true }],
    ["pay_at_pickup without a store source", { ...pap, pickup_source: null }],
    ["CANCELLED collection on a live order", { ...pap, collection_state: "CANCELLED" }],
    ["CANCELLED order with a PENDING collection", { ...pap, commercial_state: "CANCELLED", fulfillment_state: "CANCELLED" }],
    ["COLLECTED before shipment", { ...pap, collection_state: "COLLECTED" }],
    ["RESTOCKED before shipment", { ...pap, collection_state: "RESTOCKED" }],
    ["pay_at_pickup DRAFT", { ...pap, commercial_state: "DRAFT" }],
    ["buyer-entered store with an ECPay label", { ...paid, pickup_source: "buyer_entered", fulfillment_state: "PROVIDER_LABEL_CREATED" }],
    ["missing payment_mode", (({ payment_mode: _, ...rest }) => rest)(pap)],
  ] as const)
    assert.throws(() => parseOrderSummary(row), /unavailable/, name);
  // a card order with an ECPay label is a normal shipped state
  assert.equal(parseOrderSummary({ ...paid, pickup_source: "ecpay_directory", fulfillment_state: "PROVIDER_LABEL_CREATED" }).fulfillment_state, "PROVIDER_LABEL_CREATED");
  assert.throws(() => parseOrderSummary({ ...paid, fulfillment_state: "PROVIDER_LABEL_CREATED", work_state: "NONE" }), /unavailable/);
});

test("CVSA02 detail: pickup_source must agree with the pickup verification kind; new chains and kinds parse", () => {
  const base = { ...detail, ...pap };
  for (const [kind, verification, source] of [
    ["cvs_711", "BUYER_ENTERED", "buyer_entered"],
    ["cvs_hilife", "BUYER_ENTERED", "buyer_entered"],
    ["cvs_okmart", "BUYER_ENTERED", "buyer_entered"],
    ["cvs_711", "PROVIDER_DIRECTORY_VERIFIED", "ecpay_directory"],
    ["cvs_familymart", "MANUAL_ATTESTED", "merchant_attested"],
  ] as const)
    assert.equal(
      parseOrderDetail({ ...base, pickup_source: source, destination: cvsDest(kind, verification) }, id).destination.pickup?.verification_kind,
      verification,
    );
  for (const [name, change] of [
    ["buyer-entered pickup labelled as directory-verified", { pickup_source: "ecpay_directory", destination: cvsDest("cvs_711", "BUYER_ENTERED") }],
    ["directory pickup labelled buyer-entered", { pickup_source: "buyer_entered", destination: cvsDest("cvs_711", "PROVIDER_DIRECTORY_VERIFIED") }],
    ["CVS pickup without a source", { pickup_source: null, destination: cvsDest("cvs_711", "BUYER_ENTERED") }],
    ["unknown verification kind", { destination: cvsDest("cvs_711", "SELF_DECLARED") }],
    ["home destination with a pickup_source", { ...detail, ...pap, pickup_source: "buyer_entered" }],
  ] as const)
    assert.throws(() => parseOrderDetail({ ...base, ...change }, id), /unavailable/, name);
});

test("CVSA03 the cvs_pending list filter exists beside the older filters", () => {
  assert.ok(orderStates.includes("cvs_pending"));
  assert.ok(orderStates.includes("shipped") && orderStates.includes("unshipped"));
});
