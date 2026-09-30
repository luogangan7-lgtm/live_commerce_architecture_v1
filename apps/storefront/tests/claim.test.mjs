// Claim-link BFF and contract (live-keyword-claims-v1 §7.2, §11.1): the token travels only
// as X-Commerce-Claim-Token on B1/B2, is never echoed, and only the frozen projections
// are forwarded. Private Go transport is stubbed; real chains are KC14/KC16.
import test from "node:test";
import assert from "node:assert/strict";
import { handleBuyerRequest } from "../lib/buyer-server.ts";
import {
  claimFragment,
  validClaimPreview,
  validClaimRedeemed,
} from "../lib/claim-contract.ts";

const bff = Buffer.alloc(32, 1).toString("base64url");
const signing = Buffer.alloc(32, 2).toString("base64url");
const origin = "https://shop.example";
const api = "http://127.0.0.1:3219";
const token = Buffer.alloc(32, 7).toString("base64url");
const sku = "11111111-1111-4111-8111-111111111111";
const preview = {
  bundle_version: 2, bound: false, expires_at: "2026-10-01T00:00:00Z",
  lines: [{ keyword: "A1", sku_id: sku, sku_code: "A-RED", product_name: "Red", currency: "TWD",
    unit_price_minor: 1200, quantity: 2, pending: true, available: true }],
};
const redeemed = {
  bundle_version: 2,
  cart: { id: "22222222-2222-4222-8222-222222222222", currency: "TWD", version: 3, items: [{ sku_id: sku, quantity: 2 }] },
  applied: [{ sku_id: sku, quantity: 2 }], skipped: [],
};

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
  return new Request(`${origin}/api/buyer/${suffix}`, { method, headers, body });
}

async function session() {
  const response = await handleBuyerRequest(req("POST", "session/prepare", { body: "{}" }));
  assert.equal(response.status, 200);
  return { cookie: response.headers.get("set-cookie").split(";", 1)[0], context: (await response.json()).context };
}

test("fragment grammar accepts only one canonical token", () => {
  assert.equal(claimFragment(`#t=${token}`), token);
  for (const hash of ["", "#", `#t=${token}&x=1`, `#x=${token}`, `#t=${token.slice(0, 42)}B`, `#t=${token}=`, `#T=${token}`])
    assert.equal(claimFragment(hash), null, hash);
});

test("claim projections are closed", () => {
  assert.equal(validClaimPreview(preview), true);
  assert.equal(validClaimPreview({ ...preview, label: "amy" }), false);
  assert.equal(validClaimPreview({ ...preview, lines: [] }), false);
  assert.equal(validClaimPreview({ ...preview, lines: [{ ...preview.lines[0], actor_key: "x" }] }), false);
  assert.equal(validClaimRedeemed(redeemed), true);
  assert.equal(validClaimRedeemed({ ...redeemed, skipped: [{ sku_id: sku, reason: "gone" }] }), false);
  assert.equal(validClaimRedeemed({ ...redeemed, cart: { ...redeemed.cart, id: "" , version: 0, items: [] } }), true);
});

test("claim token rides only its header on B1/B2 and is never echoed", async () => {
  enabled();
  const old = globalThis.fetch;
  const calls = [];
  globalThis.fetch = async (url, options) => {
    calls.push({ url: String(url), options });
    if (String(url).endsWith("/session/bootstrap"))
      return Response.json({ authenticated: true, expires_at: new Date(Date.now() + 3500_000).toISOString() });
    if (String(url).endsWith("/claim-link")) return Response.json(preview);
    if (String(url).endsWith("/claim-link/redeem")) return Response.json(redeemed);
    return Response.json({ id: "", currency: "TWD", version: 0, items: [] });
  };
  try {
    const { cookie, context } = await session();
    const activated = await handleBuyerRequest(req("POST", "session/activate", { cookie, context, body: "{}" }));
    assert.equal(activated.status, 200);
    const claim = { "X-Commerce-Claim-Token": token };

    let response = await handleBuyerRequest(req("GET", "claim-link", { cookie, context, extra: claim }));
    assert.equal(response.status, 200);
    assert.deepEqual(await response.json(), preview);
    let call = calls.at(-1);
    assert.equal(call.url, `${api}/v1/buyer/claim-link`);
    assert.equal(call.options.headers.get("X-Commerce-Claim-Token"), token);
    assert.equal(call.options.headers.get("Idempotency-Key"), null);

    response = await handleBuyerRequest(req("POST", "claim-link/redeem", {
      cookie, context, body: '{"expected_bundle_version":2}', extra: { ...claim, "Idempotency-Key": "redeem-key-1" } }));
    assert.equal(response.status, 200);
    assert.deepEqual(await response.json(), redeemed);
    call = calls.at(-1);
    assert.equal(call.url, `${api}/v1/buyer/claim-link/redeem`);
    assert.equal(call.options.headers.get("X-Commerce-Claim-Token"), token);
    assert.equal(call.options.headers.get("Idempotency-Key"), "redeem-key-1");

    const before = calls.length;
    for (const [method, suffix, extra, body, status] of [
      ["GET", "claim-link", {}, undefined, 422],
      ["GET", "claim-link", { "X-Commerce-Claim-Token": token.slice(0, 42) + "B" }, undefined, 422],
      ["GET", "claim-link", { ...claim, "Idempotency-Key": "read-key-1" }, undefined, 422],
      ["POST", "claim-link/redeem", claim, '{"expected_bundle_version":2}', 422],
      ["POST", "claim-link/redeem", { ...claim, "Idempotency-Key": "redeem-key-2" }, '{"expected_bundle_version":2,"token":"x"}', 400],
      ["GET", "cart", claim, undefined, 403],
      ["PUT", "cart", { ...claim, "Idempotency-Key": "cart-key-01" }, '{"items":[]}', 403],
      ["GET", "claim-link/other", claim, undefined, 404],
      ["DELETE", "claim-link", claim, undefined, 405],
    ]) {
      response = await handleBuyerRequest(req(method, suffix, { cookie, context, body, extra }));
      assert.equal(response.status, status, `${method} ${suffix}`);
      assert.equal((await response.text()).includes(token), false);
    }
    response = await handleBuyerRequest(new Request(`${origin}/api/buyer/claim-link?t=${token}`, {
      method: "GET", headers: { Host: "shop.example", Cookie: cookie, "X-Buyer-Context": context, ...claim } }));
    assert.equal(response.status, 422);
    assert.equal(calls.length, before, "rejected claim requests must not reach Go");

    globalThis.fetch = async () => Response.json({ ...preview, label: "amy" });
    response = await handleBuyerRequest(req("GET", "claim-link", { cookie, context, extra: claim }));
    assert.equal(response.status, 503, "an unexpected projection field is never forwarded");
    globalThis.fetch = async () => Response.json({ code: "not_found" }, { status: 404 });
    response = await handleBuyerRequest(req("GET", "claim-link", { cookie, context, extra: claim }));
    assert.equal(response.status, 404);
    assert.equal((await response.json()).code, "not_found");
  } finally {
    globalThis.fetch = old;
  }
});
