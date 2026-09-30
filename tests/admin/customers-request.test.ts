// customers-billing-ui: BFF grammar for customers/finance/billing (apps/admin/lib/customers-request.ts).
// Pure fence tests; the Go handlers and the BFF route.ts wiring are proved by customers-billing-tests (CB09).
import assert from "node:assert/strict";
import { test } from "node:test";
import {
  customersRoute,
  validCustomersBody,
  validCustomersQuery,
  validCustomersRequest,
} from "../../apps/admin/lib/customers-request.ts";

const id = "11111111-1111-4111-8111-111111111111";
const base = "http://127.0.0.1:3100/api/stores/" + id + "/";
const key = "customer-key-0001";

test("route table admits exactly the eleven contract resources", () => {
  const admitted: [string, string, string][] = [
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
  for (const [method, path, kind] of admitted) assert.equal(customersRoute(method, path), kind, path);
  for (const [method, path] of [
    ["POST", "customers"],
    ["PUT", `customers/${id}`],
    ["DELETE", `customers/${id}`],
    ["GET", `customers/${id}/exports`],
    ["GET", "customers/not-a-uuid"],
    ["GET", "customers/11111111-1111-4111-8111-11111111111A"],
    ["GET", "finance"],
    ["GET", "finance/summary.json"],
    ["GET", "billing/checkout"],
    ["POST", "billing"],
    ["POST", "billing/standing"],
    ["GET", `customers/${id}/erasure`],
    ["GET", "customers/"],
  ])
    assert.equal(customersRoute(method, path), null, `${method} ${path}`);
});

test("list query: limit/after/q grammar, no duplicates, no unknown keys", () => {
  const url = base + "customers";
  for (const query of ["", "?limit=1", "?limit=100&after=A_-", "?q=%E7%8E%8B", "?q=0912%20345&limit=50&after=abc"])
    assert.equal(validCustomersQuery("list", url + query), true, query);
  const bad = [
    "?", "?limit=0", "?limit=101", "?limit=01", "?limit=x", "?limit=1&limit=2", "?after=", "?after=a%20b",
    `?after=${"a".repeat(1025)}`, "?q=", "?q=%20%20", "?q=a+b", `?q=${"a".repeat(41)}`, "?q=%00", "?q=%E0%A4%A",
    "?from=2026-01-01", "?state=all", "?limit=1&", "?limit",
  ];
  for (const query of bad) assert.equal(validCustomersQuery("list", url + query), false, query);
  // 40 code points including astral characters is the boundary.
  assert.equal(validCustomersQuery("list", `${url}?q=${encodeURIComponent("😀".repeat(40))}`), true);
  assert.equal(validCustomersQuery("list", `${url}?q=${encodeURIComponent("😀".repeat(41))}`), false);
});

test("finance query: both dates required, real calendar days, 0..91 day range", () => {
  const url = base + "finance/summary";
  for (const kind of ["finance", "finance-csv"] as const) {
    assert.equal(validCustomersQuery(kind, `${url}?from=2026-09-01&to=2026-09-30`), true);
    assert.equal(validCustomersQuery(kind, `${url}?to=2026-09-30&from=2026-09-30`), true);
    assert.equal(validCustomersQuery(kind, `${url}?from=2026-01-01&to=2026-04-02`), true); // 91 days
    for (const query of [
      "", "?from=2026-09-01", "?to=2026-09-30", "?from=2026-01-01&to=2026-04-03", // 92 days
      "?from=2026-09-30&to=2026-09-01", "?from=2026-02-30&to=2026-03-01", "?from=2026-9-1&to=2026-09-30",
      "?from=2026-09-01&to=2026-09-30&limit=1", "?from=2026-09-01&from=2026-09-01&to=2026-09-02",
    ])
      assert.equal(validCustomersQuery(kind, url + query), false, query);
  }
});

test("exact resources refuse any query, even a bare question mark", () => {
  for (const kind of ["detail", "withdraw", "export", "erase", "billing", "standing", "checkout", "portal"] as const) {
    assert.equal(validCustomersQuery(kind, base + "x"), true);
    assert.equal(validCustomersQuery(kind, base + "x?"), false, kind);
    assert.equal(validCustomersQuery(kind, base + "x?a=b"), false, kind);
  }
});

test("key rules: required on the three customer POSTs, forbidden everywhere else", () => {
  const post = (path: string, headers: Record<string, string>) =>
    new Request(base + path, { method: "POST", headers, body: headers["Content-Type"] ? "{}" : null });
  const json = { "Content-Type": "application/json" };
  assert.equal(validCustomersRequest("withdraw", post("x", { ...json, "Idempotency-Key": key })), true);
  assert.equal(validCustomersRequest("erase", post("x", { ...json, "Idempotency-Key": key })), true);
  assert.equal(validCustomersRequest("withdraw", post("x", json)), false, "missing key");
  assert.equal(validCustomersRequest("withdraw", post("x", { ...json, "Idempotency-Key": "short" })), false);
  assert.equal(validCustomersRequest("withdraw", post("x", { ...json, "Idempotency-Key": "bad key with spaces" })), false);
  assert.equal(validCustomersRequest("withdraw", post("x", { "Idempotency-Key": key })), false, "json required");
  assert.equal(validCustomersRequest("export", post("x", { "Idempotency-Key": key })), true);
  assert.equal(validCustomersRequest("export", post("x", {})), false, "export needs a key");
  // billing POSTs: keyless
  assert.equal(validCustomersRequest("checkout", post("x", json)), true);
  assert.equal(validCustomersRequest("checkout", post("x", { ...json, "Idempotency-Key": key })), false);
  assert.equal(validCustomersRequest("portal", post("x", {})), true);
  assert.equal(validCustomersRequest("portal", post("x", { "Idempotency-Key": key })), false);
  // GET: no key, no body
  const get = (headers: Record<string, string> = {}) => new Request(base + "customers", { method: "GET", headers });
  assert.equal(validCustomersRequest("list", get()), true);
  assert.equal(validCustomersRequest("list", get({ "Idempotency-Key": key })), false);
  assert.equal(validCustomersRequest("list", get({ "Content-Length": "5" })), false);
  assert.equal(validCustomersRequest("list", get({ "Transfer-Encoding": "chunked" })), false);
});

test("export and portal must declare an empty body", () => {
  const request = (headers: Record<string, string>) => new Request(base + "x", { method: "POST", headers });
  assert.equal(validCustomersRequest("portal", request({ "Content-Length": "0" })), true);
  assert.equal(validCustomersRequest("portal", request({ "Content-Length": "2" })), false);
  assert.equal(validCustomersRequest("export", request({ "Content-Length": "2", "Idempotency-Key": key })), false);
});

test("bodies are exact and closed", () => {
  assert.equal(validCustomersBody("erase", '{"confirm":"ERASE"}'), true);
  for (const text of ['{"confirm":"erase"}', '{"confirm":"ERASE","x":1}', "{}", "[]", "null", "", '"ERASE"'])
    assert.equal(validCustomersBody("erase", text), false, text);
  assert.equal(validCustomersBody("withdraw", '{"purpose":"marketing_messages","channel":"meta_dm"}'), true);
  assert.equal(validCustomersBody("withdraw", '{"channel":"meta_ads","purpose":"ads_personalization"}'), true);
  for (const text of [
    '{"purpose":"marketing_messages","channel":"meta_ads"}',
    '{"purpose":"ads_personalization","channel":"meta_dm"}',
    '{"purpose":"marketing_messages"}',
    '{"purpose":"marketing_messages","channel":"meta_dm","granted":false}',
  ])
    assert.equal(validCustomersBody("withdraw", text), false, text);
  assert.equal(validCustomersBody("checkout", '{"price_id":"price_1Abc"}'), true);
  for (const text of ['{"price_id":"prod_1"}', '{"price_id":"price_1","x":1}', "{}", '{"price_id":1}'])
    assert.equal(validCustomersBody("checkout", text), false, text);
  assert.equal(validCustomersBody("export", ""), true);
  assert.equal(validCustomersBody("export", "{}"), false);
  assert.equal(validCustomersBody("portal", ""), true);
});
