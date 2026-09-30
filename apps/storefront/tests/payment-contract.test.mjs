import test from "node:test";
import assert from "node:assert/strict";
import {
  validOrderPayment,
  validPaymentPrepared,
  validHostedHandoff,
  validPaymentSignal,
  isStripeView,
} from "../lib/payment-contract.ts";

const orderID = "12345678-1234-1234-1234-123456789abc";
const expiry = "2026-09-25T01:02:03.123456789Z";
const method = {
  code: "payuni_credit",
  version: 1,
  name_hans: "测试",
  name_hant: "測試",
  name_en: "Mock",
};
const view = {
  order_id: orderID,
  currency: "TWD",
  total_minor: 2500,
  commercial_state: "DRAFT",
  payment_state: "NOT_STARTED",
  handoff_state: "NONE",
  handoff_expires_at: null,
  test_mode: true,
  methods: [method],
};
const prepared = {
  order_id: orderID,
  state: "PAYMENT_PENDING",
  currency: "TWD",
  amount_minor: 2500,
};
const form = {
  action: "https://sandbox-api.payuni.com.tw/api/upp",
  fields: {
    Version: "2.0",
    MerID: "synthetic_merchant",
    EncryptInfo: "ab".repeat(8),
    HashInfo: "A".repeat(64),
  },
};
const handoff = {
  order_id: orderID,
  disposition: "ISSUED",
  expires_at: expiry,
  form,
};

test("BPT03 exact valid projection and Go nanosecond UTC timestamps", () => {
  assert.equal(validOrderPayment(view, orderID), true);
  assert.equal(validPaymentPrepared(prepared, orderID, "payuni_credit"), true);
  assert.equal(validHostedHandoff(handoff, orderID), true);
  assert.equal(
    validHostedHandoff(
      { order_id: orderID, disposition: "ALREADY_ISSUED", expires_at: expiry },
      orderID,
    ),
    true,
  );
  for (const state of ["PENDING", "AUTHORIZED", "CAPTURED", "REVIEW_REQUIRED"])
    assert.equal(
      validOrderPayment({ ...view, payment_state: state, methods: [] }, orderID),
      true,
    );
  assert.equal(
    validOrderPayment(
      { ...view, payment_state: "PENDING", handoff_state: "UNAVAILABLE", handoff_expires_at: null, methods: [] },
      orderID,
    ),
    true,
  );
});

test("BPT03 view rejects private fields, inconsistent state and malformed names", () => {
  for (const [name, mutation] of [
    ["private attempt", { attempt_id: orderID }],
    ["wrong order", { order_id: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa" }],
    ["currency", { currency: "twD" }],
    ["negative amount", { total_minor: -1 }],
    ["unsafe amount", { total_minor: Number.MAX_SAFE_INTEGER }],
    ["state", { payment_state: "PAID" }],
    ["handoff state", { handoff_state: "SUCCESS" }],
    ["missing page timestamp", { handoff_state: "PREPARED" }],
    ["invented timestamp", { handoff_expires_at: expiry }],
    ["invalid calendar date", { handoff_state: "PREPARED", handoff_expires_at: "2026-02-30T00:00:00Z" }],
    ["local timestamp", { handoff_state: "PREPARED", handoff_expires_at: "2026-09-25T01:02:03+08:00" }],
    ["methods after pending", { payment_state: "PENDING" }],
    ["two methods", { methods: [method, method] }],
    ["method private field", { methods: [{ ...method, connection_id: orderID }] }],
    ["method code", { methods: [{ ...method, code: "payuni_atm" }] }],
    ["method version", { methods: [{ ...method, version: 0 }] }],
    ["blank name", { methods: [{ ...method, name_en: "  \t " }] }],
    ["control name", { methods: [{ ...method, name_en: "Mock\n" }] }],
    ["121 Unicode points", { methods: [{ ...method, name_en: "😀".repeat(121) }] }],
  ]) {
    const candidate = { ...view, ...mutation };
    assert.equal(validOrderPayment(candidate, orderID), false, name);
  }
  assert.equal(
    validOrderPayment({ ...view, methods: [{ ...method, name_en: "😀".repeat(120) }] }, orderID),
    true,
    "120 astral Unicode code points must not be counted as 240 UTF-16 units",
  );
});

test("BPT03 prepared receipt enforces exact original credit-card amount", () => {
  for (const candidate of [
    { ...prepared, attempt_id: orderID },
    { ...prepared, order_id: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa" },
    { ...prepared, state: "CAPTURED" },
    { ...prepared, currency: "USD" },
    { ...prepared, amount_minor: 0 },
    { ...prepared, amount_minor: 2501 },
    { ...prepared, amount_minor: 20000000 },
    { ...prepared, amount_minor: 1.5 },
  ]) assert.equal(validPaymentPrepared(candidate, orderID, "payuni_credit"), false);
  assert.equal(validPaymentPrepared({ ...prepared, amount_minor: 100 }, orderID, "payuni_credit"), true);
  assert.equal(validPaymentPrepared({ ...prepared, amount_minor: 19999900 }, orderID, "payuni_credit"), true);
});

test("BPT03 hosted form and replay are exact, bounded and action-allowlisted", () => {
  for (const [name, candidate] of [
    ["wrong order", { ...handoff, order_id: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa" }],
    ["bad disposition", { ...handoff, disposition: "PENDING" }],
    ["bad calendar", { ...handoff, expires_at: "2026-02-30T00:00:00Z" }],
    ["evil action", { ...handoff, form: { ...form, action: "https://evil.example/api/upp" } }],
    ["prefix action", { ...handoff, form: { ...form, action: form.action + "/evil" } }],
    ["form extra", { ...handoff, form: { ...form, target: "_blank" } }],
    ["field extra", { ...handoff, form: { ...form, fields: { ...form.fields, Credential: "secret" } } }],
    ["bad merid", { ...handoff, form: { ...form, fields: { ...form.fields, MerID: "bad id" } } }],
    ["short cipher", { ...handoff, form: { ...form, fields: { ...form.fields, EncryptInfo: "ab".repeat(7) } } }],
    ["odd cipher", { ...handoff, form: { ...form, fields: { ...form.fields, EncryptInfo: "a".repeat(17) } } }],
    ["uppercase cipher", { ...handoff, form: { ...form, fields: { ...form.fields, EncryptInfo: "AB".repeat(8) } } }],
    ["long cipher", { ...handoff, form: { ...form, fields: { ...form.fields, EncryptInfo: "ab".repeat(12289) } } }],
    ["lowercase hash", { ...handoff, form: { ...form, fields: { ...form.fields, HashInfo: "a".repeat(64) } } }],
    ["short hash", { ...handoff, form: { ...form, fields: { ...form.fields, HashInfo: "A".repeat(63) } } }],
    ["replay form", { ...handoff, disposition: "ALREADY_ISSUED" }],
    ["missing issued form", { order_id: orderID, disposition: "ISSUED", expires_at: expiry }],
  ]) assert.equal(validHostedHandoff(candidate, orderID), false, name);
});

// ---- SU01 (author-side): Stripe deltas, stripe-buyer-ui-v1 §3. Independent gates live elsewhere.
const stripeMethod = { ...method, code: "stripe_checkout", name_en: "Card" };
const stripeFresh = { ...view, methods: [stripeMethod] };
const attempt = (extra) => ({
  ...view, methods: [], cancel_requested: false, commercial_state: "AWAITING_PAYMENT",
  payment_state: "PENDING", handoff_state: "READY", handoff_expires_at: expiry, ...extra,
});
const checkout = "https://checkout.stripe.com/c/pay/cs_test_a1B2#fid";

test("SU01 Stripe views: fresh, both methods, attempt states, PAYUNi unchanged", () => {
  assert.equal(validOrderPayment(stripeFresh, orderID), true);
  assert.equal(isStripeView(stripeFresh), true);
  assert.equal(isStripeView(view), false);
  assert.equal(validOrderPayment({ ...view, methods: [method, stripeMethod] }, orderID), true);
  assert.equal(isStripeView({ ...view, methods: [method, stripeMethod] }), false);
  for (const handoff_state of ["CREATING", "READY", "CLOSED"])
    for (const cancel_requested of [false, true])
      assert.equal(validOrderPayment(attempt({ handoff_state, cancel_requested }), orderID), true, handoff_state);
  assert.equal(validOrderPayment(attempt({ payment_state: "CLOSED_UNPAID", commercial_state: "CANCELLED", handoff_state: "CLOSED", cancel_requested: true }), orderID), true);
  assert.equal(validOrderPayment(attempt({ handoff_state: "UNAVAILABLE", handoff_expires_at: null }), orderID), true);
  assert.equal(isStripeView(attempt({})), true);
  assert.equal(validOrderPayment(view, orderID), true, "PAYUNi view without cancel_requested");
});

test("SU01 Stripe view negatives", () => {
  for (const [name, candidate] of [
    ["CREATING without cancel_requested", (({ cancel_requested, ...rest }) => rest)(attempt({ handoff_state: "CREATING" }))],
    ["CLOSED without cancel_requested", (({ cancel_requested, ...rest }) => rest)(attempt({ handoff_state: "CLOSED" }))],
    ["CLOSED_UNPAID without cancel_requested", (({ cancel_requested, ...rest }) => rest)(attempt({ payment_state: "CLOSED_UNPAID" }))],
    ["CREATING without expiry", attempt({ handoff_state: "CREATING", handoff_expires_at: null })],
    ["READY with local expiry", attempt({ handoff_expires_at: "2026-09-25T01:02:03+08:00" })],
    ["cancel with PREPARED", attempt({ handoff_state: "PREPARED" })],
    ["cancel with ISSUED", attempt({ handoff_state: "ISSUED" })],
    ["cancel with EXPIRED", attempt({ handoff_state: "EXPIRED" })],
    ["cancel with methods", attempt({ methods: [stripeMethod] })],
    ["cancel not boolean", attempt({ cancel_requested: "false" })],
    ["cancel null", attempt({ cancel_requested: null })],
    ["cancel on fresh with methods", { ...stripeFresh, cancel_requested: false }],
    ["duplicate methods", { ...view, methods: [stripeMethod, stripeMethod] }],
    ["three methods", { ...view, methods: [method, stripeMethod, { ...method, code: "x" }] }],
    ["unknown method", { ...view, methods: [{ ...stripeMethod, code: "stripe_card" }] }],
    ["stripe method extra field", { ...view, methods: [{ ...stripeMethod, secret: "x" }] }],
    ["unknown extra key", attempt({ extra: 1 })],
    ["unknown payment state", attempt({ payment_state: "CLOSED" })],
  ]) assert.equal(validOrderPayment(candidate, orderID), false, name);
});

test("SU01 prepared amounts follow the corrected per-currency table", () => {
  const ok = (currency, amount_minor, method = "stripe_checkout") =>
    validPaymentPrepared({ ...prepared, currency, amount_minor }, orderID, method);
  for (const [currency, low, high] of [
    ["HKD", 400, 99999999], ["USD", 50, 99999999], ["SGD", 50, 99999999], ["MYR", 200, 99999999],
  ]) {
    assert.equal(ok(currency, low), true, currency);
    assert.equal(ok(currency, high), true, currency);
    assert.equal(ok(currency, low - 1), false, currency);
    assert.equal(ok(currency, high + 1), false, currency);
  }
  // TWD min 2500: Stripe SANDBOX rejected 100/1200, accepted 2500 (2026-09-29)
  assert.equal(ok("TWD", 2500), true);
  assert.equal(ok("TWD", 2499), false);
  assert.equal(ok("TWD", 100), false, "old TWD min");
  assert.equal(ok("TWD", 99999900), true);
  assert.equal(ok("TWD", 99), false);
  assert.equal(ok("TWD", 150), false, "TWD must be whole dollars");
  assert.equal(ok("TWD", 100000000), false);
  for (const currency of ["EUR", "JPY", "twd", "TWDX", "", null, undefined]) assert.equal(ok(currency, 5000), false, String(currency));
  assert.equal(ok("TWD", 2500, "payuni_credit"), true);
  assert.equal(ok("USD", 2500, "payuni_credit"), false, "PAYUNi stays TWD only");
  assert.equal(ok("TWD", 20000000, "payuni_credit"), false, "PAYUNi keeps its own upper bound");
  assert.equal(ok("TWD", 2500, "stripe"), false);
  assert.equal(validPaymentPrepared({ ...prepared, amount_minor: 2500 }, orderID, undefined), false, "method is required");
  assert.equal(ok("TWD", 1.5), false);
});

test("SU01 Stripe handoff union and every redirect_url negative", () => {
  const base = { order_id: orderID, expires_at: expiry };
  const redirect = { ...base, disposition: "REDIRECT", redirect_url: checkout };
  assert.equal(validHostedHandoff(redirect, orderID), true);
  assert.equal(validHostedHandoff(redirect, orderID, "stripe_checkout"), true);
  assert.equal(validHostedHandoff(redirect, orderID, "payuni_credit"), false);
  for (const disposition of ["CREATING", "CLOSED", "UNAVAILABLE"])
    assert.equal(validHostedHandoff({ ...base, disposition }, orderID, "stripe_checkout"), true, disposition);
  assert.equal(validHostedHandoff(handoff, orderID, "payuni_credit"), true);
  assert.equal(validHostedHandoff(handoff, orderID, "stripe_checkout"), false, "Stripe path rejects ISSUED");
  assert.equal(validHostedHandoff({ ...base, disposition: "ALREADY_ISSUED" }, orderID, "stripe_checkout"), false);
  for (const [name, url] of [
    ["http", "http://checkout.stripe.com/c/pay/x"],
    ["suffix host", "https://checkout.stripe.com.evil.example/c/pay/x"],
    ["userinfo", "https://checkout.stripe.com@evil.example/c/pay/x"],
    ["userinfo colon", "https://checkout.stripe.com:x@evil.example/"],
    ["explicit port", "https://checkout.stripe.com:443/c/pay/x"],
    ["upper-case host", "https://CHECKOUT.stripe.com/c/pay/x"],
    ["space", "https://checkout.stripe.com/c/pay/x y"],
    ["newline", "https://checkout.stripe.com/c/pay/x\ny"],
    ["tab", "https://checkout.stripe.com/c/\tpay"],
    ["non-ASCII", "https://checkout.stripe.com/c/pay/é"],
    ["4001 path chars", "https://checkout.stripe.com/" + "a".repeat(4001)],
    ["missing path", "https://checkout.stripe.com"],
    ["empty path", "https://checkout.stripe.com/"],
    ["other stripe host", "https://billing.stripe.com/p/x"],
    ["subdomain", "https://evil.checkout.stripe.com/x"],
    ["payuni form host", "https://sandbox-api.payuni.com.tw/api/upp"],
    ["empty", ""],
    ["null", null],
    ["number", 5],
  ]) assert.equal(validHostedHandoff({ ...redirect, redirect_url: url }, orderID), false, name);
  assert.equal(validHostedHandoff({ ...redirect, redirect_url: "https://checkout.stripe.com/" + "a".repeat(4000) }, orderID), true);
  for (const [name, candidate] of [
    ["URL on CREATING", { ...base, disposition: "CREATING", redirect_url: checkout }],
    ["URL on CLOSED", { ...base, disposition: "CLOSED", redirect_url: checkout }],
    ["URL on UNAVAILABLE", { ...base, disposition: "UNAVAILABLE", redirect_url: checkout }],
    ["REDIRECT without URL", { ...base, disposition: "REDIRECT" }],
    ["form on Stripe", { ...redirect, form }],
    ["form on CREATING", { ...base, disposition: "CREATING", form }],
    ["bad expiry", { ...redirect, expires_at: "2026-02-30T00:00:00Z" }],
    ["wrong order", { ...redirect, order_id: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa" }],
    ["extra key", { ...redirect, extra: 1 }],
    ["lowercase disposition", { ...redirect, disposition: "redirect" }],
    ["PAYUNi ISSUED with URL", { ...handoff, redirect_url: checkout }],
  ]) assert.equal(validHostedHandoff(candidate, orderID), false, name);
});

test("SU01 payment signal is exactly {order_id, scheduled:boolean}", () => {
  assert.equal(validPaymentSignal({ order_id: orderID, scheduled: true }, orderID), true);
  assert.equal(validPaymentSignal({ order_id: orderID, scheduled: false }, orderID), true);
  for (const candidate of [
    { order_id: orderID }, { scheduled: true }, { order_id: orderID, scheduled: "true" },
    { order_id: orderID, scheduled: 1 }, { order_id: orderID, scheduled: true, extra: 1 },
    { order_id: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", scheduled: true }, null, [], "x",
  ]) assert.equal(validPaymentSignal(candidate, orderID), false, JSON.stringify(candidate));
  assert.equal(validPaymentSignal({ order_id: orderID, scheduled: true }, "not-a-uuid"), false);
});

// ---- Refund view (stripe-refund-v1 §7.2): mirrors validPaymentViewFor in internal/checkout/payment_view.go.
const captured = (extra) => attempt({
  commercial_state: "CONFIRMED", payment_state: "CAPTURED", handoff_state: "CLOSED", cancel_requested: false, ...extra,
});
const refundJSON = (refunded_minor, pending_minor) => ({ refunded_minor, pending_minor });

test("RUI01 refund summary: refunded states, pending money, absent ≡ null", () => {
  for (const [name, candidate] of [
    ["CAPTURED without refund key", captured({})],
    ["CAPTURED refund null", captured({ refund: null })],
    ["CAPTURED pending only", captured({ refund: refundJSON(0, 2500) })],
    ["CAPTURED zero totals", captured({ refund: refundJSON(0, 0) })],
    ["PARTIALLY_REFUNDED", captured({ payment_state: "PARTIALLY_REFUNDED", refund: refundJSON(1000, 0) })],
    ["PARTIALLY_REFUNDED with pending remainder", captured({ payment_state: "PARTIALLY_REFUNDED", refund: refundJSON(1000, 1500) })],
    ["REFUNDED", captured({ payment_state: "REFUNDED", refund: refundJSON(2500, 0) })],
  ]) assert.equal(validOrderPayment(candidate, orderID), true, name);
  assert.equal(isStripeView(captured({ payment_state: "REFUNDED", refund: refundJSON(2500, 0) })), true);
});

test("RUI01 refund summary negatives", () => {
  for (const [name, candidate] of [
    ["PARTIALLY_REFUNDED without refund", captured({ payment_state: "PARTIALLY_REFUNDED" })],
    ["REFUNDED refund null", captured({ payment_state: "REFUNDED", refund: null })],
    ["PARTIALLY_REFUNDED zero refunded", captured({ payment_state: "PARTIALLY_REFUNDED", refund: refundJSON(0, 100) })],
    ["PARTIALLY_REFUNDED covering the total", captured({ payment_state: "PARTIALLY_REFUNDED", refund: refundJSON(2500, 0) })],
    ["REFUNDED below the total", captured({ payment_state: "REFUNDED", refund: refundJSON(2499, 0) })],
    ["refunded above total", captured({ refund: refundJSON(2501, 0) })],
    ["pending above remainder", captured({ refund: refundJSON(1000, 1501) })],
    ["negative refunded", captured({ refund: refundJSON(-1, 0) })],
    ["negative pending", captured({ refund: refundJSON(0, -1) })],
    ["fractional", captured({ refund: refundJSON(0.5, 0) })],
    ["string amount", captured({ refund: refundJSON("1", 0) })],
    ["extra refund key (reason leak)", captured({ refund: { ...refundJSON(1, 0), reason: "duplicate" } })],
    ["missing pending", captured({ refund: { refunded_minor: 1 } })],
    ["refund on PAYUNi view (no cancel_requested)", { ...view, payment_state: "CAPTURED", handoff_state: "ISSUED", handoff_expires_at: expiry, methods: [], refund: refundJSON(0, 1) }],
    ["REFUNDED on PAYUNi view", { ...view, payment_state: "REFUNDED", methods: [], refund: refundJSON(2500, 0) }],
  ]) assert.equal(validOrderPayment(candidate, orderID), false, name);
});
