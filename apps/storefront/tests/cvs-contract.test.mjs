import test from "node:test";
import assert from "node:assert/strict";
import {
  CVS_ERROR_CODES,
  CVS_MAP_ACTIONS,
  CVS_SEARCH_LINKS,
  DEFINITE_CVS_CODES,
  cvsReturnID,
  errorField,
  isCvsErrorCode,
  normalizeSelection,
  normalizeTwMobile,
  recipientNameOK,
  saveCvsDraft,
  shipmentCopyKey,
  takeCvsDraft,
  validBuyerCvsShipment,
  validBuyerStore,
  validBuyerStoreBody,
  validCvsSelection,
  validCvsSelectionOpen,
  validMapForm,
  validReturnPath,
  validStoreAddress,
  validStoreCode,
  validStoreName,
} from "../lib/cvs-contract.ts";
import { buyerRequest, BuyerClientError } from "../lib/buyer-client.ts";

// Synthetic values only (no real buyer, store or merchant data). Contract: taiwan-cvs-logistics-v1 §5.2, §16.
const uid = (n) => `00000000-0000-0000-0000-${String(n).padStart(12, "0")}`;
const fields = () => ({
  MerchantID: "2000132",
  MerchantTradeNo: "AbCdEfGhIjKlMnOpQrSt",
  LogisticsType: "CVS",
  LogisticsSubType: "UNIMARTC2C",
  IsCollection: "N",
  ServerReplyURL: `https://hooks.example.test/v1/cvs/ecpay/map-return/${uid(1)}`,
  Device: "1",
});
const form = (patch = {}) => ({ action: CVS_MAP_ACTIONS[0], fields: { ...fields(), ...patch } });
const open = { selection_id: uid(1), expires_at: "2026-09-30T10:15:00Z", form: form() };
const pickup = {
  pickup_id: uid(2),
  kind: "cvs_711",
  code: "123456",
  name: "Synthetic Store",
  address: "Synthetic Address 1",
  outside: false,
};
const verified = { selection_id: uid(1), state: "VERIFIED", reject_code: null, retry_after_s: null, pickup };

test("CVS01 map form: only the two ECPay e-map actions, exact fields, top-level-safe values", () => {
  assert.equal(validMapForm(form()), true);
  assert.equal(validMapForm({ action: CVS_MAP_ACTIONS[1], fields: fields() }), true);
  for (const [name, bad] of [
    ["other host", { action: "https://evil.example.test/Express/map", fields: fields() }],
    ["http action", { action: "http://logistics.ecpay.com.tw/Express/map", fields: fields() }],
    ["path variant", { action: "https://logistics.ecpay.com.tw/Express/map/", fields: fields() }],
    ["collection Y", form({ IsCollection: "Y" })],
    ["not CVS", form({ LogisticsType: "HOME" })],
    ["unknown subtype", form({ LogisticsSubType: "UNIMART" })],
    ["http reply url", form({ ServerReplyURL: "http://hooks.example.test/x" })],
    ["device 2", form({ Device: "2" })],
    ["empty merchant", form({ MerchantID: "" })],
    ["extra field", form({ Extra: "x" })],
    ["extra form key", { ...form(), target: "_blank" }],
  ])
    assert.equal(validMapForm(bad), false, name);
  const missing = form();
  delete missing.fields.Device;
  assert.equal(validMapForm(missing), false, "missing Device");
  assert.equal(validCvsSelectionOpen(open), true);
  assert.equal(validCvsSelectionOpen({ ...open, selection_id: "x" }), false);
  assert.equal(validCvsSelectionOpen({ ...open, form: form({ IsCollection: "Y" }) }), false);
  assert.equal(validCvsSelectionOpen({ ...open, extra: 1 }), false);
});

test("CVS02 return path allowlist excludes /claim and anything but the product page", () => {
  for (const good of ["/zh-TW/products/abc_1-2", "/en/products/x", "/zh-CN/products/" + "a".repeat(64)])
    assert.equal(validReturnPath(good), true, good);
  for (const bad of [
    "/zh-TW/claim",
    "/fr/products/x",
    "/en/products/",
    "/en/products/a/b",
    "//evil.example.test/en/products/x",
    "https://x.test/en/products/x",
    "/en/products/x?y=1",
    "/en/products/x#f",
    "/en/products/" + "a".repeat(65),
  ])
    assert.equal(validReturnPath(bad), false, bad);
});

test("CVS03 selection projection: VERIFIED<=>pickup, REJECTED<=>reject_code, absent keys read as null", () => {
  assert.equal(validCvsSelection(verified), true);
  assert.equal(validCvsSelection({ selection_id: uid(1), state: "OPEN" }), true, "Go may omit null keys");
  assert.deepEqual(normalizeSelection({ selection_id: uid(1), state: "OPEN" }), {
    selection_id: uid(1),
    state: "OPEN",
    reject_code: null,
    retry_after_s: null,
    pickup: null,
  });
  assert.equal(
    validCvsSelection({ selection_id: uid(1), state: "RETURNED", reject_code: null, retry_after_s: 30, pickup: null }),
    true,
  );
  assert.equal(
    validCvsSelection({ selection_id: uid(1), state: "REJECTED", reject_code: "store_not_found", retry_after_s: null, pickup: null }),
    true,
  );
  for (const [name, bad] of [
    ["VERIFIED without pickup", { ...verified, pickup: null }],
    ["pickup without VERIFIED", { ...verified, state: "RETURNED" }],
    ["REJECTED without code", { selection_id: uid(1), state: "REJECTED" }],
    ["code without REJECTED", { selection_id: uid(1), state: "OPEN", reject_code: "x_y" }],
    ["bad code charset", { selection_id: uid(1), state: "REJECTED", reject_code: "Bad-Code" }],
    ["unknown state", { ...verified, state: "DONE" }],
    ["negative retry", { selection_id: uid(1), state: "RETURNED", retry_after_s: -1 }],
    ["fractional retry", { selection_id: uid(1), state: "RETURNED", retry_after_s: 1.5 }],
    ["unknown key", { ...verified, owner_id: uid(9) }],
    ["bad id", { ...verified, selection_id: "nope" }],
    ["pickup extra key", { ...verified, pickup: { ...pickup, namespace: "ecpay.UNIMARTC2C" } }],
    ["pickup bad kind", { ...verified, pickup: { ...pickup, kind: "home" } }],
    ["pickup outside not boolean", { ...verified, pickup: { ...pickup, outside: "no" } }],
    ["pickup empty name", { ...verified, pickup: { ...pickup, name: "" } }],
  ])
    assert.equal(validCvsSelection(bad), false, name);
  // X10: 7-11 values arrive space-padded and FamilyMart uses full-width digits; both are strings as returned.
  assert.equal(validCvsSelection({ ...verified, pickup: { ...pickup, code: "006045", address: "台北市中正區忠孝西路１段３－１號" } }), true);
});

test("CVS04 buyer-entered store answer and request body", () => {
  const store = { pickup_id: uid(3), kind: "cvs_okmart", code: "0012", name: "S", address: "Synthetic Address 2", source: "buyer_entered" };
  assert.equal(validBuyerStore(store), true);
  for (const bad of [
    { ...store, source: "PROVIDER_DIRECTORY_VERIFIED" },
    { ...store, source: "verified" },
    { ...store, kind: "home" },
    { ...store, extra: 1 },
    { ...store, pickup_id: "x" },
  ])
    assert.equal(validBuyerStore(bad), false);
  const body = {
    cart_version: 3,
    market_id: uid(4),
    service_code: "cvs-711",
    store_code: "001234",
    store_name: "Synthetic Store",
    store_address: "Synthetic Address 3",
  };
  assert.equal(validBuyerStoreBody(body, "cvs_711"), true);
  for (const [name, bad] of [
    ["okmart 6 digits", [{ ...body, store_code: "001234" }, "cvs_okmart"]],
    ["711 five digits", [{ ...body, store_code: "12345" }, "cvs_711"]],
    ["letters", [{ ...body, store_code: "12345A" }, "cvs_711"]],
    ["untrimmed name", [{ ...body, store_name: " S" }, "cvs_711"]],
    ["short address", [{ ...body, store_address: "abcd" }, "cvs_711"]],
    ["extra key", [{ ...body, return_path: "/en/products/x" }, "cvs_711"]],
    ["missing key", [(({ store_name, ...rest }) => rest)(body), "cvs_711"]],
    ["bad service code", [{ ...body, service_code: "Bad Code" }, "cvs_711"]],
    ["cart 0", [{ ...body, cart_version: 0 }, "cvs_711"]],
  ])
    assert.equal(validBuyerStoreBody(...bad), false, name);
});

test("CVS05 store code table (§16.1) keeps leading zeros and per-chain lengths", () => {
  assert.equal(validStoreCode("cvs_711", "000123"), true);
  assert.equal(validStoreCode("cvs_711", "1234567"), false);
  assert.equal(validStoreCode("cvs_familymart", "006045"), true);
  assert.equal(validStoreCode("cvs_familymart", "６０４５００"), false, "full-width digits are not a typed code");
  assert.equal(validStoreCode("cvs_okmart", "0123"), true);
  assert.equal(validStoreCode("cvs_okmart", "012"), false);
  for (const ok of ["123", "12345678"]) assert.equal(validStoreCode("cvs_hilife", ok), true);
  for (const bad of ["12", "123456789", "12 34", ""]) assert.equal(validStoreCode("cvs_hilife", bad), false);
  assert.equal(validStoreName("一"), true);
  assert.equal(validStoreName("x".repeat(40)), true);
  assert.equal(validStoreName("x".repeat(41)), false);
  assert.equal(validStoreName("  "), false);
  assert.equal(validStoreName("a\nb"), false);
  assert.equal(validStoreAddress("12345"), true);
  assert.equal(validStoreAddress("1234"), false);
  assert.equal(validStoreAddress("x".repeat(121)), false);
  assert.equal(validStoreAddress("a b street"), false);
});

test("CVS06 recipient mirror: real-name width 4..10 (CJK=2), no digits/symbols/emoji; 09 mobile", () => {
  for (const ok of ["王小明", "王明", "陳大文文", "Ann Lee", "ANNE", "王小明明明"])
    assert.equal(recipientNameOK(ok), true, ok);
  for (const bad of ["王", "Ann", "王小明明明明", "A1ce Lee", "小明!", "王小明😀", "a_b_c_d", "", "   ", "Ann\nLee"])
    assert.equal(recipientNameOK(bad), false, JSON.stringify(bad));
  assert.equal(normalizeTwMobile("0912345678"), "0912345678");
  assert.equal(normalizeTwMobile("0912-345-678"), "0912345678");
  assert.equal(normalizeTwMobile("(0912) 345 678"), "0912345678");
  assert.equal(normalizeTwMobile("+886912345678"), "0912345678");
  for (const bad of ["+8860912345678", "0812345678", "091234567", "09123456789", "0912a45678", "886912345678", ""])
    assert.equal(normalizeTwMobile(bad), null, bad);
});

function memoryStore() {
  const data = new Map();
  return {
    data,
    getItem: (k) => data.get(k) ?? null,
    setItem: (k, v) => data.set(k, v),
    removeItem: (k) => data.delete(k),
  };
}
test("CVS07 checkout draft: name, phone and mode only; read once; corrupt/foreign shapes are dropped", () => {
  const store = memoryStore();
  const context = "a".repeat(43);
  saveCvsDraft(store, context, { recipient_name: "Synthetic Name", phone: "0912345678", payment_mode: "pay_at_pickup" });
  const raw = [...store.data.values()][0];
  assert.deepEqual(Object.keys(JSON.parse(raw)).sort(), ["payment_mode", "phone", "recipient_name", "v"]);
  assert.equal(takeCvsDraft(store, "b".repeat(43)), null, "other context");
  assert.deepEqual(takeCvsDraft(store, context), {
    v: 1,
    recipient_name: "Synthetic Name",
    phone: "0912345678",
    payment_mode: "pay_at_pickup",
  });
  assert.equal(store.data.size, 0, "consumed on read");
  assert.equal(takeCvsDraft(store, context), null);
  for (const bad of [
    "not json",
    JSON.stringify({ v: 1, recipient_name: "n", phone: "1", payment_mode: "cash" }),
    JSON.stringify({ v: 1, recipient_name: "n", phone: "1", payment_mode: "card", token: "x" }),
    JSON.stringify({ v: 2, recipient_name: "n", phone: "1", payment_mode: "card" }),
    "x".repeat(3000),
  ]) {
    store.setItem(`commerce-cvs-draft-v1:${context}`, bad);
    assert.equal(takeCvsDraft(store, context), null);
    assert.equal(store.data.size, 0, "a bad draft is removed too");
  }
  const throwing = { getItem: () => { throw new Error("blocked"); }, setItem() {}, removeItem() {} };
  assert.equal(takeCvsDraft(throwing, context), null);
});

test("CVS08 return URL: only ?cvs_selection=<uuid> is accepted", () => {
  assert.equal(cvsReturnID(`?cvs_selection=${uid(7)}`), uid(7));
  for (const bad of ["", "?", `?cvs_selection=${uid(7)}&x=1`, `?x=1&cvs_selection=${uid(7)}`, "?cvs_selection=abc", `?cvs_selection=${"aaaaaaaa-0000-0000-0000-00000000000b".toUpperCase()}`, `cvs_selection=${uid(7)}`])
    assert.equal(cvsReturnID(bad), null, bad);
});

test("CVS09 buyer order projection and copy keys: nothing but PICKED_UP says picked up", () => {
  const shipment = { state: "AT_STORE", chain: "cvs_711", store_name: "S", store_code: "123456", updated_at: "2026-09-30T01:02:03Z" };
  assert.equal(validBuyerCvsShipment(shipment), true);
  for (const bad of [
    { ...shipment, state: "DELIVERED" },
    { ...shipment, chain: "home" },
    { ...shipment, provider_logistics_id: "x" },
    { ...shipment, updated_at: "later" },
    { ...shipment, code: "x" },
  ])
    assert.equal(validBuyerCvsShipment(bad), false);
  for (const s of ["REQUESTED", "UNKNOWN", "FAILED", "ABANDONED"]) assert.equal(shipmentCopyKey(s), "processing", s);
  for (const s of ["CREATED", "AT_DC", "AT_STORE", "PICKED_UP", "UNCLAIMED"]) assert.equal(shipmentCopyKey(s), s);
});

test("CVS10 error codes: field mapping, definite refusals, official links are https constants", () => {
  assert.equal(errorField("bad_store_code"), "store_code");
  assert.equal(errorField("bad_store_name"), "store_name");
  assert.equal(errorField("bad_store_address"), "store_address");
  assert.equal(errorField("cvs_recipient_rejected"), "recipient");
  assert.equal(errorField("cvs_source_mismatch"), "store");
  assert.equal(errorField("pay_at_pickup_limit"), "payment");
  assert.equal(errorField("nope"), null);
  assert.equal(isCvsErrorCode("pay_at_pickup_limit"), true);
  assert.equal(isCvsErrorCode("rate_limited"), false);
  assert.equal(CVS_ERROR_CODES.every((c) => /^[a-z_]{1,64}$/.test(c)), true, "matches the client definiteError code charset");
  assert.deepEqual(DEFINITE_CVS_CODES, ["pay_at_pickup_limit"]);
  for (const url of Object.values(CVS_SEARCH_LINKS)) assert.match(url, /^https:\/\/[a-z0-9.-]+\//);
  assert.deepEqual(Object.keys(CVS_SEARCH_LINKS).sort(), ["cvs_711", "cvs_familymart", "cvs_hilife", "cvs_okmart"]);
});

test("CVS11 client transport: verify is the one new keyless POST; the server refusal code rides BuyerClientError.detail", async () => {
  const oldFetch = globalThis.fetch;
  const data = new Map();
  globalThis.localStorage = { getItem: (k) => data.get(k) ?? null, setItem: (k, v) => data.set(k, v), removeItem: (k) => data.delete(k) };
  globalThis.window = { localStorage };
  const seen = [];
  globalThis.fetch = async (url, init) => {
    seen.push({ url, method: init.method, key: new Headers(init.headers).get("Idempotency-Key"), body: init.body });
    return Response.json({});
  };
  const context = "a".repeat(43);
  try {
    const verify = `cvs-selections/${uid(1)}/verify`;
    await buyerRequest("POST", verify, context);
    assert.deepEqual(seen.at(-1), { url: `/api/buyer/${verify}`, method: "POST", key: null, body: undefined });
    await assert.rejects(buyerRequest("POST", verify, context, {}, "key-0000001"), (e) => e instanceof BuyerClientError && e.code === "request_failed");
    await assert.rejects(buyerRequest("POST", verify, context, undefined, "key-0000001"), BuyerClientError);
    // any other keyless POST is still refused (no generic keyless mutation)
    await assert.rejects(buyerRequest("POST", `cvs-selections/${uid(1)}`, context), BuyerClientError);
    await assert.rejects(buyerRequest("POST", "cvs-stores", context), BuyerClientError);
    await assert.rejects(buyerRequest("POST", `cvs-selections/${uid(1)}/verify/x`, context), BuyerClientError);
    // keyed open/store still require key + body
    await buyerRequest("POST", "cvs-selections", context, { a: 1 }, "key-0000002");
    await buyerRequest("POST", "cvs-stores", context, { a: 1 }, "key-0000003");
    await assert.rejects(buyerRequest("POST", "cvs-stores", context, { a: 1 }), BuyerClientError);
    assert.equal(seen.length, 3);
    assert.equal(new BuyerClientError("request_failed", 422, "bad_store_code").detail, "bad_store_code");
    assert.equal(new BuyerClientError("uncertain").detail, undefined);
  } finally {
    globalThis.fetch = oldFetch;
    delete globalThis.window;
    delete globalThis.localStorage;
  }
});
