// ads-core/ads-capi review fix: the frozen ad link and the Meta feed link are origin + /products/{id} (no locale).
// The route handler must 308 to the served /zh-TW/products/{id}; anything but a lowercase uuid stays 404.
import test from "node:test";
import assert from "node:assert/strict";
import { GET } from "../app/products/[productID]/route.ts";

const id = "0b2f6f3e-3c4d-4a59-8f0e-1a2b3c4d5e6f";
const call = (productID) => GET(new Request(`https://shop.example/products/${productID}`), { params: Promise.resolve({ productID }) });

test("/products/{uuid} redirects permanently to the zh-TW product page", async () => {
  const res = await call(id);
  assert.equal(res.status, 308);
  assert.equal(res.headers.get("location"), `/zh-TW/products/${id}`);
});

test("non-uuid ids are 404, never redirected", async () => {
  for (const bad of ["abc", id.toUpperCase(), `${id}/x`, "../etc", `${id}?x=1`]) assert.equal((await call(bad)).status, 404, bad);
});
