// Comment source pure contracts (claim-source HTTP interface): BFF grammar for exactly
// GET/PUT claim-source, the pasted-input validator, the five-key PUT body (+ optional platform), the closed
// response parser, and three-locale copy completeness for every refusal code.
import assert from "node:assert/strict";
import { test } from "node:test";
import { claimSourceBody, claimSourceInputMax, claimsRoutes, claimsSubpath, validClaimSourceInput } from "../../apps/admin/lib/claims-request.ts";
import { parseClaimSource, parseClaimSourceEnvelope } from "../../apps/admin/lib/claim-source-model.ts";
import { claimsCopy } from "../../apps/admin/lib/claims-copy.ts";

const session = "live-sessions/11111111-1111-4111-8111-111111111111";
const other = "22222222-2222-4222-8222-222222222222";
const wire = {
  id: other, platform: "facebook", object: "page", asset_id: "page-asset-1", source_object_id: "123456789012345",
  private_reply: true, reply_locale: "zh-TW", active: true, version: 1, verified: false, intake_count: 4, intake_capped: 1,
  updated_at: "2026-09-29T08:30:00Z",
};

test("claim-source BFF allowlist is exactly GET and PUT on the scene", () => {
  const matches = (method: keyof typeof claimsRoutes, path: string) => new RegExp(`^${claimsRoutes[method]}$`).test(path);
  assert.ok(matches("GET", `${session}/claim-source`));
  assert.ok(matches("PUT", `${session}/claim-source`));
  assert.ok(new RegExp(`^live-sessions/[0-9a-f-]{36}/${claimsSubpath}$`).test(`${session}/claim-source`));
  for (const path of [`${session}/claim-source/`, `${session}/claim-source/${other}`, `${session}/claim-sources`,
    `live-sessions/AAAAAAAA-2222-4222-8222-222222222222/claim-source`, `${session}/claims/claim-source`, `${session}/claim-source?x=1`])
    for (const method of ["GET", "PUT"] as const) assert.equal(matches(method, path), false, `${method} ${path}`);
  for (const method of ["POST", "PATCH"] as const) assert.equal(matches(method, `${session}/claim-source`), false, method);
  // The old claims routes stay reachable and PUT reaches nothing else.
  assert.ok(matches("GET", `${session}/claims`) && matches("GET", `${session}/claims/bundles`));
  assert.equal(matches("PUT", `${session}/claims`), false);
});

test("pasted input: one link or id, trimmed, no whitespace or controls, bounded", () => {
  for (const ok of ["123456789012345", "  123  ", "https://www.facebook.com/page/posts/123", "https://www.instagram.com/reel/Cabc_-123/?igsh=x"])
    assert.equal(validClaimSourceInput(ok), true, ok);
  for (const bad of ["", "   ", "two words", "a\tb", "a\nb", "a\u0000b", "a\u2003b", "x".repeat(claimSourceInputMax + 1)])
    assert.equal(validClaimSourceInput(bad), false, JSON.stringify(bad));
  assert.equal(validClaimSourceInput("x".repeat(claimSourceInputMax)), true);
});

test("PUT body is exactly five keys with the trimmed input, plus platform only when chosen (ruling p)", () => {
  const body = claimSourceBody({ input: " 123 ", private_reply: false, reply_locale: "en", active: true, platform: "" }, 0);
  assert.equal(JSON.stringify(body), '{"input":"123","private_reply":false,"reply_locale":"en","active":true,"expected_version":0}');
  const hinted = claimSourceBody({ input: "123", private_reply: false, reply_locale: "en", active: false, platform: "instagram" }, 3);
  assert.equal(JSON.stringify(hinted), '{"input":"123","private_reply":false,"reply_locale":"en","active":false,"expected_version":3,"platform":"instagram"}');
});

test("source response parser is closed", () => {
  assert.deepEqual(parseClaimSource(wire), wire);
  assert.deepEqual(parseClaimSourceEnvelope({ source: null, platforms: [] }), { source: null, platforms: [] });
  assert.deepEqual(parseClaimSourceEnvelope({ source: wire, platforms: ["facebook", "instagram"] }), { source: wire, platforms: ["facebook", "instagram"] });
  assert.deepEqual(parseClaimSourceEnvelope({ source: null, platforms: ["instagram"] }).platforms, ["instagram"]);
  assert.deepEqual(parseClaimSource({ ...wire, platform: "instagram", object: "instagram", verified: true }).platform, "instagram");
  const { id: _id, ...missing } = wire;
  for (const bad of [
    { ...wire, extra: 1 }, missing, { ...wire, platform: "tiktok" }, { ...wire, object: "instagram" },
    { ...wire, platform: "instagram", object: "page" }, { ...wire, id: "nope" }, { ...wire, version: 0 },
    { ...wire, version: 1.5 }, { ...wire, intake_count: -1 }, { ...wire, intake_capped: "1" }, { ...wire, private_reply: "yes" },
    { ...wire, reply_locale: "fr" }, { ...wire, source_object_id: "" }, { ...wire, source_object_id: "a b" },
    { ...wire, asset_id: "" }, { ...wire, updated_at: "soon" }, { ...wire, verified: null },
  ]) assert.throws(() => parseClaimSource(bad), /invalid_claim_source_response/, JSON.stringify(bad));
  for (const bad of [null, [], {}, { source: undefined }, { source: null }, { source: null, platforms: [], extra: 1 }, { source: [], platforms: [] },
    { source: null, platforms: null }, { source: null, platforms: ["tiktok"] }, { source: null, platforms: ["instagram", "facebook"] },
    { source: null, platforms: ["facebook", "facebook"] }, { source: null, platforms: "facebook" }])
    assert.throws(() => parseClaimSourceEnvelope(bad), /invalid_claim_source_response/, JSON.stringify(bad));
});

test("every refusal code and source label is worded in all three locales", () => {
  const codes = ["input_invalid", "input_unresolvable", "binding_missing", "binding_ambiguous", "page_token_missing", "source_conflict", "version_changed"];
  const seen = new Set<string>();
  for (const locale of ["en", "zh-CN", "zh-TW"] as const) {
    const c = claimsCopy[locale];
    assert.deepEqual(Object.keys(c.sourceErrors).sort(), [...codes].sort());
    for (const text of [...Object.values(c.sourceErrors), c.sourceForbidden, c.sourceUnavailable, c.conflict.source,
      c.source, c.sourceIntro, c.sourceInput, c.sourceSave, c.sourceNone, c.sourceUnverified, c.sourcePlatform.facebook, c.sourcePlatform.instagram,
      c.sourcePlatformLabel, c.sourcePlatformAuto, c.sourcePlatformHint, c.feedNone, c.feedBound, c.sourceCapped]) {
      assert.ok(text.length >= 2, text);
      seen.add(`${locale}:${text}`);
    }
  }
  assert.ok(seen.size > 3 * 10);
  // A locale that silently fell back to English would repeat the English sentence.
  for (const locale of ["zh-CN", "zh-TW"] as const)
    for (const code of codes) assert.notEqual((claimsCopy[locale].sourceErrors as Record<string, string>)[code], (claimsCopy.en.sourceErrors as Record<string, string>)[code]);
  assert.notEqual(claimsCopy["zh-CN"].sourceErrors.input_invalid, claimsCopy["zh-TW"].sourceErrors.input_invalid);
});

test("ruling t: defaults and replaced MOCK banner copy", () => {
  assert.equal(claimsCopy.en.feedNone, "Bind a Facebook or Instagram post to read comments automatically");
  assert.equal(claimsCopy.en.feedBound, "Reading comments from the bound post");
  assert.equal(claimsCopy.en.sourceCapped, "Skipped: intake limit reached");
  for (const locale of ["en", "zh-CN", "zh-TW"] as const) {
    const c = claimsCopy[locale] as Record<string, unknown>;
    assert.equal("mock" in c, false, `${locale} still carries the stale MOCK banner`);
    for (const text of [c.feedNone, c.feedBound, c.sourceCapped]) assert.doesNotMatch(String(text), /MOCK/);
  }
  for (const locale of ["zh-CN", "zh-TW"] as const)
    for (const key of ["feedNone", "feedBound", "sourceCapped"] as const) assert.notEqual(claimsCopy[locale][key], claimsCopy.en[key]);
});
