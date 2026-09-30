// Studio › Claims pure contracts (live-keyword-claims-v1 §7.1, §11.1): the admin BFF path
// grammar for exactly M1–M7, the closed M7 token shape, and the FROZEN host prompt copy.
import assert from "node:assert/strict";
import { test } from "node:test";
import {
  claimLinkRoute, claimsCollection, claimsRoutes, claimsSubpath, validClaimLink,
} from "../../apps/admin/lib/claims-request.ts";
import { hostPrompt, hostPromptExact } from "../../apps/admin/lib/claims-copy.ts";

const session = "live-sessions/11111111-1111-4111-8111-111111111111";
const other = "22222222-2222-4222-8222-222222222222";
const token = Buffer.alloc(32, 7).toString("base64url");

test("claims BFF allowlist is exactly M1–M7 per method", () => {
  const allowed: [string, string][] = [
    ["GET", `${session}/claims`], ["GET", `${session}/claims/bundles`],
    ["POST", `${session}/claims/window`], ["POST", `${session}/claims/offers`], ["POST", `${session}/claims/manual`],
    ["POST", `${session}/claims/bundles/${other}/link`], ["PATCH", `${session}/claims/offers/${other}`],
  ];
  for (const [method, path] of allowed) {
    assert.ok(new RegExp(`^${claimsRoutes[method as keyof typeof claimsRoutes]}$`).test(path), `${method} ${path}`);
    assert.ok(new RegExp(`^live-sessions/[0-9a-f-]{36}/${claimsSubpath}$`).test(path), path);
  }
  for (const [method, path] of [
    ["POST", `${session}/claims`], ["GET", `${session}/claims/window`], ["PATCH", `${session}/claims/offers`],
    ["POST", `${session}/claims/bundles`], ["GET", `${session}/claims/bundles/${other}/link`],
    ["POST", `${session}/claims/offers/${other}`], ["POST", `${session}/claims/labels`],
    ["POST", `${session}/claims/bundles/AAAAAAAA-2222-4222-8222-222222222222/link`],
  ] as [keyof typeof claimsRoutes, string][])
    assert.equal(new RegExp(`^${claimsRoutes[method] ?? "(?!)"}$`).test(path), false, `${method} ${path}`);
  assert.equal(claimsCollection(`${session}/claims/bundles`), true);
  assert.equal(claimsCollection(`${session}/claims`), false);
  assert.equal(claimLinkRoute(`${session}/claims/bundles/${other}/link`), true);
  assert.equal(claimLinkRoute(`${session}/claims/bundles`), false);
});

test("M7 link response shape is closed", () => {
  const first = { token, generation: 1, expires_at: "2026-10-01T00:00:00Z", released: false, replayed: false };
  assert.equal(validClaimLink(first), true);
  assert.equal(validClaimLink({ ...first, token: null, replayed: true }), true);
  for (const bad of [
    { ...first, token: null }, { ...first, replayed: true }, { ...first, token: token.slice(0, 42) + "B" },
    { ...first, token: token + "=" }, { ...first, generation: 0 }, { ...first, expires_at: "soon" },
    { ...first, bundle_id: other }, { token, generation: 1, expires_at: first.expires_at, released: false },
  ]) assert.equal(validClaimLink(bad), false, JSON.stringify(bad));
});

test("host prompt copy is the frozen text, verbatim", () => {
  assert.equal(hostPromptExact["zh-TW"], "留言關鍵字就能登記：{KW} = 1 件；{KW}+2 = 數量改成 2 件（不是再加 2 件）。之後再留言，以最新數量為準。請只留關鍵字，不要加其他文字。留言不代表已保留庫存，結帳時才確認。");
  assert.equal(hostPromptExact["zh-CN"], "评论口令即可登记：{KW} = 1 件；{KW}+2 = 数量改成 2 件（不是再加 2 件）。再次评论以最新数量为准。请只发口令，不要加其他文字。评论不代表已保留库存，结账时才确认。");
  assert.equal(hostPromptExact.en, "Comment the code to claim: {KW} = 1 item; {KW}+2 = set your quantity to 2 (it does not add 2 more). Your latest comment replaces the earlier quantity. Comment only the code. Claims don't reserve stock; stock is confirmed at checkout.");
  assert.equal(hostPrompt("en", "EXACT", "A1"), "Comment the code to claim: A1 = 1 item; A1+2 = set your quantity to 2 (it does not add 2 more). Your latest comment replaces the earlier quantity. Comment only the code. Claims don't reserve stock; stock is confirmed at checkout.");
  // KEYWORD_QTY_ONLY replaces only the first clause (before the first semicolon).
  assert.equal(hostPrompt("en", "KEYWORD_QTY_ONLY", "B2"), "Comment B2+quantity, e.g. B2+1; B2 alone is not counted; B2+2 = set your quantity to 2 (it does not add 2 more). Your latest comment replaces the earlier quantity. Comment only the code. Claims don't reserve stock; stock is confirmed at checkout.");
  assert.equal(hostPrompt("zh-TW", "KEYWORD_QTY_ONLY", "A1"), "請留言「A1+數量」，例如 A1+1；只留 A1 不會登記；A1+2 = 數量改成 2 件（不是再加 2 件）。之後再留言，以最新數量為準。請只留關鍵字，不要加其他文字。留言不代表已保留庫存，結帳時才確認。");
  assert.equal(hostPrompt("zh-CN", "KEYWORD_QTY_ONLY", "A1"), "请评论“A1+数量”，例如 A1+1；只发 A1 不会登记；A1+2 = 数量改成 2 件（不是再加 2 件）。再次评论以最新数量为准。请只发口令，不要加其他文字。评论不代表已保留库存，结账时才确认。");
});
