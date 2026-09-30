import test from "node:test";
import assert from "node:assert/strict";
import { handleBuyerRequest } from "../lib/buyer-server.ts";

const origin = "https://shop.example";
const api = "http://127.0.0.1:3219";
const orderID = "12345678-1234-1234-1234-123456789abc";
const path = `orders/${orderID}/payment`;
const expiry = "2026-09-25T01:02:03.123456789Z";
const key = "public-payment-key-1";
const view = {
  order_id: orderID, currency: "TWD", total_minor: 2500,
  commercial_state: "DRAFT", payment_state: "NOT_STARTED",
  handoff_state: "NONE", handoff_expires_at: null, test_mode: true,
  methods: [{ code: "payuni_credit", version: 1, name_hans: "测试", name_hant: "測試", name_en: "Mock" }],
};
const prepared = { order_id: orderID, state: "PAYMENT_PENDING", currency: "TWD", amount_minor: 2500 };
const handoff = {
  order_id: orderID, disposition: "ISSUED", expires_at: expiry,
  form: {
    action: "https://sandbox-api.payuni.com.tw/api/upp",
    fields: { Version: "2.0", MerID: "synthetic_merchant", EncryptInfo: "ab".repeat(8), HashInfo: "A".repeat(64) },
  },
};

function enabled() {
  process.env.COMMERCE_BUYER_WEB_ENABLED = "1";
  process.env.COMMERCE_BUYER_API_ORIGIN = api;
  process.env.COMMERCE_BUYER_BFF_KEY = Buffer.alloc(32, 1).toString("base64url");
  process.env.COMMERCE_BUYER_COOKIE_KEY = Buffer.alloc(32, 2).toString("base64url");
  process.env.COMMERCE_BUYER_SESSION_TTL = "3600";
}

function request(method, suffix, { cookie, context, body, idempotencyKey, extra = {}, signal } = {}) {
  const headers = new Headers({ Host: "shop.example" });
  if (method !== "GET") headers.set("Origin", origin);
  if (cookie) headers.set("Cookie", cookie);
  if (context) headers.set("X-Buyer-Context", context);
  if (body !== undefined) headers.set("Content-Type", "application/json");
  if (idempotencyKey !== undefined) headers.set("Idempotency-Key", idempotencyKey);
  for (const [name, value] of Object.entries(extra)) headers.set(name, value);
  return new Request(`${origin}/api/buyer/${suffix}`, { method, headers, body, signal });
}

async function session() {
  const response = await handleBuyerRequest(request("POST", "session/prepare", { body: "{}" }));
  assert.equal(response.status, 200);
  const cookie = response.headers.get("set-cookie")?.split(";", 1)[0];
  const { context } = await response.json();
  assert.match(cookie, /^__Host-commerce_buyer=/);
  assert.match(context, /^[A-Za-z0-9_-]{43}$/);
  return { cookie, context };
}

function payment(method, suffix, auth, input = {}) {
  return handleBuyerRequest(request(method, path + suffix, { ...auth, ...input }));
}

async function safeFailure(response, expectedStatus, nonretryable = true) {
  if (expectedStatus) assert.equal(response.status, expectedStatus);
  assert.ok(response.status >= 400);
  assert.equal(response.headers.get("cache-control"), "no-store");
  assert.equal(response.headers.get("x-content-type-options"), "nosniff");
  assert.equal(response.headers.get("set-cookie"), null);
  const raw = await response.text();
  assert.equal(raw.includes("upstream-secret-marker"), false);
  const envelope = JSON.parse(raw);
  assert.deepEqual(Object.keys(envelope).sort(), ["code", "details", "message", "request_id", "retryable"]);
  if (nonretryable) assert.equal(envelope.retryable, false);
  assert.match(envelope.request_id, /^[0-9a-f]{32}$/);
  assert.deepEqual(envelope.details, {});
  return envelope;
}

test("BPT01 exact private path, headers, body and three-locale one-fetch transport", async () => {
  enabled();
  const old = globalThis.fetch;
  try {
    for (const locale of ["zh-CN", "zh-TW", "en"]) {
      const auth = await session();
      const calls = [];
      globalThis.fetch = async (url, options) => {
        calls.push({ url: String(url), options });
        assert.equal(options.headers.get("X-Commerce-Buyer-BFF-Key"), process.env.COMMERCE_BUYER_BFF_KEY);
        assert.equal(options.headers.get("X-Commerce-Storefront-Origin"), origin);
        assert.match(options.headers.get("Authorization"), /^Bearer [A-Za-z0-9_-]{43}$/);
        assert.equal(options.redirect, "error");
        assert.equal(options.cache, "no-store");
        if (String(url).endsWith("/prepare")) return Response.json(prepared);
        if (String(url).endsWith("/handoff")) return Response.json(handoff);
        return Response.json(view);
      };
      const gotView = await payment("GET", "", auth);
      assert.equal(gotView.status, 200);
      assert.deepEqual(await gotView.json(), view);
      const gotPrepare = await payment("POST", "/prepare", auth, {
        body: JSON.stringify({ method_code: "payuni_credit", method_version: 1, locale }),
        idempotencyKey: key,
      });
      assert.equal(gotPrepare.status, 200);
      assert.deepEqual(await gotPrepare.json(), prepared);
      const gotHandoff = await payment("POST", "/handoff", auth);
      assert.equal(gotHandoff.status, 200);
      assert.deepEqual(await gotHandoff.json(), handoff);
      assert.deepEqual(calls.map(({ url }) => url), [
        `${api}/v1/buyer/${path}`, `${api}/v1/buyer/${path}/prepare`, `${api}/v1/buyer/${path}/handoff`,
      ]);
      assert.deepEqual(calls.map(({ options }) => options.method), ["GET", "POST", "POST"]);
      assert.equal(calls[0].options.headers.has("Idempotency-Key"), false);
      assert.equal(calls[1].options.headers.get("Idempotency-Key"), key);
      assert.equal(calls[2].options.headers.has("Idempotency-Key"), false);
      assert.equal(calls[2].options.body, undefined);
      assert.deepEqual(JSON.parse(calls[1].options.body), { method_code: "payuni_credit", method_version: 1, locale });
      assert.equal(gotHandoff.headers.get("set-cookie"), null);
      assert.equal(JSON.stringify(view).includes(calls[0].options.headers.get("Authorization").slice(7)), false);
    }
  } finally {
    globalThis.fetch = old;
    enabled();
  }
});

test("BPT02 payment routes reject untrusted path, context, headers, key and body before fetch", async () => {
  enabled();
  const auth = await session();
  const old = globalThis.fetch;
  let calls = 0;
  globalThis.fetch = () => { calls++; throw new Error("must not fetch"); };
  try {
    const input = { body: '{"method_code":"payuni_credit","method_version":1,"locale":"en"}', idempotencyKey: key };
    const cases = [
      request("GET", `${path}?x=1`, auth),
      request("GET", `${path}#fragment`, auth),
      request("POST", `${path}`, auth),
      request("POST", `${path}/prepare?x=1`, { ...auth, ...input }),
      request("POST", `${path}/handoff?x=1`, auth),
      request("POST", `orders/${orderID.toUpperCase()}/payment/handoff`, auth),
      request("GET", `orders/${orderID}/payment/extra`, auth),
      request("GET", `${path}`, { ...auth, idempotencyKey: key }),
      request("POST", `${path}/prepare`, { ...auth, ...input, idempotencyKey: undefined }),
      request("POST", `${path}/handoff`, { ...auth, body: "{}" }),
      request("POST", `${path}/handoff`, { ...auth, idempotencyKey: key }),
      request("POST", `${path}/handoff`, { cookie: auth.cookie, context: Buffer.alloc(32, 4).toString("base64url") }),
      request("POST", `${path}/handoff`, { context: auth.context }),
      request("POST", `${path}/handoff`, { cookie: auth.cookie + "x", context: auth.context }),
      request("POST", `${path}/handoff`, { ...auth, extra: { Origin: "https://evil.example" } }),
      request("POST", `${path}/handoff`, { ...auth, extra: { Authorization: "Bearer attacker" } }),
      request("POST", `${path}/handoff`, { ...auth, extra: { "X-Commerce-Buyer-BFF-Key": "attacker" } }),
      request("POST", `${path}/handoff`, { ...auth, extra: { "X-Store-ID": orderID } }),
      request("POST", `${path}/handoff`, { ...auth, extra: { "X-Tenant-ID": orderID } }),
      request("POST", `${path}/handoff`, { ...auth, extra: { "X-Commerce-Storefront-Origin": origin } }),
    ];
    for (const [index, candidate] of cases.entries()) {
      const rejected = await handleBuyerRequest(candidate);
      assert.ok(rejected.status >= 400, `case ${index}`);
      assert.equal((await rejected.json()).retryable, false, `case ${index}`);
      assert.equal(calls, 0, `case ${index}`);
    }
    for (const raw of [
      "null", "{}", '{"method_code":"payuni_credit","method_version":1,"locale":null}',
      '{"method_code":"payuni_credit","method_version":1,"locale":"en","locale":"zh-TW"}',
      '{"method_code":"payuni_credit","method_version":1,"locale":"en","extra":1}',
      '{"method_code":{"nested":1},"method_version":1,"locale":"en"}',
      '{"method_code":{"nested":1,"nested":2},"method_version":1,"locale":"en"}',
      '{"method_code":"payuni_atm","method_version":1,"locale":"en"}',
      '{"method_code":"payuni_credit","method_version":0,"locale":"en"}',
      '{"method_code":"payuni_credit","method_version":1,"locale":"fr"}',
    ]) {
      const rejected = await payment("POST", "/prepare", auth, { body: raw, idempotencyKey: key });
      assert.ok(rejected.status >= 400, raw);
      assert.equal(calls, 0, raw);
    }
    const wrongMime = await payment("POST", "/prepare", auth, { ...input, extra: { "Content-Type": "text/plain" } });
    assert.equal(wrongMime.status, 415);
    const invalidUTF8 = await payment("POST", "/prepare", auth, { body: Buffer.from([0xc3, 0x28]), idempotencyKey: key });
    assert.equal(invalidUTF8.status, 400);
    const oversized = await payment("POST", "/prepare", auth, { body: "x".repeat(65537), idempotencyKey: key });
    assert.equal(oversized.status, 422);
    assert.equal(calls, 0);
    const brokenInput = new Request(`${origin}/api/buyer/${path}/prepare`, {
      method: "POST",
      headers: {
        Host: "shop.example", Origin: origin, Cookie: auth.cookie,
        "X-Buyer-Context": auth.context, "Idempotency-Key": key,
        "Content-Type": "application/json",
      },
      body: new ReadableStream({ pull() { throw new Error("synthetic input read error"); } }),
      duplex: "half",
    });
    assert.equal((await handleBuyerRequest(brokenInput)).status, 503);
    assert.equal(calls, 0);
    const brokenHandoff = new Request(`${origin}/api/buyer/${path}/handoff`, {
      method: "POST",
      headers: { Host: "shop.example", Origin: origin, Cookie: auth.cookie, "X-Buyer-Context": auth.context },
      body: new ReadableStream({ pull() { throw new Error("synthetic no-body read error"); } }),
      duplex: "half",
    });
    await safeFailure(await handleBuyerRequest(brokenHandoff), 503);
    assert.equal(calls, 0);
    const duplicateCookie = request("POST", `${path}/handoff`, auth);
    duplicateCookie.headers.append("Cookie", auth.cookie);
    const duplicateKey = request("POST", `${path}/prepare`, { ...auth, ...input });
    duplicateKey.headers.append("Idempotency-Key", key);
    for (const candidate of [duplicateCookie, duplicateKey]) {
      const rejected = await handleBuyerRequest(candidate);
      assert.ok(rejected.status >= 400);
      assert.equal(calls, 0);
    }
    const clock = Date.now;
    Date.now = () => clock() + 3601_000;
    try {
      const expired = await payment("POST", "/handoff", auth);
      assert.equal(expired.status, 401);
      assert.equal((await expired.json()).retryable, false);
      assert.equal(calls, 0);
    } finally {
      Date.now = clock;
    }
  } finally {
    globalThis.fetch = old;
  }
});

test("BPT03 hostile upstream success is sanitized, never returned as payment data", async () => {
  enabled();
  const auth = await session();
  const old = globalThis.fetch;
  try {
    const bodies = [
      ["", { ...view, attempt_id: orderID }],
      ["", { ...view, order_id: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa" }],
      ["", { ...view, payment_state: "PAID" }],
      ["", { ...view, total_minor: -1 }],
      ["", { ...view, handoff_state: "PREPARED", handoff_expires_at: "2026-02-30T00:00:00Z" }],
      ["/prepare", { ...prepared, amount_minor: 0 }],
      ["/prepare", { ...prepared, currency: "USD" }],
      ["/prepare", { ...prepared, amount_minor: 2501 }],
      ["/handoff", { ...handoff, form: { ...handoff.form, action: "https://evil.example/api/upp" } }],
      ["/handoff", { ...handoff, form: { ...handoff.form, fields: { ...handoff.form.fields, Credential: "upstream-secret-marker" } } }],
      ["/handoff", { ...handoff, disposition: "ALREADY_ISSUED" }],
    ];
    for (const [suffix, body] of bodies) {
      globalThis.fetch = async () => Response.json(body);
      const input = suffix === "/prepare"
        ? { body: '{"method_code":"payuni_credit","method_version":1,"locale":"en"}', idempotencyKey: key }
        : {};
      await safeFailure(await payment(suffix === "" ? "GET" : "POST", suffix, auth, input), 503, suffix === "/handoff");
    }
    const duplicate = JSON.stringify(handoff).replace('"Version":"2.0"', '"Version":"2.0","Version":"2.0"');
    for (const response of [
      new Response(duplicate, { status: 200, headers: { "Content-Type": "application/json" } }),
      new Response(Buffer.from([0xc3, 0x28]), { status: 200, headers: { "Content-Type": "application/json" } }),
      new Response(JSON.stringify(handoff), { status: 200, headers: { "Content-Type": "text/html" } }),
      new Response(JSON.stringify(handoff), { status: 200, headers: { "Content-Type": "application/json; charset=iso-8859-1" } }),
      new Response(JSON.stringify(handoff), { status: 200, headers: { "Content-Type": "application/json", "Content-Encoding": "gzip" } }),
      new Response(JSON.stringify(handoff), { status: 200, headers: { "Content-Type": "application/json", "Content-Length": "1048577" } }),
      new Response("x".repeat(1048577), { status: 200, headers: { "Content-Type": "application/json" } }),
      new Response(new ReadableStream({ pull() { throw new Error("upstream-secret-marker"); } }), { status: 200, headers: { "Content-Type": "application/json" } }),
      new Response(JSON.stringify(handoff), { status: 201, headers: { "Content-Type": "application/json" } }),
      new Response(null, { status: 204 }),
    ]) {
      globalThis.fetch = async () => response;
      await safeFailure(await payment("POST", "/handoff", auth), 503);
    }
  } finally {
    globalThis.fetch = old;
  }
});

test("BPT04 every handoff failure is nonretryable, including pre-route and transport loss", async () => {
  enabled();
  const auth = await session();
  const old = globalThis.fetch;
  const oldTimeout = AbortSignal.timeout;
  let calls = 0;
  try {
    globalThis.fetch = () => { calls++; throw new Error("upstream-secret-marker"); };
    await safeFailure(await payment("POST", "/handoff", auth), 503);
    assert.equal(calls, 1);
    for (const [status, code] of [[429, "rate_limited"], [503, "unavailable"]]) {
      globalThis.fetch = async () => {
        calls++;
        return Response.json({ code, message: "upstream-secret-marker", retryable: true }, { status });
      };
      await safeFailure(await payment("POST", "/handoff", auth), status);
    }
    assert.equal(calls, 3);
    globalThis.fetch = async (_url, options) => {
      calls++;
      assert.equal(options.signal.aborted, true);
      throw new Error("deadline");
    };
    AbortSignal.timeout = () => AbortSignal.abort();
    await safeFailure(await payment("POST", "/handoff", auth), 503);
    assert.equal(calls, 4);
    AbortSignal.timeout = oldTimeout;
    const abort = new AbortController();
    abort.abort();
    await safeFailure(await payment("POST", "/handoff", auth, { signal: abort.signal }), 503);
    assert.equal(calls, 4);
    process.env.COMMERCE_BUYER_WEB_ENABLED = "0";
    await safeFailure(await payment("POST", "/handoff", auth), 404);
    enabled();
    process.env.COMMERCE_BUYER_COOKIE_KEY = "broken";
    await safeFailure(await payment("POST", "/handoff", auth), 503);
    enabled();
    for (const candidate of [
      request("GET", `${path}/handoff`, auth),
      request("POST", `${path}/handoff?x=1`, auth),
      request("POST", `orders/${orderID.toUpperCase()}/payment/handoff`, auth),
    ]) await safeFailure(await handleBuyerRequest(candidate));
    assert.equal(calls, 4);
  } finally {
    globalThis.fetch = old;
    AbortSignal.timeout = oldTimeout;
    enabled();
  }
});

// ---- SU02 (author-side): Stripe routes, stripe-buyer-ui-v1 §2.
const stripeView = { ...view, methods: [{ ...view.methods[0], code: "stripe_checkout" }] };
const checkoutURL = "https://checkout.stripe.com/c/pay/cs_test_a1B2";
const redirect = { order_id: orderID, disposition: "REDIRECT", expires_at: expiry, redirect_url: checkoutURL };
const signal = { order_id: orderID, scheduled: true };
const stripePrepare = JSON.stringify({ method_code: "stripe_checkout", method_version: 1, locale: "en" });

test("SU02 five routes: exact upstream path, method, key, body, one fetch each", async () => {
  enabled();
  const old = globalThis.fetch;
  try {
    const auth = await session();
    const calls = [];
    globalThis.fetch = async (url, options) => {
      calls.push({ url: String(url), options });
      const u = String(url);
      if (u.endsWith("/prepare")) return Response.json({ ...prepared, currency: "USD", amount_minor: 2500 });
      if (u.endsWith("/handoff")) return Response.json(redirect);
      if (u.endsWith("/refresh") || u.endsWith("/cancel")) return Response.json(signal);
      return Response.json(stripeView);
    };
    assert.deepEqual(await (await payment("GET", "", auth)).json(), stripeView);
    const prep = await payment("POST", "/prepare", auth, { body: stripePrepare, idempotencyKey: key });
    assert.equal(prep.status, 200);
    for (const suffix of ["/handoff", "/refresh", "/cancel"]) {
      const response = await payment("POST", suffix, auth);
      assert.equal(response.status, 200, suffix);
      assert.equal(response.headers.get("cache-control"), "no-store");
      assert.equal(response.headers.get("set-cookie"), null);
    }
    assert.deepEqual(calls.map(({ url }) => url.replace(`${api}/v1/buyer/${path}`, "")), ["", "/prepare", "/handoff", "/refresh", "/cancel"]);
    assert.deepEqual(calls.map(({ options }) => options.method), ["GET", "POST", "POST", "POST", "POST"]);
    assert.equal(calls[1].options.headers.get("Idempotency-Key"), key);
    assert.equal(calls[1].options.body, stripePrepare);
    for (const index of [0, 2, 3, 4]) {
      assert.equal(calls[index].options.headers.has("Idempotency-Key"), false);
      assert.equal(calls[index].options.body, undefined);
      assert.equal(calls[index].options.redirect, "error");
    }
  } finally {
    globalThis.fetch = old;
    enabled();
  }
});

test("SU02 refresh and cancel are keyless and pass every CSRF/context denial before fetch", async () => {
  enabled();
  const auth = await session();
  const old = globalThis.fetch;
  let calls = 0;
  globalThis.fetch = () => { calls++; throw new Error("must not fetch"); };
  try {
    for (const suffix of ["refresh", "cancel"]) {
      const at = `${path}/${suffix}`;
      for (const [index, candidate] of [
        request("POST", at, { ...auth, idempotencyKey: key }),
        request("POST", at, { ...auth, body: "{}" }),
        request("GET", at, auth),
        request("POST", `${at}?x=1`, auth),
        request("POST", `${at}/extra`, auth),
        request("POST", `orders/${orderID.toUpperCase()}/payment/${suffix}`, auth),
        request("POST", at, { context: auth.context }),
        request("POST", at, { cookie: auth.cookie, context: Buffer.alloc(32, 4).toString("base64url") }),
        request("POST", at, { ...auth, extra: { Origin: "https://evil.example" } }),
        request("POST", at, { ...auth, extra: { Authorization: "Bearer attacker" } }),
        request("POST", at, { ...auth, extra: { "X-Tenant-ID": orderID } }),
      ].entries()) {
        const rejected = await handleBuyerRequest(candidate);
        assert.ok(rejected.status >= 400, `${suffix} ${index}`);
        assert.equal((await rejected.json()).retryable, false, `${suffix} ${index}`);
      }
    }
    assert.equal(calls, 0);
  } finally {
    globalThis.fetch = old;
  }
});

test("SU02 every handoff/refresh/cancel failure is nonretryable and sanitized", async () => {
  enabled();
  const auth = await session();
  const old = globalThis.fetch;
  try {
    for (const suffix of ["/handoff", "/refresh", "/cancel"]) {
      globalThis.fetch = () => { throw new Error("upstream-secret-marker"); };
      await safeFailure(await payment("POST", suffix, auth), 503);
      for (const [status, code] of [[429, "rate_limited"], [503, "unavailable"], [404, "not_found"], [409, "conflict"]]) {
        globalThis.fetch = async () => Response.json({ code, message: "upstream-secret-marker", retryable: true }, { status });
        await safeFailure(await payment("POST", suffix, auth), status);
      }
      globalThis.fetch = async () => new Response("<html>upstream-secret-marker", { status: 502 });
      await safeFailure(await payment("POST", suffix, auth), 503);
    }
  } finally {
    globalThis.fetch = old;
  }
});

test("SU02 hostile Stripe bodies become the sanitized 503; PAYUNi and Stripe bodies stay byte-identical", async () => {
  enabled();
  const auth = await session();
  const old = globalThis.fetch;
  try {
    const stripePrep = { body: stripePrepare, idempotencyKey: key };
    const cases = [
      ["", "GET", {}, { ...stripeView, methods: [stripeView.methods[0], stripeView.methods[0]] }],
      ["", "GET", {}, { ...view, handoff_state: "CREATING", handoff_expires_at: expiry }],
      ["", "GET", {}, { ...view, cancel_requested: "no", methods: [] }],
      ["/prepare", "POST", stripePrep, { ...prepared, currency: "EUR", amount_minor: 2500 }],
      ["/prepare", "POST", stripePrep, { ...prepared, currency: "USD", amount_minor: 49 }],
      ["/prepare", "POST", stripePrep, { ...prepared, currency: "TWD", amount_minor: 2550 }],
      ["/prepare", "POST", { body: '{"method_code":"payuni_credit","method_version":1,"locale":"en"}', idempotencyKey: key }, { ...prepared, currency: "USD" }],
      ["/handoff", "POST", {}, { ...redirect, redirect_url: "https://checkout.stripe.com.evil.example/x" }],
      ["/handoff", "POST", {}, { ...redirect, redirect_url: undefined }],
      ["/handoff", "POST", {}, { ...redirect, disposition: "CREATING" }],
      ["/handoff", "POST", {}, { ...redirect, form: handoff.form }],
      ["/refresh", "POST", {}, { order_id: orderID }],
      ["/refresh", "POST", {}, { ...signal, scheduled: "yes" }],
      ["/cancel", "POST", {}, { ...signal, extra: 1 }],
      ["/cancel", "POST", {}, { ...signal, order_id: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa" }],
    ];
    for (const [suffix, method, input, body] of cases) {
      globalThis.fetch = async () => Response.json(body);
      await safeFailure(await payment(method, suffix, auth, input), 503, suffix !== "" && suffix !== "/prepare");
    }
    // Valid bodies pass through byte-for-byte for both providers.
    const same = [
      ["", "GET", {}, stripeView],
      ["", "GET", {}, { ...view, cancel_requested: false, methods: [], payment_state: "PENDING", commercial_state: "AWAITING_PAYMENT", handoff_state: "READY", handoff_expires_at: expiry }],
      ["/prepare", "POST", { body: stripePrepare, idempotencyKey: key }, { ...prepared, currency: "HKD", amount_minor: 400 }],
      ["/prepare", "POST", { body: '{"method_code":"payuni_credit","method_version":1,"locale":"en"}', idempotencyKey: key }, prepared],
      ["/handoff", "POST", {}, redirect],
      ["/handoff", "POST", {}, { order_id: orderID, disposition: "UNAVAILABLE", expires_at: expiry }],
      ["/handoff", "POST", {}, handoff],
      ["/cancel", "POST", {}, { ...signal, scheduled: false }],
    ];
    for (const [suffix, method, input, body] of same) {
      const raw = JSON.stringify(body);
      globalThis.fetch = async () => new Response(raw, { status: 200, headers: { "Content-Type": "application/json" } });
      const got = await payment(method, suffix, auth, input);
      assert.equal(got.status, 200, suffix);
      assert.equal(await got.text(), raw, suffix);
    }
  } finally {
    globalThis.fetch = old;
  }
});

test("SU02 prepare accepts only the two known method codes", async () => {
  enabled();
  const auth = await session();
  const old = globalThis.fetch;
  let calls = 0;
  globalThis.fetch = () => { calls++; throw new Error("must not fetch"); };
  try {
    for (const code of ["stripe", "stripe_card", "Stripe_Checkout", ""]) {
      const rejected = await payment("POST", "/prepare", auth, {
        body: JSON.stringify({ method_code: code, method_version: 1, locale: "en" }), idempotencyKey: key,
      });
      assert.ok(rejected.status >= 400, code);
    }
    assert.equal(calls, 0);
  } finally {
    globalThis.fetch = old;
  }
});
