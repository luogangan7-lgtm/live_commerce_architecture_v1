import test from "node:test";
import assert from "node:assert/strict";
import { BuyerClientError, buyerRequest } from "../lib/buyer-client.ts";

const orderID = "12345678-1234-1234-1234-123456789abc";
const context = Buffer.alloc(32, 3).toString("base64url");
const suffix = `orders/${orderID}/payment/handoff`;

function browser() {
  const data = new Map();
  Object.defineProperty(globalThis, "window", {
    configurable: true,
    value: {
      localStorage: {
        getItem: (name) => data.get(name) ?? null,
        setItem: (name, value) => data.set(name, value),
        removeItem: (name) => data.delete(name),
      },
    },
  });
  return data;
}

async function rejectsLocal(work, code = "request_failed") {
  await assert.rejects(work, (error) => error instanceof BuyerClientError && error.code === code);
}

test("BPT05 exact handoff permits absent body/key, one fetch, no form persistence", async () => {
  const data = browser();
  const old = globalThis.fetch;
  const calls = [];
  globalThis.fetch = async (url, options) => {
    calls.push({ url, options });
    return Response.json({ disposition: "ISSUED", form: { action: "https://sandbox-api.payuni.com.tw/api/upp" } });
  };
  try {
    const result = await buyerRequest("POST", suffix, context);
    assert.equal(result.status, 200);
    assert.equal(calls.length, 1);
    assert.equal(calls[0].url, `/api/buyer/${suffix}`);
    assert.equal(calls[0].options.method, "POST");
    assert.equal(calls[0].options.body, undefined);
    assert.equal(calls[0].options.headers.get("Idempotency-Key"), null);
    assert.equal(calls[0].options.headers.get("Content-Type"), null);
    assert.equal(calls[0].options.headers.get("X-Buyer-Context"), context);
    assert.equal(calls[0].options.credentials, "same-origin");
    assert.equal(calls[0].options.redirect, "error");
    assert.equal(data.size, 0);
  } finally {
    globalThis.fetch = old;
  }
});

test("BPT05 wrong handoff form and ordinary writes fail before any fetch", async () => {
  browser();
  const old = globalThis.fetch;
  let calls = 0;
  globalThis.fetch = () => { calls++; throw new Error("must not fetch"); };
  try {
    for (const [method, path, body, key] of [
      ["POST", suffix, {}, undefined],
      ["POST", suffix, null, undefined],
      ["POST", suffix, undefined, "valid-key-1"],
      ["POST", suffix + "?x=1", undefined, undefined],
      ["POST", `orders/${orderID.toUpperCase()}/payment/handoff`, undefined, undefined],
      ["POST", `orders/${orderID}/payment/handoff/`, undefined, undefined],
      ["POST", `orders/${orderID}/payment/prepare`, undefined, undefined],
      ["POST", "checkout", undefined, undefined],
      ["PUT", "cart", undefined, undefined],
      ["GET", `orders/${orderID}/payment`, {}, undefined],
    ]) await rejectsLocal(buyerRequest(method, path, context, body, key));
    await rejectsLocal(buyerRequest("POST", suffix, "invalid"), "context_changed");
    assert.equal(calls, 0);
  } finally {
    globalThis.fetch = old;
  }
});

test("BPT05 unresolved cookie journal blocks handoff; lost response is one fetch without persistence", async () => {
  const data = browser();
  const old = globalThis.fetch;
  let calls = 0;
  globalThis.fetch = async () => { calls++; throw new Error("response lost"); };
  try {
    data.set("commerce-buyer-pending-v1", JSON.stringify({
      v: 1,
      id: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
      baseline: context,
      phase: "reset",
    }));
    await rejectsLocal(buyerRequest("POST", suffix, context), "uncertain");
    assert.equal(calls, 0);
    data.delete("commerce-buyer-pending-v1");
    await rejectsLocal(buyerRequest("POST", suffix, context), "uncertain");
    assert.equal(calls, 1);
    assert.equal(data.size, 0);
  } finally {
    globalThis.fetch = old;
  }
});

// ---- SU02 client half (author-side): refresh/cancel share the keyless exception.
for (const kind of ["refresh", "cancel"]) {
  test(`SU02 ${kind} permits absent body/key, one fetch, no persistence`, async () => {
    const data = browser();
    const old = globalThis.fetch;
    const calls = [];
    globalThis.fetch = async (url, options) => {
      calls.push({ url, options });
      return Response.json({ order_id: orderID, scheduled: true });
    };
    try {
      const result = await buyerRequest("POST", `orders/${orderID}/payment/${kind}`, context);
      assert.equal(result.status, 200);
      assert.equal(calls.length, 1);
      assert.equal(calls[0].url, `/api/buyer/orders/${orderID}/payment/${kind}`);
      assert.equal(calls[0].options.body, undefined);
      assert.equal(calls[0].options.headers.get("Idempotency-Key"), null);
      assert.equal(calls[0].options.redirect, "error");
      assert.equal(data.size, 0);
    } finally {
      globalThis.fetch = old;
    }
  });

  test(`SU02 ${kind} rejects a body, a key or a malformed path before any fetch`, async () => {
    browser();
    const old = globalThis.fetch;
    let calls = 0;
    globalThis.fetch = () => { calls++; throw new Error("must not fetch"); };
    try {
      const at = `orders/${orderID}/payment/${kind}`;
      for (const [path, body, key] of [
        [at, {}, undefined], [at, undefined, "valid-key-1"], [`${at}?x=1`, undefined, undefined],
        [`${at}/`, undefined, undefined], [`orders/${orderID.toUpperCase()}/payment/${kind}`, undefined, undefined],
      ]) await rejectsLocal(buyerRequest("POST", path, context, body, key));
      assert.equal(calls, 0);
    } finally {
      globalThis.fetch = old;
    }
  });
}
