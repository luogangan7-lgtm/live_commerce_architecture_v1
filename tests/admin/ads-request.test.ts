// Unit tests for apps/admin/lib/ads-request.ts (pure): the BFF route grammar for the frozen ads resources, the report
// query rule, and the helpers of the dedicated connect/callback routes (cookie, dialog origin, callback parameter bounds).
// The browser/BFF gate is MA09 (ads-tests); this file only proves the grammar and bounds.
import assert from "node:assert/strict";
import { test } from "node:test";
import {
  adsAny, adsBodyless, adsKeyless, adsRoutes, callbackRedirect, clearConnectCookie, connectCookie, connectErrorFor, localeFromCookie, parseCallbackQuery,
  parseConnectBody, safeDialogURL, storeFromCookie, validAdsQuery, validIdempotencyKey, validIfMatch,
} from "../../apps/admin/lib/ads-request.ts";

const uuid = "abcdefab-1111-4111-8111-11111111abcd"; // has hex letters so the upper-case negative cases are real
const other = "22222222-2222-4222-8222-2222222222ef";
const method = (m: "GET" | "POST" | "PUT") => new RegExp(`^(?:${adsRoutes[m]})$`);

test("route grammar: exactly the frozen resources except meta/connect and meta/callback", () => {
  const get = method("GET");
  for (const ok of ["ads/settings", "ads/report", "ads/drafts", `ads/drafts/${uuid}`, `ads/meta/states/${uuid}`]) assert.ok(get.test(ok), ok);
  for (const bad of [
    "ads", "ads/", "ads/meta/connect", "ads/meta/callback", "ads/capi", `ads/drafts/${uuid}/approve`, "ads/drafts/../settings",
    `ads/drafts/${uuid}/`, "ads/drafts/abc", `ads/meta/states/${uuid}/x`, "ads/settings/x", "ads/report/x", "ADS/settings", `ads/drafts/${uuid.toUpperCase()}`,
    "ads/meta/states/abc", "ads/drafts%2fx",
  ]) assert.ok(!get.test(bad), bad);
  const post = method("POST");
  for (const ok of ["ads/meta/bindings", "ads/drafts", ...["approve", "publish", "pause", "end"].map((a) => `ads/drafts/${uuid}/${a}`)]) assert.ok(post.test(ok), ok);
  for (const bad of ["ads/meta/connect", "ads/meta/callback", "ads/capi", `ads/drafts/${uuid}`, `ads/drafts/${uuid}/resume`, `ads/drafts/${uuid}/activate`,
    `ads/drafts/${uuid}/approve/x`, "ads/settings", "ads/report"]) assert.ok(!post.test(bad), bad);
  const put = method("PUT");
  for (const ok of [`ads/drafts/${uuid}`, "ads/capi"]) assert.ok(put.test(ok), ok);
  for (const bad of ["ads/drafts", `ads/drafts/${uuid}/approve`, "ads/settings", "ads/meta/bindings", `ads/drafts/${uuid}/`]) assert.ok(!put.test(bad), bad);
  assert.ok(adsAny.test(`ads/drafts/${uuid}/pause`));
  assert.ok(!adsAny.test("ads/meta/connect"));
  // No DELETE-style verbs and no resume: nothing in the grammar can re-activate a paused draft (X7).
  assert.ok(!/resume|activate/.test(`${adsRoutes.GET}${adsRoutes.POST}${adsRoutes.PUT}`));
});

test("query rule: only report has a query, from+to exactly once, real dates, at most 92 days", () => {
  const base = `http://127.0.0.1:3100/api/stores/${uuid}/`;
  assert.equal(validAdsQuery(`${base}ads/report?from=2026-09-01&to=2026-09-30`, "ads/report"), true);
  assert.equal(validAdsQuery(`${base}ads/report?to=2026-09-30&from=2026-09-01`, "ads/report"), true);
  assert.equal(validAdsQuery(`${base}ads/report?from=2026-01-01&to=2026-04-02`, "ads/report"), true);
  for (const q of ["", "?", "?from=2026-09-01", "?to=2026-09-30", "?from=2026-09-01&to=2026-09-30&", "?from=2026-09-01&from=2026-09-02&to=2026-09-30",
    "?from=2026-09-30&to=2026-09-01", "?from=2026-01-01&to=2026-04-03", "?from=2026-02-30&to=2026-03-01", "?from=2026-9-1&to=2026-09-30",
    "?from=%32026-09-01&to=2026-09-30", "?from=2026-09-01&to=2026-09-30&x=1", "?limit=1"])
    assert.equal(validAdsQuery(`${base}ads/report${q}`, "ads/report"), false, q);
  for (const path of ["ads/settings", "ads/drafts", `ads/drafts/${uuid}`, `ads/meta/states/${uuid}`, "ads/capi"]) {
    assert.equal(validAdsQuery(`${base}${path}`, path), true, path);
    assert.equal(validAdsQuery(`${base}${path}?`, path), false, `${path}?`);
    assert.equal(validAdsQuery(`${base}${path}?x=1`, path), false, `${path}?x=1`);
  }
});

test("headers: If-Match is a bare revision, Idempotency-Key is the repo grammar", () => {
  for (const v of ["0", "1", "42", "1000000000"]) assert.equal(validIfMatch(v), true, v);
  for (const v of [null, "", "01", "-1", "1.5", '"3"', "W/3", "12345678901", "abc"]) assert.equal(validIfMatch(v), false, String(v));
  assert.equal(validIdempotencyKey("ads-0f1d2c3b-aaaa-4bbb-8ccc-1234567890ab"), true);
  for (const v of ["", "short", "has space here", "x".repeat(129), "bad/key1234"]) assert.equal(validIdempotencyKey(v), false, v);
});

test("connect cookie: httpOnly, Secure, Lax, Path is the callback only, 10 minutes; cleared with Max-Age=0", () => {
  const set = connectCookie(uuid);
  assert.equal(set, `lc_ads_connect=${uuid}; Path=/api/ads/meta/callback; Secure; HttpOnly; SameSite=Lax; Max-Age=600`);
  assert.ok(clearConnectCookie().startsWith("lc_ads_connect=; Path=/api/ads/meta/callback;"));
  assert.ok(clearConnectCookie().includes("Max-Age=0"));
  assert.equal(storeFromCookie(`a=b; lc_ads_connect=${uuid}; c=d`), uuid);
  assert.equal(storeFromCookie(`lc_ads_connect=${uuid}; lc_ads_connect=${other}`), null);
  for (const v of [null, "", "lc_ads_connect=", "lc_ads_connect=not-a-uuid", `lc_ads_connect=${uuid.toUpperCase()}`, `x_lc_ads_connect=${uuid}`]) assert.equal(storeFromCookie(v), null, String(v));
});

test("connect body: exactly {store: uuid}", () => {
  assert.equal(parseConnectBody(JSON.stringify({ store: uuid })), uuid);
  for (const v of ["", "{}", "[]", "null", JSON.stringify({ store: "x" }), JSON.stringify({ store: uuid, extra: 1 }), JSON.stringify({ store: 5 }), "{"]) assert.equal(parseConnectBody(v), null, v);
});

test("dialog URL: only exactly https://www.facebook.com", () => {
  assert.ok(safeDialogURL("https://www.facebook.com/v26.0/dialog/oauth?client_id=1&state=abc"));
  for (const v of [
    "http://www.facebook.com/x", "https://facebook.com/x", "https://www.facebook.com.evil.example/x", "https://evil.example/https://www.facebook.com",
    "https://www.facebook.com:8443/x", "https://user:pw@www.facebook.com/x", "//www.facebook.com/x", "javascript:alert(1)", "https://www.facebook.com/x#f",
    "", null, undefined, 5, "https://www.facebook.com/" + "a".repeat(5000),
  ]) assert.equal(safeDialogURL(v), null, String(v).slice(0, 60));
});

test("callback parameters: bounded charset/length, duplicates rejected, Meta cancel is denied, extras ignored", () => {
  const state = "A".repeat(43);
  const code = "AQD-x_y.z~1";
  assert.deepEqual(parseCallbackQuery(`?code=${code}&state=${state}`), { kind: "ok", code, state });
  assert.deepEqual(parseCallbackQuery(`?state=${state}&code=${code}&granted_scopes=ads_management`), { kind: "ok", code, state });
  assert.deepEqual(parseCallbackQuery(`?code=${"a".repeat(2048)}&state=${state}`), { kind: "ok", code: "a".repeat(2048), state });
  assert.deepEqual(parseCallbackQuery(`?state=${state}&error=access_denied&error_reason=user_denied`), { kind: "denied" });
  for (const q of [
    "", `?state=${state}`, `?code=${code}`, `?code=&state=${state}`, `?code=${code}&state=`,
    `?code=${"a".repeat(2049)}&state=${state}`, `?code=${code}&state=${"A".repeat(129)}`, `?code=${code}&state=${"A".repeat(15)}`,
    `?code=${code}&code=${code}&state=${state}`, `?code=${code}&state=${state}&state=${state}`, `?code=a b&state=${state}`,
    `?code=${code}%00&state=${state}`, `?code=${code}&state=${state}%0d`, `?code=%3Cscript%3E&state=${state}`, `?code=${code}&state=${state}/../x`,
    `?code=a&error=1&error=2&state=${state}`,
  ]) assert.equal(parseCallbackQuery(q).kind, "invalid", q);
});

test("callback redirect: only fixed codes and canonical uuids ever reach the URL (never code/state)", () => {
  assert.equal(callbackRedirect("zh-TW", uuid, { connect: other }), `/zh-TW/ads?store=${uuid}&connect=${other}`);
  assert.equal(callbackRedirect("en", uuid, { error: "state_expired" }), `/en/ads?store=${uuid}&connect_error=state_expired`);
  assert.equal(callbackRedirect("zh-CN", null, { error: "state_mismatch" }), "/zh-CN/ads?connect_error=state_mismatch");
  assert.equal(callbackRedirect("en", uuid, null), `/en/ads?store=${uuid}`);
  assert.equal(callbackRedirect("en", null, null), "/en/ads");
  // A non-uuid state id or an error string with unsafe characters is dropped, not concatenated.
  assert.equal(callbackRedirect("en", uuid, { connect: "x&code=1" }), `/en/ads?store=${uuid}`);
  assert.equal(callbackRedirect("en", uuid, { error: "a&b=1" }), `/en/ads?store=${uuid}`);
  assert.equal(localeFromCookie("commerce_locale=en"), "en");
  assert.equal(localeFromCookie("x=1; commerce_locale=zh-CN"), "zh-CN");
  assert.equal(localeFromCookie("commerce_locale=fr"), "zh-TW");
  assert.equal(localeFromCookie(null), "zh-TW");
});

test("callback Go answers map to the fixed connect_error values", () => {
  assert.equal(connectErrorFor(409, { error: "state_mismatch" }), "state_mismatch");
  assert.equal(connectErrorFor(410, { code: "state_expired" }), "state_expired");
  assert.equal(connectErrorFor(502, { error: "meta_connect_failed" }), "meta_connect_failed");
  assert.equal(connectErrorFor(409, {}), "state_mismatch");
  assert.equal(connectErrorFor(410, null), "state_expired");
  assert.equal(connectErrorFor(502, null), "meta_connect_failed");
  assert.equal(connectErrorFor(401, { error: "unauthorized" }), null);
  assert.equal(connectErrorFor(403, { error: "forbidden" }), "forbidden");
  assert.equal(connectErrorFor(404, null), "forbidden");
  assert.equal(connectErrorFor(422, null), "invalid_request");
  assert.equal(connectErrorFor(503, null), "unavailable");
  assert.equal(connectErrorFor(500, { error: "over_allowance" }), "unavailable");
});

test("Go transport split: pause/end bodiless, approve keyless, nothing else", () => {
  for (const a of ["pause", "end"]) assert.ok(adsBodyless.test(`ads/drafts/${uuid}/${a}`), a);
  assert.ok(adsKeyless.test(`ads/drafts/${uuid}/approve`));
  for (const bad of [`ads/drafts/${uuid}/approve`, `ads/drafts/${uuid}/publish`, `ads/drafts/${uuid}`, "ads/capi", `ads/drafts/${uuid}/pause/x`])
    assert.ok(!adsBodyless.test(bad), bad);
  for (const bad of [`ads/drafts/${uuid}/publish`, `ads/drafts/${uuid}/pause`, "ads/drafts", `ads/drafts/${uuid}/approve/x`])
    assert.ok(!adsKeyless.test(bad), bad);
});
