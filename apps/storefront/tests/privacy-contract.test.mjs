// customers-billing-ui: the buyer privacy wire contract (lib/privacy-contract.ts) and its copy (lib/privacy-copy.ts).
// Pure: the BFF wiring (buyer-server.ts hook) and the real Go chain are proved by customers-billing-tests (CB09/CB11).
import test from "node:test";
import assert from "node:assert/strict";
import {
  LC_PRIVACY_POLICY_VERSION,
  consentBody,
  consentPairs,
  erasureBody,
  privacyBodies,
  privacyFailure,
  privacyRoutes,
  validBuyerExport,
  validBuyerPrivacy,
  validConsentResult,
  validErasureSummary,
} from "../lib/privacy-contract.ts";
import { privacyCopy } from "../lib/privacy-copy.ts";

test("policy version is the frozen constant, in the customers-core D4 shape", () => {
  assert.equal(LC_PRIVACY_POLICY_VERSION, "lc-2026-10");
  assert.match(LC_PRIVACY_POLICY_VERSION, /^lc-\d{4}-\d{2}$/);
});

test("route table: exactly the four buyer privacy routes, methods and body kinds", () => {
  assert.deepEqual(Object.keys(privacyRoutes).sort(), ["consents", "privacy", "privacy/erasure", "privacy/export"]);
  assert.deepEqual(privacyRoutes.privacy, { GET: { privatePath: "privacy" } });
  assert.deepEqual(privacyRoutes.consents, { PUT: { privatePath: "consents", body: "consent" } });
  assert.deepEqual(privacyRoutes["privacy/export"], { POST: { privatePath: "privacy/export", body: "empty" } });
  assert.deepEqual(privacyRoutes["privacy/erasure"], { POST: { privatePath: "privacy/erasure", body: "erasure" } });
  // Every non-GET route names a body shape; the shape names exist (or are the BFF's built-in "empty").
  for (const methods of Object.values(privacyRoutes))
    for (const [method, route] of Object.entries(methods))
      if (method !== "GET") assert.ok(route.body === "empty" || Object.hasOwn(privacyBodies, route.body), route.privatePath);
});

test("body shapes are closed and mirror the frozen Go inputs (no source, no policy_version)", () => {
  assert.deepEqual(Object.keys(privacyBodies.consent).sort(), ["channel", "context", "granted", "purpose"]);
  assert.deepEqual(privacyBodies.erasure, { confirm: "string" });
  assert.deepEqual(consentBody("marketing_messages", "meta_dm", true, "checkout"),
    { purpose: "marketing_messages", channel: "meta_dm", granted: true, context: "checkout" });
  assert.deepEqual(consentBody("ads_personalization", "meta_ads", false, "settings"),
    { purpose: "ads_personalization", channel: "meta_ads", granted: false, context: "settings" });
  assert.deepEqual(erasureBody, { confirm: "ERASE" });
  assert.deepEqual(consentPairs.map((p) => `${p.purpose}/${p.channel}`), ["marketing_messages/meta_dm", "ads_personalization/meta_ads"]);
});

const privacy = (over = {}) => ({ consents: { marketing_messages: true, ads_personalization: false }, erased: false, ...over });
test("GET privacy validator: closed shape, erased owner holds no consent", () => {
  assert.equal(validBuyerPrivacy(privacy()), true);
  assert.equal(validBuyerPrivacy(privacy({ erased: true, consents: { marketing_messages: false, ads_personalization: false } })), true);
  for (const bad of [
    privacy({ erased: true }), privacy({ extra: 1 }), privacy({ erased: "no" }), privacy({ consents: { marketing_messages: true } }),
    privacy({ consents: { marketing_messages: 1, ads_personalization: false } }), null, [], "x", { consents: {}, erased: false },
  ])
    assert.equal(validBuyerPrivacy(bad), false, JSON.stringify(bad));
});

test("consent result and erasure summary validators", () => {
  const result = { purpose: "ads_personalization", channel: "meta_ads", granted: true, occurred_at: "2026-09-30T01:02:03.123456Z" };
  assert.equal(validConsentResult(result), true);
  for (const bad of [
    { ...result, channel: "meta_dm" }, { ...result, granted: "true" }, { ...result, occurred_at: "yesterday" },
    { ...result, source: "buyer_checkout" }, { purpose: "ads_personalization" },
  ])
    assert.equal(validConsentResult(bad), false, JSON.stringify(bad));
  const summary = { consents_withdrawn: 2, sessions_revoked: 1, snapshots_redacted: 3, bundles_relabelled: 0 };
  const envelope = (over = {}) => ({ erased: true, orders_retained: true, summary, ...over });
  assert.equal(validErasureSummary(envelope()), true);
  const badSummaries = [{ ...summary, extra: 1 }, { ...summary, sessions_revoked: -1 }, { ...summary, snapshots_redacted: 1.5 }, {}];
  for (const bad of [
    summary, // the old flat shape
    envelope({ erased: false }), envelope({ orders_retained: false }), envelope({ extra: 1 }),
    { erased: true, orders_retained: true },
    ...badSummaries.map((s) => envelope({ summary: s })),
  ])
    assert.equal(validErasureSummary(bad), false, JSON.stringify(bad));
});

test("buyer export envelope never carries customer_id", () => {
  const doc = { format: "lc.customer-export.v1", generated_at: "2026-09-30T00:00:00Z", store: { name: "Shop" },
    orders: [], consents: [], claims: [], privacy_actions: [] };
  assert.equal(validBuyerExport(doc), true);
  assert.equal(validBuyerExport({ ...doc, customer_id: "11111111-1111-4111-8111-111111111111" }), false);
  assert.equal(validBuyerExport({ ...doc, format: "lc.customer-export.v2" }), false);
  assert.equal(validBuyerExport({ ...doc, orders: {} }), false);
  assert.equal(validBuyerExport(null), false);
});

test("failure classification: 410 and erased are the erased state", () => {
  assert.equal(privacyFailure(410, ""), "erased");
  assert.equal(privacyFailure(409, "erased"), "erased");
  assert.equal(privacyFailure(409, "erasure_blocked"), "erasure_blocked");
  assert.equal(privacyFailure(409, "export_too_large"), "export_too_large");
  assert.equal(privacyFailure(409, "idempotency_conflict"), "idempotency_conflict");
  assert.equal(privacyFailure(500, ""), "other");
});

test("copy: every locale has the same keys, the ads box discloses that sent events cannot be recalled", () => {
  const keys = (locale) => Object.keys(privacyCopy[locale]).sort().join(",");
  assert.equal(keys("zh-CN"), keys("en"));
  assert.equal(keys("zh-TW"), keys("en"));
  for (const locale of ["en", "zh-CN", "zh-TW"]) {
    const copy = privacyCopy[locale];
    assert.equal(copy.noticeItems.length, privacyCopy.en.noticeItems.length, locale);
    assert.equal(copy.deletionSteps.length, privacyCopy.en.deletionSteps.length, locale);
    assert.ok(copy.noticeVersion(LC_PRIVACY_POLICY_VERSION).includes(LC_PRIVACY_POLICY_VERSION), locale);
    assert.ok(copy.eraseKept.length > 20 && copy.deletionKept.length > 20, locale);
  }
  assert.match(privacyCopy.en.adsDisclosure, /cannot be recalled/);
  assert.match(privacyCopy["zh-CN"].adsDisclosure, /无法撤回/);
  assert.match(privacyCopy["zh-TW"].adsDisclosure, /無法撤回/);
});
