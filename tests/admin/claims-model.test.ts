// MDEF-1 / ruling F1: the Studio › Claims bundle parser accepts the platforms Go emits
// (manual with a label; facebook/instagram with an empty label, meta-claims-intake-v1 §4/§8)
// and still rejects unknown platforms and label/platform mismatches.
import assert from "node:assert/strict";
import { test } from "node:test";
import { parseBundlePage } from "../../apps/admin/lib/claims-model.ts";

const bundle = (platform: string, label: string) => ({
  bundle_id: "11111111-1111-4111-8111-111111111111", ref: "11111111", platform, label, bound: false, version: 1,
  link: { state: "NONE", generation: 0, expires_at: null }, lines: [],
  created_at: "2026-09-29T00:00:00Z", updated_at: "2026-09-29T00:00:00Z",
});
const page = (b: unknown) => ({ items: [b], next_cursor: "" });

test("bundle parser accepts manual (labelled) and Meta (unlabelled) bundles", () => {
  assert.equal(parseBundlePage(page(bundle("manual", "Amy"))).items[0].platform, "manual");
  assert.equal(parseBundlePage(page(bundle("facebook", ""))).items[0].platform, "facebook");
  assert.equal(parseBundlePage(page(bundle("instagram", ""))).items[0].platform, "instagram");
});

test("bundle parser rejects unknown platforms and label/platform mismatches", () => {
  for (const b of [bundle("tiktok", ""), bundle("manual", ""), bundle("facebook", "Amy"), bundle("", "")])
    assert.throws(() => parseBundlePage(page(b)), `${b.platform}/${b.label}`);
});
