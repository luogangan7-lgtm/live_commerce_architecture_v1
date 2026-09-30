import test from "node:test";
import assert from "node:assert/strict";
import { handleBuyerRequest } from "../lib/buyer-server.ts";

const bff = Buffer.alloc(32, 1).toString("base64url");
const signing = Buffer.alloc(32, 2).toString("base64url");
const origin = "https://shop.example";
const api = "http://127.0.0.1:3219";

function enabled() {
  process.env.COMMERCE_BUYER_WEB_ENABLED = "1";
  process.env.COMMERCE_BUYER_API_ORIGIN = api;
  process.env.COMMERCE_BUYER_BFF_KEY = bff;
  process.env.COMMERCE_BUYER_COOKIE_KEY = signing;
  process.env.COMMERCE_BUYER_SESSION_TTL = "3600";
}

function req(method, suffix, { cookie, context, body, extra = {} } = {}) {
  const headers = { Host: "shop.example", ...extra };
  if (method !== "GET") headers.Origin = origin;
  if (cookie) headers.Cookie = cookie;
  if (context) headers["X-Buyer-Context"] = context;
  if (body !== undefined) headers["Content-Type"] = "application/json";
  return new Request(`${origin}/api/buyer/${suffix}`, {
    method,
    headers,
    body,
  });
}

function cookie(response) {
  const set = response.headers.get("set-cookie");
  assert.match(set ?? "", /^__Host-commerce_buyer=/);
  assert.match(set, /Path=\/; Max-Age=3600; Secure; HttpOnly; SameSite=Lax/);
  return set.split(";", 1)[0];
}

test("disabled and invalid configuration fail closed before private fetch", async () => {
  const old = globalThis.fetch;
  globalThis.fetch = () => {
    throw new Error("must not fetch");
  };
  try {
    process.env.COMMERCE_BUYER_WEB_ENABLED = "0";
    let response = await handleBuyerRequest(req("GET", "session"));
    assert.equal(response.status, 404);
    process.env.COMMERCE_BUYER_WEB_ENABLED = "1";
    process.env.COMMERCE_BUYER_COOKIE_KEY = bff;
    response = await handleBuyerRequest(
      req("POST", "session/prepare", { body: "{}" }),
    );
    assert.equal(response.status, 503);
    assert.equal((await response.json()).retryable, false);
    assert.equal(response.headers.get("set-cookie"), null);
  } finally {
    globalThis.fetch = old;
    enabled();
  }
});

test("private authority, cookie lifecycle and strict local denial", async () => {
  enabled();
  const old = globalThis.fetch;
  const calls = [];
  globalThis.fetch = async (url, options) => {
    calls.push({ url, options });
    assert.ok(String(url).startsWith(`${api}/v1/buyer/`));
    assert.equal(options.headers.get("X-Commerce-Storefront-Origin"), origin);
    assert.equal(options.headers.get("X-Commerce-Buyer-BFF-Key"), bff);
    assert.equal(options.redirect, "error");
    if (String(url).endsWith("/session/retire"))
      return new Response(null, { status: 204 });
    if (String(url).endsWith("/session/bootstrap"))
      return Response.json({
        authenticated: true,
        expires_at: new Date(Date.now() + 3500_000).toISOString(),
      });
    if (String(url).endsWith("/session"))
      return Response.json({ authenticated: true });
    if (String(url).endsWith("/destination"))
      return Response.json({ destination: null });
    return Response.json({
      id: "cart",
      currency: "TWD",
      version: 1,
      items: [],
    });
  };
  try {
    let response = await handleBuyerRequest(req("GET", "session"));
    assert.deepEqual(await response.json(), {
      state: "absent",
      context: null,
      expires_at: null,
    });
    assert.equal(calls.length, 0);

    for (const body of ['{"__proto__":{}}', '{"constructor":{}}']) {
      const rejected = await handleBuyerRequest(
        req("POST", "session/prepare", { body }),
      );
      assert.equal(rejected.status, 400);
      assert.equal((await rejected.json()).retryable, false);
      assert.equal(rejected.headers.get("set-cookie"), null);
      assert.equal(calls.length, 0);
    }

    response = await handleBuyerRequest(
      req("POST", "session/prepare", { body: "{}" }),
    );
    assert.equal(response.status, 200);
    const firstCookie = cookie(response);
    const prepared = await response.json();
    assert.equal(prepared.state, "inactive");
    assert.match(prepared.context, /^[A-Za-z0-9_-]{43}$/);
    assert.equal(JSON.stringify(prepared).includes("token"), false);
    assert.equal(calls.length, 0);

    const stale = await handleBuyerRequest(
      req("PUT", "cart", {
        cookie: firstCookie,
        context: bff,
        body: '{"items":[]}',
        extra: { "Idempotency-Key": "validkey1" },
      }),
    );
    assert.equal(stale.status, 409);
    assert.equal((await stale.json()).code, "context_changed");
    assert.equal(calls.length, 0);

    for (const body of [
      '{"items":null}',
      '{"items":[],"items":[]}',
      '{"items":[{"sku_id":"x","quantity":1,"extra":1}]}',
      '{"items":[{"constructor":{}}]}',
      '{"items":[{"__proto__":{}}]}',
    ]) {
      const bad = await handleBuyerRequest(
        req("PUT", "cart", {
          cookie: firstCookie,
          context: prepared.context,
          body,
          extra: { "Idempotency-Key": "validkey1" },
        }),
      );
      assert.equal(bad.status, 400);
      assert.equal(calls.length, 0);
    }
    response = await handleBuyerRequest(
      req("POST", "session/activate", {
        cookie: firstCookie,
        context: prepared.context,
        body: "{}",
      }),
    );
    assert.equal(response.status, 200);
    assert.equal((await response.json()).state, "active");
    assert.equal(response.headers.get("set-cookie"), null);

    response = await handleBuyerRequest(
      req("GET", "cart", { cookie: firstCookie, context: prepared.context }),
    );
    assert.equal(response.status, 200);
    assert.deepEqual(await response.json(), {
      id: "cart",
      currency: "TWD",
      version: 1,
      items: [],
    });
    assert.equal(response.headers.get("set-cookie"), null);

    response = await handleBuyerRequest(
      req("GET", "destination", {
        cookie: firstCookie,
        context: prepared.context,
      }),
    );
    assert.equal(response.status, 200);
    assert.deepEqual(await response.json(), { destination: null });
    assert.equal(response.headers.get("cache-control"), "no-store");
    assert.equal(response.headers.get("set-cookie"), null);
    const countBefore = calls.length;
    for (const suffix of ["destination?owner_id=x", "destination?"]) {
      const denied = await handleBuyerRequest(
        req("GET", suffix, {
          cookie: firstCookie,
          context: prepared.context,
        }),
      );
      assert.equal(denied.status, 422);
      assert.equal(calls.length, countBefore);
    }

    response = await handleBuyerRequest(
      req("POST", "session/reset", {
        cookie: firstCookie,
        context: prepared.context,
        body: "{}",
      }),
    );
    assert.equal(response.status, 200);
    const secondCookie = cookie(response);
    assert.notEqual(secondCookie, firstCookie);
    assert.equal((await response.json()).state, "inactive");
    assert.equal(
      calls.filter((call) => String(call.url).endsWith("/session/retire"))
        .length,
      1,
    );
  } finally {
    globalThis.fetch = old;
  }
});

test("bad host, cookie, query and method never reach private API", async () => {
  enabled();
  const old = globalThis.fetch;
  globalThis.fetch = () => {
    throw new Error("must not fetch");
  };
  try {
    for (const host of [
      "SHOP.example",
      "shop.example:443",
      "127.0.0.1",
      "shop.example,other.example",
      "shop.example.",
    ]) {
      const response = await handleBuyerRequest(
        req("POST", "session/prepare", { body: "{}", extra: { Host: host } }),
      );
      assert.equal(response.status, 403, host);
      assert.equal(response.headers.get("set-cookie"), null);
    }
    let response = await handleBuyerRequest(
      req("POST", "session/prepare", {
        body: "{}",
        extra: { Authorization: `Bearer ${bff}` },
      }),
    );
    assert.equal(response.status, 403);
    response = await handleBuyerRequest(
      req("GET", "session", { cookie: "__Host-commerce_buyer=bad" }),
    );
    assert.equal(response.status, 401);
    response = await handleBuyerRequest(
      req("GET", "session", {
        cookie: "__Host-commerce_buyer=bad; __Host-commerce_buyer=bad",
      }),
    );
    assert.equal(response.status, 401);
    response = await handleBuyerRequest(req("GET", "session?"));
    assert.equal(response.status, 422);
    response = await handleBuyerRequest(req("GET", "catalog?limit=1&limit=2"));
    assert.equal(response.status, 422);
    response = await handleBuyerRequest(req("HEAD", "session"));
    assert.equal(response.status, 405);
    response = await handleBuyerRequest(req("GET", "quotes/not-a-uuid"));
    assert.equal(response.status, 422);
    for (const path of [
      "orders?owner_id=other",
      "orders?country=TW",
      "orders?limit=101",
      "orders?limit=1&limit=2",
      "orders?cursor=bad%2Bcursor",
    ])
      assert.equal((await handleBuyerRequest(req("GET", path))).status, 422);
    assert.equal(
      (await handleBuyerRequest(req("POST", "orders", { body: "{}" }))).status,
      405,
    );
  } finally {
    globalThis.fetch = old;
  }
});

test("stalled inbound POST body is canceled at its deadline without a cookie or private call", async (t) => {
  enabled();
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const old = globalThis.fetch;
  globalThis.fetch = () => {
    throw new Error("must not fetch");
  };
  let canceled = false;
  const stalled = new ReadableStream({
    pull() {
      return new Promise(() => {});
    },
    cancel() {
      canceled = true;
    },
  });
  try {
    const request = new Request(`${origin}/api/buyer/session/prepare`, {
      method: "POST",
      duplex: "half",
      body: stalled,
      headers: {
        Host: "shop.example",
        Origin: origin,
        "Content-Type": "application/json",
      },
    });
    const pending = handleBuyerRequest(request);
    t.mock.timers.tick(12_000);
    const response = await pending;
    assert.equal(response.status, 503);
    assert.equal((await response.json()).retryable, false);
    assert.equal(response.headers.get("set-cookie"), null);
    assert.equal(canceled, true);
  } finally {
    globalThis.fetch = old;
    t.mock.timers.reset();
  }
});

// ---- CVS routes (taiwan-cvs-logistics-v1 §5.2, §16.1): exact grammar, strict bodies, validated answers ----
const cid = (n) => `00000000-0000-0000-0000-${String(n).padStart(12, "0")}`;
const mapFormBody = () => ({
  action: "https://logistics-stage.ecpay.com.tw/Express/map",
  fields: {
    MerchantID: "2000132", MerchantTradeNo: "AbCdEfGhIjKlMnOpQrSt", LogisticsType: "CVS", LogisticsSubType: "UNIMARTC2C",
    IsCollection: "N", ServerReplyURL: `https://hooks.example.test/v1/cvs/ecpay/map-return/${cid(1)}`, Device: "0",
  },
});
const selectionBody = { cart_version: 2, market_id: cid(4), service_code: "cvs-711", return_path: "/zh-TW/products/abc" };
const storeBody = {
  cart_version: 2, market_id: cid(4), service_code: "cvs-711", store_code: "123456", store_name: "Synthetic Store",
  store_address: "Synthetic Address 1",
};

async function cvsSession() {
  const prepared = await handleBuyerRequest(req("POST", "session/prepare", { body: "{}" }));
  const ck = cookie(prepared);
  const { context } = await prepared.json();
  return { ck, context };
}
const cvsCall = (s, method, suffix, { body, key } = {}) =>
  handleBuyerRequest(
    req(method, suffix, {
      cookie: s.ck, context: s.context, body,
      extra: key ? { "Idempotency-Key": key } : {},
    }),
  );

test("CVS BFF: exact routes, keyed open/store, keyless verify, validated answers, no generic proxy", async () => {
  enabled();
  const old = globalThis.fetch;
  const calls = [];
  let answer = () => Response.json({}, { status: 500 });
  globalThis.fetch = async (url, options) => {
    calls.push({ url: String(url), method: options.method, key: options.headers.get("Idempotency-Key"), body: options.body });
    return answer(String(url), options);
  };
  try {
    const s = await cvsSession();
    // open: 201 from Go, validated, forwarded with the key and the exact body
    answer = () => Response.json({ selection_id: cid(1), expires_at: "2026-09-30T10:15:00Z", form: mapFormBody() }, { status: 201 });
    let response = await cvsCall(s, "POST", "cvs-selections", { body: JSON.stringify(selectionBody), key: "open-key-0001" });
    assert.equal(response.status, 200);
    assert.equal((await response.json()).selection_id, cid(1));
    assert.equal(calls.at(-1).url, `${api}/v1/buyer/cvs-selections`);
    assert.equal(calls.at(-1).key, "open-key-0001");
    assert.equal(calls.at(-1).body, JSON.stringify(selectionBody));

    const before = calls.length;
    for (const [name, opts, status] of [
      ["no key", { body: JSON.stringify(selectionBody) }, 422],
      ["claim return path", { body: JSON.stringify({ ...selectionBody, return_path: "/zh-TW/claim" }), key: "open-key-0002" }, 400],
      ["off-site return path", { body: JSON.stringify({ ...selectionBody, return_path: "//evil.example.test/en/products/x" }), key: "open-key-0003" }, 400],
      ["extra field", { body: JSON.stringify({ ...selectionBody, return_origin: "https://evil.example.test" }), key: "open-key-0004" }, 400],
      ["missing field", { body: JSON.stringify((({ return_path, ...r }) => r)(selectionBody)), key: "open-key-0005" }, 400],
      ["bad market id", { body: JSON.stringify({ ...selectionBody, market_id: "x" }), key: "open-key-0006" }, 400],
      ["duplicate keys", { body: '{"cart_version":2,"cart_version":3,"market_id":"' + cid(4) + '","service_code":"c","return_path":"/en/products/x"}', key: "open-key-0007" }, 400],
    ]) {
      response = await cvsCall(s, "POST", "cvs-selections", opts);
      assert.equal(response.status, status, name);
    }
    assert.equal(calls.length, before, "no rejected request reached the private API");

    // a tampered upstream form never reaches the browser
    answer = () => Response.json({ selection_id: cid(1), expires_at: "2026-09-30T10:15:00Z", form: { ...mapFormBody(), action: "https://evil.example.test/map" } }, { status: 201 });
    response = await cvsCall(s, "POST", "cvs-selections", { body: JSON.stringify(selectionBody), key: "open-key-0008" });
    assert.equal(response.status, 503);

    // verify: keyless, bodyless POST; GET by id; answers must name the same selection
    const projection = { selection_id: cid(1), state: "OPEN", reject_code: null, retry_after_s: null, pickup: null };
    answer = () => Response.json(projection);
    response = await cvsCall(s, "POST", `cvs-selections/${cid(1)}/verify`);
    assert.equal(response.status, 200);
    assert.equal(calls.at(-1).url, `${api}/v1/buyer/cvs-selections/${cid(1)}/verify`);
    assert.equal(calls.at(-1).key, null);
    response = await cvsCall(s, "GET", `cvs-selections/${cid(1)}`);
    assert.equal(response.status, 200);
    assert.equal(calls.at(-1).method, "GET");
    answer = () => Response.json({ ...projection, selection_id: cid(2) });
    assert.equal((await cvsCall(s, "GET", `cvs-selections/${cid(1)}`)).status, 503, "another selection's answer");
    const count = calls.length;
    assert.equal((await cvsCall(s, "POST", `cvs-selections/${cid(1)}/verify`, { key: "verify-key-01" })).status, 422, "key on keyless route");
    assert.equal((await cvsCall(s, "POST", `cvs-selections/${cid(1)}/verify`, { body: "{}" })).status, 422, "body on bodyless route");
    assert.equal((await cvsCall(s, "GET", `cvs-selections/${cid(1)}/verify`)).status, 405);
    assert.equal((await cvsCall(s, "POST", `cvs-selections/${cid(1)}`, { body: "{}", key: "verify-key-02" })).status, 405);
    assert.equal((await cvsCall(s, "GET", "cvs-selections/not-a-uuid")).status, 422);
    assert.equal((await cvsCall(s, "GET", `cvs-selections/${cid(1)}/extra`)).status, 404);
    assert.equal((await cvsCall(s, "GET", "cvs-selections")).status, 405);
    assert.equal((await cvsCall(s, "POST", `cvs-selections/${cid(1)}/verify?x=1`)).status, 422);
    assert.equal(calls.length, count, "grammar denials stay local");

    // buyer-entered store: keyed, answer validated as source=buyer_entered
    const stored = { pickup_id: cid(9), kind: "cvs_711", code: "123456", name: "Synthetic Store", address: "Synthetic Address 1", source: "buyer_entered" };
    answer = () => Response.json(stored, { status: 201 });
    response = await cvsCall(s, "POST", "cvs-stores", { body: JSON.stringify(storeBody), key: "store-key-0001" });
    assert.equal(response.status, 200);
    assert.deepEqual(await response.json(), stored);
    assert.equal(calls.at(-1).url, `${api}/v1/buyer/cvs-stores`);
    for (const bad of [
      { ...storeBody, extra: 1 },
      (({ store_name, ...r }) => r)(storeBody),
      { ...storeBody, store_code: 123456 },
    ])
      assert.equal((await cvsCall(s, "POST", "cvs-stores", { body: JSON.stringify(bad), key: "store-key-0002" })).status, 400);
    answer = () => Response.json({ ...stored, source: "PROVIDER_DIRECTORY_VERIFIED" }, { status: 201 });
    assert.equal((await cvsCall(s, "POST", "cvs-stores", { body: JSON.stringify(storeBody), key: "store-key-0003" })).status, 503);
  } finally {
    globalThis.fetch = old;
  }
});

test("CVS BFF: contract refusal codes pass through; pay_at_pickup_limit is a definite (non-retryable) 429", async () => {
  enabled();
  const old = globalThis.fetch;
  const calls = [];
  let refusal = { status: 422, code: "bad_store_code" };
  globalThis.fetch = async (url, options) => {
    calls.push({ url: String(url), body: options.body });
    return Response.json({ code: refusal.code, message: "x", request_id: "0".repeat(32), retryable: false, details: {} }, { status: refusal.status });
  };
  try {
    const s = await cvsSession();
    let response = await cvsCall(s, "POST", "cvs-stores", { body: JSON.stringify(storeBody), key: "store-key-0004" });
    let body = await response.json();
    assert.equal(response.status, 422);
    assert.equal(body.code, "bad_store_code");
    assert.equal(body.retryable, false);
    const checkout = { quote_id: cid(4), destination_id: cid(5), cart_version: 2, service_version: 1, allocation_version: 1, payment_mode: "pay_at_pickup" };
    for (const next of [
      { status: 429, code: "pay_at_pickup_limit" },
      { status: 422, code: "pay_at_pickup_unavailable" },
      { status: 422, code: "pay_at_pickup_amount_exceeds" },
      { status: 422, code: "cvs_recipient_rejected" },
    ]) {
      refusal = next;
      response = await cvsCall(s, "POST", "checkout", { body: JSON.stringify(checkout), key: "checkout-key-01" });
      body = await response.json();
      assert.equal(response.status, next.status, next.code);
      assert.equal(body.code, next.code);
      assert.equal(body.retryable, false, `${next.code} committed nothing, never "retry"`);
    }
    assert.equal(calls.at(-1).body, JSON.stringify(checkout), "payment_mode is forwarded");
    // an ordinary rate limit stays retryable; an unlisted code degrades to unavailable
    refusal = { status: 429, code: "rate_limited" };
    response = await cvsCall(s, "POST", "checkout", { body: JSON.stringify(checkout), key: "checkout-key-02" });
    assert.equal((await response.json()).retryable, true);
    refusal = { status: 422, code: "made_up_code" };
    response = await cvsCall(s, "POST", "checkout", { body: JSON.stringify(checkout), key: "checkout-key-03" });
    assert.equal(response.status, 503);
    const sent = calls.length;
    for (const mode of ["cash", "", 1, null])
      assert.equal(
        (await cvsCall(s, "POST", "checkout", { body: JSON.stringify({ ...checkout, payment_mode: mode }), key: "checkout-key-04" })).status,
        400,
        String(mode),
      );
    assert.equal(calls.length, sent, "a bad payment_mode never reaches Go");
    // the pre-CVS body (no payment_mode) is still accepted
    refusal = { status: 422, code: "invalid_request" };
    const { payment_mode, ...legacy } = checkout;
    response = await cvsCall(s, "POST", "checkout", { body: JSON.stringify(legacy), key: "checkout-key-05" });
    assert.equal(response.status, 422, "legacy body reaches Go and gets Go's own answer");
    assert.equal(calls.length, sent + 1);
  } finally {
    globalThis.fetch = old;
  }
});
