// CB09 Node half (contracts/customers-billing-v1.md §5, customers-core.md D5/D6/D8/D12/D13 + FROZEN DTOs,
// billing-core.md FROZEN DTOs/B3): the admin BFF fence and decoders must admit exactly the eleven §5 resources,
// enforce the Idempotency-Key rule (required on the three customer POSTs, forbidden everywhere else), refuse
// every body but the frozen ones, and decode exactly what Go emits. Independent of the UI unit's own tests:
// only the public pure functions (customersRoute, validCustomersQuery/Request/Body, parse*) are imported and the
// frozen expectations are restated here. Not testable without the Next runtime (no `@/` alias in plain Node):
// the route.ts wiring itself; that stays a Playwright BFF concern (customers-billing.spec.ts).
// Run: node --test --experimental-strip-types tests/admin/customers-bff.test.ts
import assert from "node:assert/strict";
import { test } from "node:test";
import {
  customersRoute,
  validCustomersBody,
  validCustomersQuery,
  validCustomersRequest,
  type CustomersRouteKind,
} from "../../apps/admin/lib/customers-request.ts";
import {
  parseCustomer,
  parseCustomerDetail,
  parseCustomerList,
  parseErasureSummary,
  parseFinanceSummary,
} from "../../apps/admin/lib/customers-model.ts";
import { parseBillingStatus, parseRedirect, parseStanding } from "../../apps/admin/lib/billing-model.ts";

const id = "11111111-1111-4111-8111-111111111111";
const other = "22222222-2222-4222-8222-222222222222";
const withLetters = "abcdefab-abcd-4abc-8abc-abcdefabcdef";

// ---------------------------------------------------------------------------------------------- grammar
const table: [string, string, CustomersRouteKind][] = [
  ["GET", "customers", "list"],
  ["GET", `customers/${id}`, "detail"],
  ["POST", `customers/${id}/consent-withdrawals`, "withdraw"],
  ["POST", `customers/${id}/exports`, "export"],
  ["POST", `customers/${id}/erasure`, "erase"],
  ["GET", "finance/summary", "finance"],
  ["GET", "finance/summary.csv", "finance-csv"],
  ["GET", "billing", "billing"],
  ["GET", "billing/standing", "standing"],
  ["POST", "billing/checkout", "checkout"],
  ["POST", "billing/portal", "portal"],
];

test("exactly the eleven contract §5 resources are admitted, each only under its own method", () => {
  for (const [method, path, kind] of table) {
    assert.equal(customersRoute(method, path), kind, `${method} ${path}`);
    for (const wrong of ["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"].filter((m) => m !== method)) {
      assert.equal(customersRoute(wrong, path), null, `${wrong} ${path} must not be admitted`);
    }
    assert.equal(customersRoute(method.toLowerCase(), path), null, "methods are case-sensitive");
  }
});

test("near misses are refused (no generic proxying)", () => {
  const nope: [string, string][] = [
    ["GET", "customers/"], ["GET", "/customers"], ["GET", "customers/not-a-uuid"], ["GET", `customers/${withLetters.toUpperCase()}`],
    ["GET", `customers/${id}/`], ["GET", `customers/${id}/orders`], ["GET", `customers/${id}/exports`],
    ["POST", `customers/${id}/consent-withdrawal`], ["POST", `customers/${id}/export`], ["POST", `customers/${id}/erase`],
    ["POST", `customers/${id}/consents`], ["POST", `customers/${id}/exports/x`], ["POST", "customers"], ["POST", `customers/${id}`],
    ["GET", "finance"], ["GET", "finance/summary.json"], ["GET", "finance/summary/"], ["GET", "finance/summary.csv/x"], ["POST", "finance/summary"],
    ["GET", "billing/"], ["GET", "billing/checkout"], ["GET", "billing/portal"], ["POST", "billing"], ["POST", "billing/standing"],
    ["POST", "billing/checkout/x"], ["POST", "billing/portal/x"], ["GET", "billing/invoices"], ["GET", "billing/subscriptions"],
    ["GET", "customers/../orders"], ["GET", `customers/${id}/../${other}`], ["GET", "customers%2F"], ["GET", "customers?x=1"], ["GET", ""],
    ["GET", "customers/00000000-0000-0000-0000-00000000000g"], ["POST", `customers/${id}%2Fexports`],
  ];
  for (const [method, path] of nope) assert.equal(customersRoute(method, path), null, `${method} ${path}`);
});

// ---------------------------------------------------------------------------------------------- query
const url = (path: string, query = "") => `https://admin.example.test/api/stores/${id}/${path}${query}`;

test("customers list query: limit 1..100, opaque after, q 1..40, nothing else", () => {
  const ok = ["", "?limit=1", "?limit=100", "?limit=50&after=abc_DEF-123", "?q=Ali", "?q=%E7%8E%8B%E5%B0%8F%E6%98%8E", `?q=${"a".repeat(40)}`, "?limit=10&after=abc&q=x", "?after=" + "A".repeat(1024)];
  for (const q of ok) assert.equal(validCustomersQuery("list", url("customers", q)), true, q);
  const bad = [
    "?", "?limit=0", "?limit=101", "?limit=-1", "?limit=abc", "?limit=", "?limit=01", "?limit=1&limit=2", "?x=1", "?limit=1&x=1",
    "?after=", "?after=has space", "?after=a/b", "?after=" + "A".repeat(1025), "?q=", "?q=%20", "?q=%20%20", `?q=${"a".repeat(41)}`,
    "?q=a%00b", "?q=a%0Ab", "?q=a%zzb", "?from=2026-01-01&to=2026-01-02", "?limit=1;limit=2", "?limit==1", "?q=a=b", "?q=a#b",
  ];
  for (const q of bad) assert.equal(validCustomersQuery("list", url("customers", q)), false, q);
});

test("finance query: from and to are required calendar dates, 0..91 days apart", () => {
  for (const kind of ["finance", "finance-csv"] as const) {
    const path = kind === "finance" ? "finance/summary" : "finance/summary.csv";
    for (const q of ["?from=2026-01-01&to=2026-01-01", "?from=2026-01-01&to=2026-04-02", "?to=2026-02-01&from=2026-01-01", "?from=2024-02-29&to=2024-03-01"]) {
      assert.equal(validCustomersQuery(kind, url(path, q)), true, `${kind} ${q}`);
    }
    for (const q of [
      "", "?from=2026-01-01", "?to=2026-01-01", "?from=2026-01-01&to=2026-04-03", "?from=2026-02-01&to=2026-01-01", "?from=2026-1-1&to=2026-01-31",
      "?from=2026-02-30&to=2026-03-01", "?from=2025-02-29&to=2025-03-01", "?from=2026-01-01T00:00:00Z&to=2026-01-31", "?from=20260101&to=20260131",
      "?from=2026-01-01&to=2026-01-31&x=1", "?from=2026-01-01&from=2026-01-02&to=2026-01-31", "?from=&to=", "?limit=1&from=2026-01-01&to=2026-01-31",
    ]) {
      assert.equal(validCustomersQuery(kind, url(path, q)), false, `${kind} ${q}`);
    }
  }
});

test("every other resource takes no query at all", () => {
  for (const [, path, kind] of table) {
    if (kind === "list" || kind === "finance" || kind === "finance-csv") continue;
    for (const q of ["?x=1", "?", "?limit=1"]) assert.equal(validCustomersQuery(kind, url(path, q)), false, `${kind} ${q}`);
    assert.equal(validCustomersQuery(kind, url(path)), true, kind);
  }
});

// ---------------------------------------------------------------------------------------------- headers
const req = (method: string, path: string, headers: Record<string, string> = {}, query = "", body?: string) =>
  new Request(url(path, query), { method, headers, ...(body !== undefined ? { body } : {}) });
const key = "cbff-key-12345678";

test("Idempotency-Key: required (and well-formed) on the three customer POSTs, forbidden everywhere else", () => {
  const post = (path: string, headers: Record<string, string>, body?: string) => req("POST", path, headers, "", body);
  const json = { "content-type": "application/json" };
  const withdraw = JSON.stringify({ purpose: "marketing_messages", channel: "meta_dm" });
  for (const [path, kind, body, h] of [
    [`customers/${id}/consent-withdrawals`, "withdraw", withdraw, json],
    [`customers/${id}/exports`, "export", "", {}],
    [`customers/${id}/erasure`, "erase", JSON.stringify({ confirm: "ERASE" }), json],
  ] as const) {
    const init = body === "" ? undefined : body;
    assert.equal(validCustomersRequest(kind, post(path, { ...h, "idempotency-key": key }, init)), true, `${kind} with a key`);
    assert.equal(validCustomersRequest(kind, post(path, { ...h }, init)), false, `${kind} without a key`);
    for (const bad of ["short", "has space here", "a".repeat(129), "bad/char/12345", ""]) {
      assert.equal(validCustomersRequest(kind, post(path, { ...h, "idempotency-key": bad }, init)), false, `${kind} key ${JSON.stringify(bad)}`);
    }
  }
  for (const [method, path, kind] of table) {
    if (["withdraw", "export", "erase"].includes(kind)) continue;
    const headers: Record<string, string> = method === "POST" && kind === "checkout" ? { "content-type": "application/json" } : {};
    const body = kind === "checkout" ? JSON.stringify({ price_id: "price_A1" }) : undefined;
    const query = kind === "finance" || kind === "finance-csv" ? "?from=2026-01-01&to=2026-01-31" : "";
    assert.equal(validCustomersRequest(kind, req(method, path, headers, query, body)), true, `${kind} keyless`);
    assert.equal(validCustomersRequest(kind, req(method, path, { ...headers, "idempotency-key": key }, query, body)), false, `${kind} with a key must be refused`);
  }
});

test("bodies: JSON content type where a body is read, none where it is not, never chunked", () => {
  const json = { "content-type": "application/json", "idempotency-key": key };
  const erase = JSON.stringify({ confirm: "ERASE" });
  assert.equal(validCustomersRequest("erase", req("POST", `customers/${id}/erasure`, json, "", erase)), true);
  assert.equal(validCustomersRequest("erase", req("POST", `customers/${id}/erasure`, { ...json, "content-type": "text/plain" }, "", erase)), false);
  assert.equal(validCustomersRequest("erase", req("POST", `customers/${id}/erasure`, { "idempotency-key": key }, "", erase)), false, "no content type");
  assert.equal(validCustomersRequest("erase", req("POST", `customers/${id}/erasure`, { ...json, "content-type": "application/json; charset=utf-8" }, "", erase)), true);
  assert.equal(validCustomersRequest("erase", req("POST", `customers/${id}/erasure`, { ...json, "transfer-encoding": "chunked" }, "", erase)), false);
  assert.equal(validCustomersRequest("export", req("POST", `customers/${id}/exports`, { "idempotency-key": key }, "", "x")), true, "content-length is checked by the transport, not by the body of a Request object built in-process");
  assert.equal(validCustomersRequest("export", req("POST", `customers/${id}/exports`, { "idempotency-key": key, "content-length": "5" })), false, "an export declaring a body");
  assert.equal(validCustomersRequest("portal", req("POST", "billing/portal", { "content-length": "2" })), false, "a portal call declaring a body");
  assert.equal(validCustomersRequest("portal", req("POST", "billing/portal")), true);
  assert.equal(validCustomersRequest("list", req("GET", "customers", { "transfer-encoding": "chunked" })), false);
});

test("body content: the frozen shapes only", () => {
  const ok = (kind: CustomersRouteKind, value: unknown) => validCustomersBody(kind, typeof value === "string" ? value : JSON.stringify(value));
  // withdrawal: exactly {purpose, channel} and only the two valid pairs (CD4, D12)
  assert.equal(ok("withdraw", { purpose: "marketing_messages", channel: "meta_dm" }), true);
  assert.equal(ok("withdraw", { purpose: "ads_personalization", channel: "meta_ads" }), true);
  for (const b of [
    { purpose: "marketing_messages", channel: "meta_ads" }, { purpose: "ads_personalization", channel: "meta_dm" }, { purpose: "sms", channel: "meta_dm" },
    { purpose: "marketing_messages" }, { channel: "meta_dm" }, {}, { purpose: "marketing_messages", channel: "meta_dm", granted: true },
    { purpose: "marketing_messages", channel: "meta_dm", source: "merchant_recorded" }, { purpose: "marketing_messages", channel: "meta_dm", policy_version: "x" },
    { purpose: "MARKETING_MESSAGES", channel: "meta_dm" }, { purpose: ["marketing_messages"], channel: "meta_dm" }, [], null, "x", 1,
  ]) assert.equal(ok("withdraw", b), false, JSON.stringify(b));
  assert.equal(ok("withdraw", ""), false);
  assert.equal(ok("withdraw", "not json"), false);
  // erasure: exactly {"confirm":"ERASE"}
  assert.equal(ok("erase", { confirm: "ERASE" }), true);
  for (const b of [{ confirm: "erase" }, { confirm: "ERASE " }, { confirm: " ERASE" }, { confirm: "" }, { confirm: true }, {}, { confirm: "ERASE", force: true }, [], null, "ERASE", ""])
    assert.equal(ok("erase", b), false, JSON.stringify(b));
  // checkout: exactly {price_id: price_...}
  assert.equal(ok("checkout", { price_id: "price_A1b2C3" }), true);
  for (const b of [{ price_id: "plan_A1" }, { price_id: "price_" }, { price_id: "price_a b" }, { price_id: `price_${"a".repeat(65)}` }, {}, { price_id: "price_A1", x: 1 }, { price_id: 1 }, [], null])
    assert.equal(ok("checkout", b), false, JSON.stringify(b));
  // export and portal carry no body
  for (const kind of ["export", "portal"] as const) {
    assert.equal(validCustomersBody(kind, ""), true, kind);
    for (const b of ["{}", " ", "null", "x"]) assert.equal(validCustomersBody(kind, b), false, `${kind} ${JSON.stringify(b)}`);
  }
  for (const kind of ["list", "detail", "finance", "finance-csv", "billing", "standing"] as const) assert.equal(validCustomersBody(kind, ""), true, kind);
});

// ---------------------------------------------------------------------------------------------- decoders
const t0 = "2026-09-27T00:00:00.000000Z";
const customer = () => ({
  customer_id: id, first_seen_at: t0, last_activity_at: "2026-09-27T01:00:00.000000Z", display_name: "Synthetic Buyer", phone_last3: "001",
  orders_count: 2, paid_orders_count: 1, captured_minor: 2500, refunded_minor: 500, currency: "TWD", claims_count: 1, platforms: ["manual"],
  consents: { marketing_messages: true, ads_personalization: false }, active: true,
});
const summary = {
  order_id: other, created_at: t0, updated_at: t0, currency: "TWD", total_minor: 2500, commercial_state: "CONFIRMED", fulfillment_state: "MANUAL_UNASSIGNED",
  payment_state: "CAPTURED", test_mode: true, work_state: "READY", refunded_minor: 0, refund_pending_minor: 0,
  pickup_source: null, payment_mode: "card", collection_state: null, // taiwan-cvs-logistics-v1 C4 keys of merchantorders.Summary
};
const detail = () => ({
  ...customer(),
  orders: [summary],
  claims: [{ session_id: other, platform: "manual", bound_at: t0, line_count: 2 }],
  consent_history: [{ purpose: "marketing_messages", channel: "meta_dm", granted: true, source: "buyer_checkout", policy_version: "lc-2026-10", occurred_at: t0 }],
  privacy_actions: [{ kind: "EXPORT", via: "merchant", completed_at: t0, summary: { orders: 1 } }],
});

test("customer list and detail decode exactly the frozen Go DTOs", () => {
  assert.equal(parseCustomer(customer()).customer_id, id);
  const list = parseCustomerList({ items: [customer()], next_cursor: "" });
  assert.equal(list.items.length, 1);
  assert.equal(parseCustomerList({ items: [], next_cursor: "" }).items.length, 0);
  assert.equal(parseCustomerList({ items: [customer()], next_cursor: "abc_DEF-1" }).next_cursor, "abc_DEF-1");
  const d = parseCustomerDetail(detail(), id);
  assert.equal(d.orders.length, 1);
  assert.equal(d.claims[0].line_count, 2);
  // nullable columns of a bundle-only customer
  assert.doesNotThrow(() => parseCustomer({ ...customer(), display_name: null, phone_last3: null, currency: null, orders_count: 0, paid_orders_count: 0, captured_minor: 0, refunded_minor: 0 }));
  assert.doesNotThrow(() => parseCustomer({ ...customer(), platforms: [] }));
  assert.doesNotThrow(() => parseCustomer({ ...customer(), platforms: ["facebook", "instagram", "manual"] }));
});

test("decoders refuse: unknown key, missing key, wrong type, impossible values, actor-level leakage", () => {
  const bad: [string, unknown][] = [
    ["unknown key", { ...customer(), actor_key: "a".repeat(64) }],
    ["a session id", { ...customer(), session_id: id }],
    ["missing key", (({ active, ...rest }) => rest)(customer())],
    ["id not canonical", { ...customer(), customer_id: withLetters.toUpperCase() }],
    ["instant not RFC3339 UTC", { ...customer(), first_seen_at: "2026-09-27 00:00:00" }],
    ["phone last 3 wrong", { ...customer(), phone_last3: "12" }],
    ["phone last 4", { ...customer(), phone_last3: "1234" }],
    ["negative count", { ...customer(), orders_count: -1 }],
    ["float money", { ...customer(), captured_minor: 2500.5 }],
    ["paid > orders", { ...customer(), paid_orders_count: 3 }],
    ["refunded > captured", { ...customer(), refunded_minor: 9999 }],
    ["currency lower", { ...customer(), currency: "twd" }],
    ["duplicate platform", { ...customer(), platforms: ["manual", "manual"] }],
    ["platforms not an array", { ...customer(), platforms: "manual" }],
    ["consents incomplete", { ...customer(), consents: { marketing_messages: true } }],
    ["consents extra key", { ...customer(), consents: { marketing_messages: true, ads_personalization: false, sms: true } }],
    ["consent not boolean", { ...customer(), consents: { marketing_messages: "yes", ads_personalization: false } }],
    ["an erased customer with a granted consent", { ...customer(), active: false }],
    ["name with a control char", { ...customer(), display_name: "a\u0000b" }],
    ["empty name", { ...customer(), display_name: "" }],
    ["null", null], ["array", []], ["string", "x"],
  ];
  for (const [name, value] of bad) assert.throws(() => parseCustomer(value), `${name}`);
  assert.doesNotThrow(() => parseCustomer({ ...customer(), active: false, consents: { marketing_messages: false, ads_personalization: false } }));
  assert.throws(() => parseCustomerList({ items: [customer()] }), "no next_cursor");
  assert.throws(() => parseCustomerList({ items: [customer()], next_cursor: "", extra: 1 }), "unknown page key");
  assert.throws(() => parseCustomerList({ items: null, next_cursor: "" }), "null items");
  assert.throws(() => parseCustomerList({ items: Array.from({ length: 101 }, customer), next_cursor: "" }), "more than 100 rows");
  assert.throws(() => parseCustomerDetail(detail(), other), "a detail for another customer than the one requested");
  assert.throws(() => parseCustomerDetail({ ...detail(), orders: [{ ...summary, extra: 1 }] }, id), "an order summary with an unknown key");
  assert.throws(() => parseCustomerDetail({ ...detail(), claims: [{ session_id: other, platform: "manual", bound_at: t0, line_count: 2, actor_key: "x" }] }, id), "an actor key in claims");
  assert.throws(() => parseCustomerDetail({ ...detail(), consent_history: [{ purpose: "marketing_messages", channel: "meta_dm", granted: true, source: "buyer_checkout", policy_version: "lc-2026-10" }] }, id), "consent event without occurred_at");
  assert.throws(() => parseCustomerDetail({ ...detail(), privacy_actions: [{ kind: "EXPORT", via: "merchant", completed_at: t0 }] }, id), "privacy action without summary");
});

test("finance and erasure summaries decode; anything else is refused", () => {
  const row = { day: "2026-09-27", currency: "TWD", environment: "SANDBOX", captured_count: 2, captured_minor: 5000, refunded_minor: 500, net_minor: 4500 };
  const ok = { from: "2026-09-01", to: "2026-09-30", timezone: "Asia/Taipei", rows: [row], totals: [{ ...row, day: "" }] };
  assert.equal(parseFinanceSummary(ok).rows.length, 1);
  assert.equal(parseFinanceSummary({ ...ok, rows: [], totals: [] }).rows.length, 0);
  assert.doesNotThrow(() => parseFinanceSummary({ ...ok, rows: [{ ...row, net_minor: -500, refunded_minor: 500, captured_minor: 0, captured_count: 0 }] }));
  for (const [name, value] of [
    ["unknown key", { ...ok, note: "x" }], ["rows null", { ...ok, rows: null }], ["timezone empty", { ...ok, timezone: "" }], ["date instant", { ...ok, from: t0 }],
    ["row extra key", { ...ok, rows: [{ ...row, cogs: 1 }] }], ["row float", { ...ok, rows: [{ ...row, captured_minor: 1.5 }] }],
  ] as [string, unknown][]) assert.throws(() => parseFinanceSummary(value), name);
  const erase = { consents_withdrawn: 2, sessions_revoked: 1, snapshots_redacted: 1, bundles_relabelled: 0 };
  assert.deepEqual(parseErasureSummary(erase), erase);
  for (const value of [{ ...erase, extra: 1 }, { consents_withdrawn: 2 }, { ...erase, sessions_revoked: -1 }, { ...erase, bundles_relabelled: "0" }, null, [], "x"])
    assert.throws(() => parseErasureSummary(value), JSON.stringify(value));
});

const sub = {
  status: "active", price_id: "price_A1", current_period_start: t0, current_period_end: "2026-10-27T00:00:00.000000Z", cancel_at_period_end: false, retrieved_at: t0,
};
const billing = () => ({
  standing: "GOOD", payment_pending: false, customer_pinned: true, subscriptions: [sub],
  usage: { period_start: "2026-09-01", period_end: "2026-10-01", paid_orders: 3, claim_windows_opened: 2, private_replies_sent: 1, members: 2 },
  plans: [{ price_id: "price_A1", name: "Pro Monthly", amount_minor: 30000, currency: "TWD", interval: "month" }],
  stale: false,
});

test("billing status, standing and redirect decode exactly the frozen DTOs (B3, BD4)", () => {
  assert.equal(parseBillingStatus(billing()).standing, "GOOD");
  for (const standing of ["GOOD", "GRACE", "RESTRICTED", "UNBILLED"]) assert.equal(parseStanding({ standing }), standing);
  for (const status of ["incomplete", "incomplete_expired", "trialing", "active", "past_due", "canceled", "unpaid", "paused"])
    assert.doesNotThrow(() => parseBillingStatus({ ...billing(), subscriptions: [{ ...sub, status }] }), status);
  assert.doesNotThrow(() => parseBillingStatus({ ...billing(), subscriptions: [{ ...sub, current_period_start: null, current_period_end: null }] }));
  assert.doesNotThrow(() => parseBillingStatus({ ...billing(), plans: [], subscriptions: [], stale: true }));
  for (const [name, value] of [
    ["unknown status", { ...billing(), subscriptions: [{ ...sub, status: "weird" }] }],
    ["unknown key", { ...billing(), invoices: [] }],
    ["a stripe id in a subscription", { ...billing(), subscriptions: [{ ...sub, stripe_subscription_id: "sub_1" }] }],
    ["a customer id", { ...billing(), stripe_customer_id: "cus_1" }],
    ["standing outside the four", { ...billing(), standing: "PAST_DUE" }],
    ["price id shape", { ...billing(), subscriptions: [{ ...sub, price_id: "plan_1" }] }],
    ["usage missing a count", { ...billing(), usage: { period_start: "2026-09-01", period_end: "2026-10-01", paid_orders: 3, claim_windows_opened: 2, private_replies_sent: 1 } }],
    ["usage negative", { ...billing(), usage: { ...billing().usage, members: -1 } }],
    ["11 plans", { ...billing(), plans: Array.from({ length: 11 }, (_, i) => ({ price_id: `price_${i}`, name: "n", amount_minor: 1, currency: "TWD", interval: "month" })) }],
    ["duplicate plan", { ...billing(), plans: [billing().plans[0], billing().plans[0]] }],
    ["plan currency lower", { ...billing(), plans: [{ ...billing().plans[0], currency: "twd" }] }],
    ["plan interval", { ...billing(), plans: [{ ...billing().plans[0], interval: "decade" }] }],
    ["stale not boolean", { ...billing(), stale: "no" }],
  ] as [string, unknown][]) assert.throws(() => parseBillingStatus(value), name);
  for (const value of [{ standing: "GOOD", extra: 1 }, {}, { standing: 1 }, null, []]) assert.throws(() => parseStanding(value), JSON.stringify(value));
});

test("a checkout/portal redirect is followed only to a hosted Stripe page (bearer links, I11)", () => {
  assert.equal(parseRedirect({ url: "https://checkout.stripe.com/c/pay/cs_test_a1#frag" }), "https://checkout.stripe.com/c/pay/cs_test_a1#frag");
  assert.equal(parseRedirect({ url: "https://billing.stripe.com/p/session/x" }), "https://billing.stripe.com/p/session/x");
  for (const u of [
    "http://checkout.stripe.com/c/pay/x", "https://evil.example/checkout.stripe.com", "https://checkout.stripe.com.evil.example/x", "https://user:pw@checkout.stripe.com/x",
    "https://checkout.stripe.com:8443/x", "javascript:alert(1)", "data:text/html,x", "//checkout.stripe.com/x", "/billing", "", "https://stripe.com/x", "https://a.checkout.stripe.com/x",
    "https://checkout.stripe.com/" + "a".repeat(2100),
  ]) assert.throws(() => parseRedirect({ url: u }), JSON.stringify(u.slice(0, 60)));
  for (const value of [{ url: "https://checkout.stripe.com/x", extra: 1 }, {}, { url: 1 }, { url: null }, null, "https://checkout.stripe.com/x"])
    assert.throws(() => parseRedirect(value), JSON.stringify(value));
});
