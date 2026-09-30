// RF09 Node half (contracts/stripe-refund-v1.md §7.1 "merchant-orders-v1 projection invariants amended (A1)"
// and manual-fulfilment-v1.md §5.1/§5.2): the admin BFF model must decode exactly what Go emits after 0062/0063
// and enforce the SAME invariants. Independent of apps/admin: it imports only the public parser/grammar functions
// that already exist (parseOrderSummary/List/Detail, validOrdersQuery) and states the frozen expectations.
// Run: node --test --experimental-strip-types tests/admin/refund-bff.test.ts
// Not testable here without the Next runtime (no `@/` alias in plain Node): the BFF route grammar itself
// (GET|POST orders/{uuid}/refunds, POST .../refresh, PUT .../shipment, GET .../shipment/history,
// GET orders/unshipped.csv, GET order-actions); that stays a Playwright BFF spec concern.
import assert from "node:assert/strict";
import { test } from "node:test";
import { validOrdersQuery } from "../../apps/admin/lib/orders-request.ts";
import {
  parseOrderDetail,
  parseOrderList,
  parseOrderSummary,
} from "../../apps/admin/lib/orders-model.ts";

const id = "11111111-1111-4111-8111-111111111111";
const sku = "22222222-2222-4222-8222-222222222222";
const base = {
  order_id: id,
  created_at: "2026-09-27T00:00:00.000000Z",
  updated_at: "2026-09-27T00:00:00.000000Z",
  currency: "TWD",
  total_minor: 2500,
  commercial_state: "CONFIRMED",
  fulfillment_state: "MANUAL_UNASSIGNED",
  payment_state: "CAPTURED",
  test_mode: true,
  work_state: "READY",
  refunded_minor: 0,
  refund_pending_minor: 0,
  // taiwan-cvs-logistics-v1 C4: every summary/detail now carries these three keys (a card, home-delivery order here).
  pickup_source: null,
  payment_mode: "card",
  collection_state: null,
};
const row =(change: Record<string, unknown>) => ({ ...base, ...change });
const detailBase = {
  ...base,
  country: "TW",
  service_code: "home",
  items: [
    {
      sku_id: sku,
      code: "FROZEN-CODE",
      name: "Frozen item",
      quantity: 2,
      unit_price_minor: 1250,
      amount: { subtotal_minor: 2500, discount_minor: 0, tax_minor: 0, total_minor: 2500 },
    },
  ],
  totals: { subtotal_minor: 2500, discount_minor: 0, shipping_minor: 0, shipping_tax_minor: 0, tax_minor: 0, total_minor: 2500 },
  destination: {
    kind: "home",
    country: "TW",
    recipient_name: "Synthetic Buyer",
    phone: "+886900000001",
    home_address: { region: "", city: "Synthetic city", postal_code: "", line1: "Synthetic home address", line2: "" },
    pickup: null,
  },
  shipment: null as unknown,
};
const shipment = {
  version: 1,
  status: "SHIPPED",
  carrier_code: "seven_eleven_cvs",
  carrier_name: null,
  tracking_number: "0012345678",
  tracking_url: "https://track.example.com/t?n=0012345678",
  recorded_at: "2026-09-27T01:00:00.000000Z",
};

test("summary decodes refunded, partially refunded, shipped and reviewed orders (Go projection invariants)", () => {
  const accepted = [
    row({}),
    row({ payment_state: "REFUNDED", refunded_minor: 2500 }),
    row({ payment_state: "PARTIALLY_REFUNDED", refunded_minor: 1000 }),
    row({ payment_state: "PARTIALLY_REFUNDED", refunded_minor: 1000, refund_pending_minor: 500 }),
    row({ payment_state: "CAPTURED", refund_pending_minor: 1000 }), // a PENDING refund does not change payment_state (RD7)
    row({ fulfillment_state: "MERCHANT_SHIPPED" }),
    row({ fulfillment_state: "MERCHANT_SHIPPED", payment_state: "PARTIALLY_REFUNDED", refunded_minor: 500 }),
    row({ fulfillment_state: "MERCHANT_SHIPPED", payment_state: "REFUNDED", refunded_minor: 2500 }),
    // READY work with a refund review: payment REVIEW_REQUIRED while the work item stays READY (A1 guard)
    row({ payment_state: "REVIEW_REQUIRED" }),
    row({ payment_state: "REVIEW_REQUIRED", fulfillment_state: "MERCHANT_SHIPPED", refunded_minor: 500 }),
  ];
  for (const r of accepted) assert.equal(parseOrderSummary(r).order_id, id, JSON.stringify(r));
  assert.equal((parseOrderSummary(row({ payment_state: "REFUNDED", refunded_minor: 2500 })) as unknown as { refunded_minor: number }).refunded_minor, 2500);
  assert.equal(parseOrderList({ items: [accepted[1], row({ order_id: sku })], next_cursor: "" }).items.length, 2);
});

test("summary keys are exact: the pre-0062 ten-key shape and extra keys are refused", () => {
  const { refunded_minor: _a, refund_pending_minor: _b, ...old } = base;
  for (const r of [old, { ...base, refunded_minor: undefined }, { ...base, extra: 1 }, { ...base, refund_id: "x" }])
    assert.throws(() => parseOrderSummary(r), /unavailable/, JSON.stringify(r));
});

test("summary refuses every state combination the amended contract forbids", () => {
  // control: the unmodified row is valid, so every refusal below is caused by its one change (not by a shape error)
  assert.equal(parseOrderSummary(row({})).order_id, id);
  const forbidden: Array<[string, Record<string, unknown>]> = [
    ["READY work with payment PENDING", { payment_state: "PENDING" }],
    ["READY work with payment AUTHORIZED", { payment_state: "AUTHORIZED" }],
    ["READY work with payment NOT_STARTED", { payment_state: "NOT_STARTED", test_mode: false }],
    ["READY work while PAID_ALLOCATION_FAILED", { fulfillment_state: "PAID_ALLOCATION_FAILED" }],
    ["READY work while CANCELLED", { fulfillment_state: "CANCELLED", commercial_state: "CANCELLED" }],
    ["READY work on a non-CONFIRMED order", { commercial_state: "AWAITING_PAYMENT" }],
    ["MERCHANT_SHIPPED without READY work", { fulfillment_state: "MERCHANT_SHIPPED", work_state: "NONE" }],
    ["MERCHANT_SHIPPED with REVIEW_REQUIRED work", { fulfillment_state: "MERCHANT_SHIPPED", work_state: "REVIEW_REQUIRED", payment_state: "REVIEW_REQUIRED" }],
    ["MERCHANT_SHIPPED on a non-CONFIRMED order", { fulfillment_state: "MERCHANT_SHIPPED", commercial_state: "AWAITING_PAYMENT" }],
    ["CONFIRMED without work", { work_state: "NONE" }],
    ["CONFIRMED with payment PENDING", { payment_state: "PENDING", work_state: "REVIEW_REQUIRED" }],
    ["CONFIRMED with payment NOT_STARTED", { payment_state: "NOT_STARTED", work_state: "NONE", test_mode: false }],
    ["PAID_ALLOCATION_FAILED with payment CAPTURED", { fulfillment_state: "PAID_ALLOCATION_FAILED", work_state: "REVIEW_REQUIRED", payment_state: "CAPTURED" }],
    ["REVIEW_REQUIRED work with payment CAPTURED", { work_state: "REVIEW_REQUIRED", payment_state: "CAPTURED" }],
    ["NOT_STARTED with work READY", { commercial_state: "DRAFT", payment_state: "NOT_STARTED", test_mode: false, fulfillment_state: "MANUAL_UNASSIGNED" }],
    ["unknown payment state", { payment_state: "REFUND_PENDING" }],
    ["unknown fulfillment state", { fulfillment_state: "DELIVERED" }],
    ["unknown fulfillment state IN_TRANSIT", { fulfillment_state: "IN_TRANSIT" }],
    ["negative refunded_minor", { refunded_minor: -1 }],
    ["fractional refunded_minor", { refunded_minor: 1.5 }],
    ["string refunded_minor", { refunded_minor: "0" }],
    ["array refunded_minor", { refunded_minor: [0] }],
    ["unsafe refund_pending_minor", { refund_pending_minor: Number.MAX_SAFE_INTEGER + 1 }],
    ["null refund_pending_minor", { refund_pending_minor: null }],
  ];
  for (const [name, change] of forbidden)
    assert.throws(() => parseOrderSummary(row(change)), /unavailable/, name);
});

test("detail carries `shipment` (null or the exact seven-key SHIPPED object) and refuses anything else", () => {
  assert.equal(parseOrderDetail(detailBase, id).order_id, id);
  const shipped = { ...detailBase, fulfillment_state: "MERCHANT_SHIPPED", shipment };
  const parsed = parseOrderDetail(shipped, id) as unknown as { shipment: typeof shipment };
  assert.equal(parsed.shipment.tracking_number, "0012345678"); // leading zeroes survive
  assert.equal(parsed.shipment.carrier_code, "seven_eleven_cvs");
  const named = { ...shipped, shipment: { ...shipment, carrier_code: "other", carrier_name: "黑貓宅急便" } };
  assert.equal((parseOrderDetail(named, id) as unknown as { shipment: { carrier_name: string } }).shipment.carrier_name, "黑貓宅急便");
  for (const code of ["seven_eleven_cvs", "familymart_cvs", "hilife_cvs", "okmart_cvs", "sf_express", "chunghwa_post"])
    assert.doesNotThrow(() => parseOrderDetail({ ...shipped, shipment: { ...shipment, carrier_code: code } }, id), code);
  const { shipment: _omit, ...withoutKey } = detailBase;
  const badShipments: Array<[string, unknown]> = [
    ["missing `shipment` key", undefined],
    ["merchant-only note leaked into the order detail", { ...shipment, note: "x" }],
    ["void_reason leaked", { ...shipment, void_reason: null }],
    ["principal_id leaked", { ...shipment, principal_id: id }],
    ["VOIDED status (the detail carries only a SHIPPED head)", { ...shipment, status: "VOIDED" }],
    ["DELIVERED status (I13)", { ...shipment, status: "DELIVERED" }],
    ["unknown carrier", { ...shipment, carrier_code: "fedex" }],
    ["other without a name", { ...shipment, carrier_code: "other", carrier_name: null }],
    ["tracking number with an underscore", { ...shipment, tracking_number: "AB_1" }],
    ["tracking number over 64 chars", { ...shipment, tracking_number: "9".repeat(65) }],
    ["http tracking url", { ...shipment, tracking_url: "http://track.example.com/x" }],
    ["tracking url with userinfo", { ...shipment, tracking_url: "https://u:p@track.example.com/x" }],
    ["javascript url", { ...shipment, tracking_url: "javascript:alert(1)" }],
    ["array tracking number", { ...shipment, tracking_number: ["1"] }],
    ["version 0", { ...shipment, version: 0 }],
    ["bad recorded_at", { ...shipment, recorded_at: "2026-02-30T00:00:00.000000Z" }],
  ];
  for (const [name, bad] of badShipments) {
    const candidate = name.startsWith("missing") ? withoutKey : { ...shipped, shipment: bad };
    assert.throws(() => parseOrderDetail(candidate, id), /unavailable/, name);
  }
});

test("state filters admit shipped and unshipped and nothing else new", () => {
  const collection = "http://127.0.0.1:3100/api/stores/11111111-1111-4111-8111-111111111111/orders";
  for (const state of ["all", "DRAFT", "AWAITING_PAYMENT", "CONFIRMED", "CANCELLED", "shipped", "unshipped"])
    assert.equal(validOrdersQuery(`${collection}?state=${state}`, false), true, state);
  for (const state of ["delivered", "SHIPPED", "Unshipped", "refunded", "in_transit", "unshipped%20", ""])
    assert.equal(validOrdersQuery(`${collection}?state=${state}`, false), false, state);
  assert.equal(validOrdersQuery(`${collection}?limit=10&state=unshipped&cursor=A_-`, false), true);
});
