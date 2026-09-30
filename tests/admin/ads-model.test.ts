// Unit tests for apps/admin/lib/ads-model.ts and ads-copy.ts (pure; no browser, no network).
// Covers: every frozen error code + draft status enum, strict parsers (unknown enum = error), money conversion vectors
// (I05, TWD whole-unit), the draft-input rules of §5.2/U4 and the X7 "no resume" action hints. The browser gate is MA09 (ads-tests).
import assert from "node:assert/strict";
import { test } from "node:test";
import {
  accountReady, adsCodes, adsErrorCode, adsLocalCodes, adsManagerHref, adsServerCodes, adsSessionCodes, buildDraftInput, canApprove,
  canCopy, canEdit, canEnd, canPause, canPublish, capiBody, connectErrors, copyForm, draftStatuses, emptyForm, epochToLocal,
  formatMinor, formFromDraft, minorToWhole, opStates, parseConnectState, parseCountries, parseDraft, parseDraftList, parseReport,
  parseSettings, validCapi, validDate, validReportWindow, validSource, wholeToMinor, AdsParseError, type Draft, type DraftForm,
} from "../../apps/admin/lib/ads-model.ts";
import { adsCopy, errorText } from "../../apps/admin/lib/ads-copy.ts";

const uuid = (n: number) => `00000000-0000-4000-8000-${String(n).padStart(12, "0")}`;
const ts = "2026-10-01T02:03:04Z";

// The Frozen HTTP code list from docs/delivery/units/ads-core.md, copied literally so a silent edit of the model is caught.
const frozenCodes = [
  "state_mismatch", "state_expired", "meta_connect_failed", "not_in_pick_list", "client_business_changed",
  "revision_changed", "over_allowance", "billing_restricted", "attempt_changed", "prior_attempt_not_paused",
  "draft_approved", "budget_below_minimum", "not_whole_unit", "currency_mismatch", "starts_too_soon",
  "binding_disabled", "source_not_owned", "product_not_published", "forbidden", "not_found", "invalid_request",
];
const frozenStatuses = ["DRAFT", "APPROVED", "SUBMITTING", "REMOTE_PAUSED", "ACTIVE", "PAUSED", "UNKNOWN", "FAILED", "ENDED", "REJECTED"];

function draftJSON(over: Record<string, unknown> = {}) {
  return {
    id: uuid(1), revision: 3, publish_attempt: 1, status: "DRAFT", ad_binding_id: uuid(2), identity_binding_id: uuid(3),
    template: "BOOST_POST", source_ref: "123456789_987654321", currency: "TWD", lifetime_budget_minor: 30000,
    starts_at: "2026-10-01T02:00:00Z", ends_at: "2026-10-08T02:00:00Z", countries: ["TW"], age_min: 18, age_max: 65,
    created_at: ts, remote: {}, ops: [], ...over,
  };
}

test("frozen error codes: model list equals the frozen list, each is read from both error shapes", () => {
  assert.deepEqual([...adsServerCodes], frozenCodes);
  for (const code of frozenCodes) {
    assert.equal(adsErrorCode({ code }), code);
    assert.equal(adsErrorCode({ error: code }), code);
  }
  for (const bad of ["", "nope", "STATE_MISMATCH", 5, null, undefined, {}, "retry_later\n"]) assert.equal(adsErrorCode({ code: bad }), "retry_later");
  assert.equal(adsErrorCode("x"), "retry_later");
  assert.equal(adsErrorCode(null), "retry_later");
});

test("copy: every code, status, op state and connect error has text in all three locales", () => {
  assert.deepEqual([...draftStatuses], frozenStatuses);
  for (const locale of ["zh-TW", "zh-CN", "en"] as const) {
    const c = adsCopy[locale];
    for (const code of adsCodes) assert.ok(c.errors[code] && c.errors[code].length > 3, `${locale} errors.${code}`);
    for (const s of frozenStatuses) assert.ok(c.statuses[s as keyof typeof c.statuses], `${locale} status ${s}`);
    for (const s of opStates) assert.ok(c.opStates[s], `${locale} opState ${s}`);
    for (const e of connectErrors) assert.ok(c.connectErrors[e], `${locale} connect ${e}`);
    assert.ok(errorText(c, "definitely_unknown") === c.errors.retry_later);
  }
  // Same keys everywhere (a missing translation is a type error too; this catches a mistyped key).
  const keys = (o: object) => Object.keys(o).sort().join(",");
  assert.equal(keys(adsCopy["zh-TW"]), keys(adsCopy.en));
  assert.equal(keys(adsCopy["zh-CN"]), keys(adsCopy.en));
  assert.deepEqual([...adsCodes], [...adsServerCodes, ...adsSessionCodes, ...adsLocalCodes]);
});

test("copy never calls a budget a hard stop and never offers resume", () => {
  for (const locale of ["zh-TW", "zh-CN", "en"] as const) {
    const c = adsCopy[locale];
    assert.ok(/(不是即時|不是实时|not a real-time)/.test(c.budgetNote), locale);
    assert.ok(/(不是即時|不是实时|not a real-time)/.test(c.reportRefreshNote), locale);
    assert.ok(!/(resume|恢復|恢复|繼續刊登|继续投放)/i.test(JSON.stringify(c)), `${locale} mentions resume`);
  }
});

test("draft: every §5.1 status parses; unknown status/template/op state is an error, never guessed", () => {
  for (const status of frozenStatuses) assert.equal(parseDraft(draftJSON({ status })).status, status);
  assert.throws(() => parseDraft(draftJSON({ status: "RUNNING" })), AdsParseError);
  assert.throws(() => parseDraft(draftJSON({ status: "active" })), AdsParseError);
  assert.throws(() => parseDraft(draftJSON({ template: "BOOST" })), AdsParseError);
  assert.throws(() => parseDraft(draftJSON({ ops: [{ kind: "activate", seq: 1, attempt: 1, state: "MAYBE", updated_at: ts }] })), AdsParseError);
  assert.throws(() => parseDraft(draftJSON({ id: "not-a-uuid" })), AdsParseError);
  assert.throws(() => parseDraft(draftJSON({ lifetime_budget_minor: 1.5 })), AdsParseError);
  assert.throws(() => parseDraft(draftJSON({ lifetime_budget_minor: -1 })), AdsParseError);
  assert.throws(() => parseDraft(draftJSON({ countries: [] })), AdsParseError);
  assert.throws(() => parseDraft(draftJSON({ age_min: 40, age_max: 30 })), AdsParseError);
  assert.throws(() => parseDraft(draftJSON({ starts_at: "yesterday" })), AdsParseError);
  assert.throws(() => parseDraft(null), AdsParseError);
  assert.throws(() => parseDraftList({ items: "x" }), AdsParseError);
  for (const state of opStates) {
    const d = parseDraft(draftJSON({ ops: [{ kind: "meta.ads.pause", seq: 2, attempt: 1, state, code: "credential_unavailable", updated_at: ts }] }));
    assert.equal(d.ops[0].state, state);
  }
});

test("draft: optional fields, remote ids and the Ads Manager link", () => {
  const d = parseDraft(draftJSON({
    approved_revision: 3, ended_at: ts, remote: { campaign_id: "120001", adset_id: "120002", creative_id: null, ad_id: "" },
    ads_manager_url: "https://adsmanager.facebook.com/adsmanager/manage/campaigns?act=1",
  }));
  assert.equal(d.approved_revision, 3);
  assert.equal(d.remote.campaign_id, "120001");
  assert.equal(d.remote.creative_id, null);
  assert.equal(d.remote.ad_id, null);
  assert.ok(d.ads_manager_url?.startsWith("https://adsmanager.facebook.com/"));
  assert.equal(parseDraft(draftJSON({ ads_manager_url: "https://evil.example/x" })).ads_manager_url, null);
  assert.equal(parseDraft(draftJSON({ ads_manager_url: "javascript:alert(1)" })).ads_manager_url, null);
  assert.equal(parseDraft(draftJSON()).approved_revision, null);
  assert.equal(adsManagerHref("http://www.facebook.com/x"), null);
  assert.equal(adsManagerHref("https://facebook.com.evil.example/"), null);
  assert.equal(adsManagerHref("https://user:pw@www.facebook.com/"), null);
  assert.ok(adsManagerHref("https://www.facebook.com/adsmanager"));
  assert.throws(() => parseDraft(draftJSON({ remote: { campaign_id: "a b" } })), AdsParseError);
});

test("action hints: pause whenever a remote id exists and not ENDED; a paused draft is only ever copied (X7)", () => {
  const withRemote = { campaign_id: "1", adset_id: null, creative_id: null, ad_id: null };
  const mk = (status: string, remote = {}) => parseDraft(draftJSON({ status, remote })) as Draft;
  for (const status of frozenStatuses) {
    const d = mk(status, withRemote);
    assert.equal(canPause(d), status !== "ENDED", `pause ${status}`);
    assert.equal(canPause(mk(status)), false, `no remote id => no pause (${status})`);
  }
  assert.equal(canCopy(mk("PAUSED", withRemote)), true);
  assert.equal(canCopy(mk("ACTIVE", withRemote)), false);
  assert.equal(canCopy(mk("DRAFT")), false);
  assert.equal(canEdit(mk("DRAFT")), true);
  assert.equal(canEdit(mk("APPROVED")), false);
  assert.equal(canApprove(mk("DRAFT")), true);
  assert.equal(canPublish(mk("APPROVED")), true);
  assert.equal(canPublish(mk("FAILED")), true);
  assert.equal(canPublish(mk("PAUSED")), false);
  assert.equal(canEnd(mk("ENDED")), false);
  assert.equal(canEnd(mk("DRAFT")), false);
  assert.equal(canEnd(mk("PAUSED", withRemote)), true);
});

test("money: whole units to minor with integer math; TWD must be whole", () => {
  const ok = (cur: string, text: string, minor: number) => assert.deepEqual(wholeToMinor(cur, text), { ok: true, minor }, `${cur} ${text}`);
  const bad = (cur: string, text: string, code: string) => assert.deepEqual(wholeToMinor(cur, text), { ok: false, code }, `${cur} ${JSON.stringify(text)}`);
  ok("TWD", "300", 30000);
  ok("TWD", " 300 ", 30000);
  ok("TWD", "300.00", 30000);
  ok("TWD", "1", 100);
  ok("TWD", "9999999999", 999_999_999_900);
  bad("TWD", "300.5", "not_whole_unit");
  bad("TWD", "300.01", "not_whole_unit");
  bad("TWD", "300.505", "not_whole_unit");
  bad("TWD", "0", "budget_below_minimum");
  bad("TWD", "0.00", "budget_below_minimum");
  bad("TWD", "", "budget_invalid");
  bad("TWD", "-5", "budget_invalid");
  bad("TWD", "+5", "budget_invalid");
  bad("TWD", "1e3", "budget_invalid");
  bad("TWD", "1,000", "budget_invalid");
  bad("TWD", "٣٠٠", "budget_invalid");
  bad("TWD", "300.", "budget_invalid");
  bad("TWD", ".5", "budget_invalid");
  bad("TWD", "12345678901", "budget_invalid");
  bad("TWD", "NaN", "budget_invalid");
  ok("USD", "12.50", 1250);
  ok("USD", "12.5", 1250);
  ok("USD", "12", 1200);
  ok("USD", "0.01", 1);
  ok("HKD", "99.99", 9999);
  bad("USD", "12.345", "budget_invalid");
  bad("USD", "0", "budget_below_minimum");
  bad("EUR", "10", "unsupported_currency");
  bad("JPY", "10", "unsupported_currency");
  assert.equal(minorToWhole(30000), "300");
  assert.equal(minorToWhole(1250), "12.50");
  assert.equal(minorToWhole(5), "0.05");
  for (const minor of [100, 30000, 1250, 1, 999_999_999_999]) {
    assert.deepEqual(wholeToMinor("USD", minorToWhole(minor)), { ok: true, minor });
  }
  assert.ok(formatMinor("en", "TWD", 30000).includes("300"));
  assert.ok(!formatMinor("en", "TWD", 30000).includes("30,000"));
  assert.ok(formatMinor("en", "USD", 1250).includes("12.50"));
});

const now = new Date(2026, 8, 30, 12, 0, 0).getTime(); // local wall time
const min = 60_000;
const day = 86_400_000;
function form(over: Partial<DraftForm> = {}): DraftForm {
  return {
    ad_binding_id: uuid(2), identity_binding_id: uuid(3), template: "BOOST_POST", source_ref: "123456789_987654321", currency: "TWD",
    budget: "300", starts_local: epochToLocal(now + 11 * min), ends_local: epochToLocal(now + 2 * day), countries: "TW", age_min: "18", age_max: "65", ...over,
  };
}
const codesOf = (f: DraftForm, allowance = "TWD") => {
  const r = buildDraftInput(f, now, allowance);
  return r.ok ? [] : r.codes;
};

test("draft input: exact 11-key body and every local rule", () => {
  const r = buildDraftInput(form(), now, "TWD");
  assert.ok(r.ok);
  if (!r.ok) return;
  assert.deepEqual(Object.keys(r.input), [
    "ad_binding_id", "identity_binding_id", "template", "source_ref", "currency", "lifetime_budget_minor", "starts_at", "ends_at",
    "countries", "age_min", "age_max",
  ]);
  assert.equal(r.input.lifetime_budget_minor, 30000);
  assert.match(r.input.starts_at, /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:00Z$/);
  assert.equal(Date.parse(r.input.starts_at), now + 11 * min - ((now + 11 * min) % min));
  assert.deepEqual(codesOf(form({ starts_local: epochToLocal(now + 9 * min) })), ["starts_too_soon"]);
  assert.deepEqual(codesOf(form({ starts_local: epochToLocal(now + 10 * min) })), []);
  assert.deepEqual(codesOf(form({ starts_local: epochToLocal(now + 3 * day), ends_local: epochToLocal(now + 2 * day) })), ["ends_before_start"]);
  assert.deepEqual(codesOf(form({ starts_local: epochToLocal(now + 1 * day), ends_local: epochToLocal(now + 1 * day + 30 * day + min) })), ["duration_too_long"]);
  assert.deepEqual(codesOf(form({ starts_local: epochToLocal(now + 1 * day), ends_local: epochToLocal(now + 1 * day + 30 * day) })), []);
  assert.deepEqual(codesOf(form({ starts_local: "" })), ["dates_invalid"]);
  assert.deepEqual(codesOf(form({ budget: "300.5" })), ["not_whole_unit"]);
  assert.deepEqual(codesOf(form({ budget: "0" })), ["budget_below_minimum"]);
  assert.deepEqual(codesOf(form({ currency: "USD", budget: "12.5" }), "TWD"), ["currency_mismatch"]);
  assert.deepEqual(codesOf(form({ currency: "USD", budget: "12.5" }), "USD"), []);
  assert.deepEqual(codesOf(form(), ""), []);
  assert.deepEqual(codesOf(form({ age_min: "17" })), ["age_invalid"]);
  assert.deepEqual(codesOf(form({ age_max: "66" })), ["age_invalid"]);
  assert.deepEqual(codesOf(form({ age_min: "40", age_max: "30" })), ["age_invalid"]);
  assert.deepEqual(codesOf(form({ age_min: "1e" })), ["age_invalid"]);
  assert.deepEqual(codesOf(form({ countries: "TW,tw" })), ["countries_invalid"]);
  assert.deepEqual(codesOf(form({ countries: "TWN" })), ["countries_invalid"]);
  assert.deepEqual(codesOf(form({ ad_binding_id: "" })), ["bindings_missing"]);
  assert.deepEqual(codesOf(form({ source_ref: "abc" })), ["source_invalid"]);
  assert.deepEqual(codesOf(form({ template: "PRODUCT_TRAFFIC", source_ref: "prod-1" })), []);
  assert.deepEqual(codesOf(form({ template: "PRODUCT_TRAFFIC", source_ref: "has space" })), ["source_invalid"]);
  const many = codesOf(form({ ad_binding_id: "", budget: "x", age_min: "5" }));
  assert.deepEqual(many.sort(), ["age_invalid", "bindings_missing", "budget_invalid"]);
  // sources / countries helpers
  assert.equal(validSource("BOOST_POST", "12345_67890"), true);
  assert.equal(validSource("BOOST_POST", "12345"), true);
  assert.equal(validSource("BOOST_POST", "1234"), false);
  assert.deepEqual(parseCountries("tw, jp"), ["TW", "JP"]);
  assert.equal(parseCountries(""), null);
});

test("draft form round trip and X7 copy (blank schedule, no id, no revision)", () => {
  const d = parseDraft(draftJSON({ status: "PAUSED", remote: { campaign_id: "1" } }));
  const f = formFromDraft(d);
  assert.equal(f.budget, "300");
  assert.equal(f.countries, "TW");
  const c = copyForm(d);
  assert.equal(c.starts_local, "");
  assert.equal(c.ends_local, "");
  assert.equal(c.source_ref, d.source_ref);
  assert.ok(!("id" in c) && !("revision" in c));
  assert.equal(emptyForm("TWD").countries, "TW");
  assert.equal(emptyForm("TWD").age_min, "18");
});

test("settings parser: environment enum, connections, identities, capi", () => {
  const good = {
    environment: "SANDBOX", allowance_currency: "TWD", max_active_budget_minor: 0,
    capi: { enabled: false }, connections: [{ binding_id: uuid(2), provider: "meta_ads", asset_id: "act_1", client_business_id: "9", enabled: true, connected_at: ts }],
    identities: [{ binding_id: uuid(3), provider: "facebook", asset_id: "555" }],
  };
  const s = parseSettings(good);
  assert.equal(s.max_active_budget_minor, 0);
  assert.equal(s.capi.dataset_binding_id, null);
  assert.equal(s.sandbox_ad_account, null);
  assert.equal(parseSettings({ ...good, environment: "LIVE", sandbox_ad_account: "act_9", capi: { enabled: true, dataset_binding_id: uuid(4), test_event_code: "TEST1" } }).capi.test_event_code, "TEST1");
  assert.throws(() => parseSettings({ ...good, environment: "PROD" }), AdsParseError);
  assert.throws(() => parseSettings({ ...good, max_active_budget_minor: "0" }), AdsParseError);
  assert.throws(() => parseSettings({ ...good, connections: [{}] }), AdsParseError);
  assert.throws(() => parseSettings({ ...good, capi: null }), AdsParseError);
  assert.throws(() => parseSettings({ ...good, identities: [{ binding_id: "x", provider: "facebook", asset_id: "1" }] }), AdsParseError);
});

test("connect state parser: pick list kinds and id match", () => {
  const good = {
    state_id: uuid(7), expires_at: ts, client_business_id: "77",
    picks: [
      { kind: "ad_account", id: "act_1", name: "Shop", currency: "TWD", timezone: "Asia/Taipei", account_status: 1 },
      { kind: "dataset", id: "d1", name: "Pixel" },
    ],
  };
  const s = parseConnectState(good, uuid(7));
  assert.equal(s.picks.length, 2);
  assert.equal(accountReady(s.picks[0].account_status), true);
  assert.equal(accountReady(2), false);
  assert.throws(() => parseConnectState(good, uuid(8)), AdsParseError);
  assert.throws(() => parseConnectState({ ...good, picks: [{ kind: "page", id: "1", name: "x" }] }, uuid(7)), AdsParseError);
});

test("report: three separate blocks, orders may be null, no combined figure", () => {
  const base = {
    window: { from: "2026-09-24", to: "2026-09-30" }, timezone: "Asia/Taipei",
    orders: { captured_minor: 100000, refunded_minor: 20000, net_minor: 80000, currency: "TWD", note: "card_payments_only", fetched_at: ts },
    meta_delivery: { spend_minor: 30000, impressions: 1200, clicks: 44, currency: "TWD", account_timezone: "Asia/Taipei", fetched_at: ts, final_through: "2026-09-27" },
    meta_reported: { purchases: 3, purchase_value_minor: 90000, currency: "TWD", fetched_at: ts },
  };
  const r = parseReport(base);
  assert.equal(r.orders?.net_minor, 80000);
  assert.deepEqual(Object.keys(r).sort(), ["meta_delivery", "meta_reported", "orders", "timezone", "window"]);
  assert.equal(parseReport({ ...base, orders: null }).orders, null);
  assert.equal(parseReport({ ...base, meta_delivery: { ...base.meta_delivery, fetched_at: null, final_through: null } }).meta_delivery.fetched_at, null);
  assert.throws(() => parseReport({ ...base, window: { from: "2026-02-30", to: "2026-03-01" } }), AdsParseError);
  assert.throws(() => parseReport({ ...base, meta_delivery: { ...base.meta_delivery, spend_minor: 1.5 } }), AdsParseError);
  assert.throws(() => parseReport({ ...base, meta_reported: undefined }), AdsParseError);
  assert.equal(validReportWindow("2026-09-01", "2026-09-30"), true);
  assert.equal(validReportWindow("2026-09-30", "2026-09-01"), false);
  assert.equal(validReportWindow("2026-01-01", "2026-04-02"), true); // 92 days inclusive
  assert.equal(validReportWindow("2026-01-01", "2026-04-03"), false);
  assert.equal(validDate("2026-02-29"), false);
  assert.equal(validDate("2024-02-29"), true);
});

test("capi body and validation", () => {
  assert.deepEqual(capiBody(true, uuid(4), " TEST1 "), { enabled: true, dataset_binding_id: uuid(4), test_event_code: "TEST1" });
  assert.deepEqual(capiBody(false, "", ""), { enabled: false, dataset_binding_id: null, test_event_code: null });
  assert.equal(validCapi(true, "", ""), false);
  assert.equal(validCapi(false, "", ""), true);
  assert.equal(validCapi(true, uuid(4), "TEST_1-a"), true);
  assert.equal(validCapi(true, uuid(4), "bad code"), false);
  assert.equal(validCapi(false, "junk", ""), false);
});
