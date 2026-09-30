import assert from "node:assert/strict";
import { test } from "node:test";
import {
  abandonBody,
  collectionActions,
  collectionBody,
  cvsSettingsBody,
  currentAttempt,
  ecpayConnectBody,
  ecpayQualified,
  labelLapsed,
  parseCollectionResult,
  parseCvsSettings,
  parseCvsShipment,
  parseCvsSubmitted,
  parseEcpay,
  parsePrintForm,
  parseReleaseResult,
  releaseBody,
  requestBody,
  validPrintAction,
  validStatusURL,
} from "../../apps/admin/lib/logistics-model.ts";

// Synthetic values only. Contract: taiwan-cvs-logistics-v1 §8 (frozen 2026-09-30) and §16.
const id = "11111111-1111-4111-8111-111111111111";
const ecpay = {
  environment: "SANDBOX", mode: "C2C", merchant_id: "2000933", version: 3, enabled: true,
  qualified_at: "2026-09-30T01:02:03Z", ok_verified: false,
  status_url: "https://hooks.example.test/v1/cvs/ecpay/status/22222222-2222-4222-8222-222222222222",
};
const settings = { version: 2, enabled_chains: ["cvs_711", "cvs_okmart"], pay_at_pickup_enabled: true, pay_at_pickup_max_twd: 3000, pay_at_pickup_max_open: 20 };
const event = { source: "ecpay_status", event_code: "status.2030", provider_code: "2030", provider_message: "to DC", from_state: "CREATED", to_state: "AT_DC", received_at: "2026-09-30T02:00:00Z" };
const attempt = (over: object = {}) => ({
  attempt: 1, state: "CREATED", subtype: "UNIMARTC2C", environment: "SANDBOX", receiver_store_id: "131386", goods_amount: 500,
  collection_amount: null, provider_logistics_id: "1234567890", code: "12345678 1234", print_available: true, result_code: null,
  last_status_code: null, last_status_at: null, alerts: [], created_at: "2026-09-30T01:00:00Z", updated_at: "2026-09-30T01:00:05Z",
  version: 2, events: [event], ...over,
});
const shipment = (over: object = {}, attempts: object[] = [attempt()]) => ({
  current_attempt: 1, expected_version: 2, validity_days: 5, attempts, ...over,
});

test("LGM01 ECPay card DTO: exact keys, enabled implies checked, https status url, no secret field", () => {
  assert.equal(parseEcpay(ecpay).merchant_id, "2000933");
  assert.equal(parseEcpay({ ...ecpay, enabled: false, qualified_at: null }).qualified_at, null);
  assert.equal(ecpayQualified(parseEcpay(ecpay)), true);
  assert.equal(ecpayQualified(parseEcpay({ ...ecpay, enabled: false })), false);
  assert.equal(ecpayQualified(null), false);
  for (const [name, bad] of [
    ["enabled without qualification", { ...ecpay, qualified_at: null }],
    ["hash_key leaked into the DTO", { ...ecpay, hash_key: "x" }],
    ["sender data leaked", { ...ecpay, sender_name: "x" }],
    ["missing status_url", (({ status_url, ...r }) => r)(ecpay)],
    ["http status url", { ...ecpay, status_url: "http://hooks.example.test/x" }],
    ["status url with userinfo", { ...ecpay, status_url: "https://u:p@hooks.example.test/x" }],
    ["status url with query", { ...ecpay, status_url: "https://hooks.example.test/x?a=1" }],
    ["unknown environment", { ...ecpay, environment: "STAGE" }],
    ["unknown mode", { ...ecpay, mode: "B2B" }],
    ["version 0", { ...ecpay, version: 0 }],
    ["merchant id with space", { ...ecpay, merchant_id: "2000 933" }],
    ["enabled not boolean", { ...ecpay, enabled: "true" }],
  ] as const)
    assert.throws(() => parseEcpay(bad), /unavailable/, name);
  assert.equal(validStatusURL("https://hooks.example.test/v1/x"), true);
  assert.equal(validStatusURL("https://localhost/x"), false);
});

test("LGM02 connect body carries exactly the frozen keys and refuses malformed input before the wire", () => {
  const ok = { expectedVersion: 0, environment: "SANDBOX" as const, mode: "C2C" as const, merchantID: " 2000933 ", hashKey: "abcdEFGH12345678", hashIV: "ijklMNOP12345678", senderName: " 王小明 ", senderPhone: "0912345678" };
  const body = ecpayConnectBody(ok)!;
  assert.deepEqual(Object.keys(body).sort(), ["environment", "expected_version", "hash_iv", "hash_key", "merchant_id", "mode", "sender_cell_phone", "sender_name"]);
  assert.equal(body.merchant_id, "2000933");
  assert.equal(body.sender_name, "王小明");
  for (const [name, change] of [
    ["short key", { hashKey: "short" }], ["key with space", { hashKey: "abcd EFGH1234" }], ["iv with control", { hashIV: "abcdEFGH\n1234" }],
    ["empty merchant", { merchantID: " " }], ["merchant symbol", { merchantID: "20-0933" }], ["name too long", { senderName: "一二三四五六七八九十十" }],
    ["name with digit", { senderName: "王1" }], ["phone not 09", { senderPhone: "0212345678" }], ["phone short", { senderPhone: "091234567" }],
    ["bad env", { environment: "TEST" as never }], ["bad mode", { mode: "X" as never }], ["negative version", { expectedVersion: -1 }],
  ] as const)
    assert.equal(ecpayConnectBody({ ...ok, ...change }), null, name);
});

test("LGM03 CVS settings DTO and body follow the §16.5 ranges", () => {
  assert.equal(parseCvsSettings(settings).pay_at_pickup_max_twd, 3000);
  assert.equal(parseCvsSettings({ ...settings, version: 0, pay_at_pickup_enabled: false, pay_at_pickup_max_twd: null }).version, 0);
  for (const [name, bad] of [
    ["pay_at_pickup without a max", { ...settings, pay_at_pickup_max_twd: null }],
    ["max above 20000", { ...settings, pay_at_pickup_max_twd: 20001 }],
    ["max zero", { ...settings, pay_at_pickup_max_twd: 0 }],
    ["open above 500", { ...settings, pay_at_pickup_max_open: 501 }],
    ["open zero", { ...settings, pay_at_pickup_max_open: 0 }],
    ["unknown chain", { ...settings, enabled_chains: ["cvs_711", "cvs_seven"] }],
    ["duplicate chain", { ...settings, enabled_chains: ["cvs_711", "cvs_711"] }],
    ["extra key", { ...settings, extra: 1 }],
    ["chains not array", { ...settings, enabled_chains: "cvs_711" }],
  ] as const)
    assert.throws(() => parseCvsSettings(bad), /unavailable/, name);
  const input = { expectedVersion: 2, chains: ["cvs_okmart", "cvs_711"] as const, payAtPickup: true, maxTWD: "3000", maxOpen: "20" };
  assert.deepEqual(cvsSettingsBody({ ...input, chains: [...input.chains] }), {
    expected_version: 2, enabled_chains: ["cvs_711", "cvs_okmart"], pay_at_pickup_enabled: true, pay_at_pickup_max_twd: 3000, pay_at_pickup_max_open: 20,
  });
  assert.equal(cvsSettingsBody({ ...input, chains: [], payAtPickup: false, maxTWD: "", maxOpen: "1" })?.pay_at_pickup_max_twd, null);
  for (const [name, change] of [
    ["pay on without max", { maxTWD: "" }], ["max 20001", { maxTWD: "20001" }], ["max 0", { maxTWD: "0" }], ["max text", { maxTWD: "12a" }],
    ["max decimal", { maxTWD: "10.5" }], ["open 0", { maxOpen: "0" }], ["open 501", { maxOpen: "501" }], ["open empty", { maxOpen: "" }],
    ["duplicate chain", { chains: ["cvs_711", "cvs_711"] as never }], ["unknown chain", { chains: ["cvs_x"] as never }],
  ] as const)
    assert.equal(cvsSettingsBody({ ...input, chains: [...input.chains], ...change }), null, name);
});

test("LGM04 order CVS shipment DTO: exact frozen keys, no PII/trade number, current attempt must exist", () => {
  assert.equal(parseCvsShipment(shipment()).attempts[0].code, "12345678 1234");
  assert.equal(parseCvsShipment({ current_attempt: null, expected_version: 0, validity_days: null, attempts: [] }).attempts.length, 0);
  assert.equal(currentAttempt(parseCvsShipment(shipment()))?.attempt, 1);
  assert.equal(currentAttempt(null), null);
  const failedThenCreated = shipment({ current_attempt: 2, expected_version: 1 }, [attempt({ state: "FAILED", print_available: false, code: null }), attempt({ attempt: 2 })]);
  assert.equal(currentAttempt(parseCvsShipment(failedThenCreated))?.attempt, 2);
  for (const [name, bad] of [
    ["trade number leaked", shipment({}, [attempt({ merchant_trade_no: "LCABC" })])],
    ["recipient leaked", shipment({}, [attempt({ recipient_name: "x" })])],
    ["missing events", shipment({}, [(({ events, ...r }) => r)(attempt())])],
    ["unknown state", shipment({}, [attempt({ state: "DELIVERED" })])],
    ["current attempt absent", shipment({ current_attempt: 9 })],
    ["null current with attempts", shipment({ current_attempt: null })],
    ["duplicate attempt number", shipment({}, [attempt(), attempt()])],
    ["goods above 20000", shipment({}, [attempt({ goods_amount: 20001 })])],
    ["collection 0", shipment({}, [attempt({ collection_amount: 0 })])],
    ["code with symbol", shipment({}, [attempt({ code: "12;34" })])],
    ["print for a requested attempt", shipment({}, [attempt({ state: "REQUESTED", print_available: true })])],
    ["alert with uppercase", shipment({}, [attempt({ alerts: ["Duplicate"] })])],
    ["event extra key", shipment({}, [attempt({ events: [{ ...event, body: "x" }] })])],
    ["bad time", shipment({}, [attempt({ updated_at: "later" })])],
    ["validity 0", shipment({ validity_days: 0 })],
    ["extra top key", shipment({ token: "x" })],
    ["six attempts", shipment({}, Array.from({ length: 6 }, (_, n) => attempt({ attempt: n + 1 })))],
  ] as const)
    assert.throws(() => parseCvsShipment(bad), /unavailable/, name);
});

test("LGM05 submit, collection and release answers", () => {
  assert.equal(parseCvsSubmitted({ attempt: 1, state: "REQUESTED", operation_id: id }).attempt, 1);
  for (const bad of [{ attempt: 1, state: "CREATED", operation_id: id }, { attempt: 0, state: "REQUESTED", operation_id: id }, { attempt: 1, state: "REQUESTED", operation_id: "x" }, { attempt: 1, state: "REQUESTED", operation_id: id, extra: 1 }])
    assert.throws(() => parseCvsSubmitted(bad), /unavailable/);
  assert.equal(parseCollectionResult({ order_id: id, collection_state: "COLLECTED" }).collection_state, "COLLECTED");
  assert.throws(() => parseCollectionResult({ order_id: id, collection_state: "PAID" }), /unavailable/);
  assert.equal(parseReleaseResult({ order_id: id, collection_state: "CANCELLED", commercial_state: "CANCELLED", released_lines: 2 }).released_lines, 2);
  for (const bad of [{ order_id: id, collection_state: "CANCELLED", commercial_state: "GONE", released_lines: 2 }, { order_id: id, collection_state: "CANCELLED", commercial_state: "CANCELLED", released_lines: -1 }, { order_id: id, collection_state: "CANCELLED", commercial_state: "CANCELLED" }])
    assert.throws(() => parseReleaseResult(bad), /unavailable/);
});

test("LGM06 print form: only the two ECPay logistics hosts and the four documented print paths", () => {
  const form = { action: "https://logistics-stage.ecpay.com.tw/Express/PrintUniMartC2COrderInfo", fields: { MerchantID: "2000933", AllPayLogisticsID: "123", PaymentNo: "1", ValidationNo: "2", CheckMacValue: "ABC" } };
  assert.equal(parsePrintForm(form).action, form.action);
  for (const path of ["Express/PrintFAMIC2COrderInfo", "Express/PrintHILIFEC2COrderInfo", "helper/printTradeDocument"])
    assert.equal(validPrintAction(`https://logistics.ecpay.com.tw/${path}`), true, path);
  for (const [name, action] of [
    ["other host", "https://evil.example.test/Express/PrintUniMartC2COrderInfo"],
    ["lookalike host", "https://logistics.ecpay.com.tw.evil.example.test/Express/PrintUniMartC2COrderInfo"],
    ["http", "http://logistics.ecpay.com.tw/Express/PrintUniMartC2COrderInfo"],
    ["map endpoint", "https://logistics.ecpay.com.tw/Express/map"],
    ["create endpoint", "https://logistics.ecpay.com.tw/Express/Create"],
    ["port", "https://logistics.ecpay.com.tw:8443/Express/PrintUniMartC2COrderInfo"],
    ["userinfo", "https://u:p@logistics.ecpay.com.tw/Express/PrintUniMartC2COrderInfo"],
    ["query", "https://logistics.ecpay.com.tw/Express/PrintUniMartC2COrderInfo?x=1"],
    ["fragment", "https://logistics.ecpay.com.tw/Express/PrintUniMartC2COrderInfo#x"],
    ["trailing path", "https://logistics.ecpay.com.tw/Express/PrintUniMartC2COrderInfo/x"],
  ] as const)
    assert.equal(validPrintAction(action), false, name);
  for (const [name, bad] of [
    ["no fields", { action: form.action, fields: {} }],
    ["non-string field", { action: form.action, fields: { A: 1 } }],
    ["bad field name", { action: form.action, fields: { "a b": "x" } }],
    ["extra key", { ...form, target: "_blank" }],
    ["fields array", { action: form.action, fields: ["x"] }],
  ] as const)
    assert.throws(() => parsePrintForm(bad), /unavailable/, name);
});

test("LGM07 request bodies carry exactly the frozen keys", () => {
  assert.deepEqual(JSON.parse(requestBody(4)), { expected_version: 4 });
  assert.deepEqual(JSON.parse(abandonBody(4, true)), { expected_version: 4, i_checked_ecpay_backend: true });
  assert.deepEqual(JSON.parse(collectionBody("PENDING", "collected")), { expected_state: "PENDING", state: "collected" });
  assert.deepEqual(JSON.parse(releaseBody("cancel", "PENDING")), { action: "cancel", expected_state: "PENDING" });
  assert.deepEqual(JSON.parse(releaseBody("restock", "RETURNED")), { action: "restock", expected_state: "RETURNED" });
});

test("LGM08 pay-at-pickup action hints follow §16.4/§16.8 (SQL stays the authority)", () => {
  const none = { collected: false, returned: false, refunded_offline: false, cancel: false, restock: false };
  assert.deepEqual(collectionActions("PENDING", "MANUAL_UNASSIGNED"), { ...none, cancel: true });
  assert.deepEqual(collectionActions("PENDING", "MERCHANT_SHIPPED"), { ...none, collected: true, returned: true });
  assert.deepEqual(collectionActions("PENDING", "PROVIDER_LABEL_CREATED"), { ...none, collected: true, returned: true });
  assert.deepEqual(collectionActions("COLLECTED", "MERCHANT_SHIPPED"), { ...none, refunded_offline: true });
  assert.deepEqual(collectionActions("RETURNED", "MERCHANT_SHIPPED"), { ...none, restock: true });
  for (const state of ["REFUNDED_OFFLINE", "CANCELLED", "RESTOCKED", null] as const)
    assert.deepEqual(collectionActions(state, "MERCHANT_SHIPPED"), none, String(state));
});

test("LGM09 a CREATED label is offered for abandon only after its validity window", () => {
  const a = parseCvsShipment(shipment()).attempts[0];
  const created = Date.parse(a.created_at);
  assert.equal(labelLapsed(a, 5, created + 5 * 86_400_000), false);
  assert.equal(labelLapsed(a, 5, created + 5 * 86_400_000 + 1), true);
  assert.equal(labelLapsed(a, null, created + 99 * 86_400_000), false);
});
