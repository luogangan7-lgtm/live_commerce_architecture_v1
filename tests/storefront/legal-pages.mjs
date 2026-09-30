// LG01 (docs/delivery/units/stripe-live-tests.md, legal-pages.md P1-P5): BROWSER gate for the storefront legal pages.
// Spawns `next start` itself on a free loopback port (no backend, no database, no network beyond loopback), then drives
// Chromium over every slug of the closed set x every locale:
//   200, exactly one h1, at least one h2, <html lang> = locale, a footer with all 6 links (5 policy pages + data deletion),
//   no request to any other origin, no horizontal scroll at 1440 and at 390 px;
// unknown slug / case variant / extra segment / unknown locale -> 404;
// /{locale}/data-deletion -> 200 only once customers-billing-ui has merged (route file present), else NOT_RUN, never PASS;
// counts the [data-owner-text] markers (the owner's to-do list) and, with LC_LEGAL_REQUIRE_FINAL=1, fails while any remains
// (the stripe-live-enable-v1 §9 `policy_pages` attestation check).
// Evidence: LC_LEGAL_EVIDENCE_DIR (default output/stripe-live-tests/lg01 under the cwd) gets lg01.json, the marker list and
// the screenshots; every screenshot is sha256-hashed into lg01.json. No PII, no secret: the pages are static text.
// Run: pnpm build:storefront && node tests/storefront/legal-pages.mjs   (cwd = repo root)
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { existsSync } from "node:fs";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import net from "node:net";
import path from "node:path";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { launch, ctxOpts } from "./browser-engine.mjs"; // LC_BROWSER_ENGINE=chromium|webkit; chromium behaviour is unchanged

const root = process.cwd();
const storefront = path.join(root, "apps/storefront");
const evidenceDir = path.resolve(process.env.LC_LEGAL_EVIDENCE_DIR || path.join(root, "output/stripe-live-tests/lg01"));
const requireFinal = process.env.LC_LEGAL_REQUIRE_FINAL === "1";
// The closed slug set of legal-pages.md P2 and the locales of packages/i18n (parsed, not imported: this gate must not
// depend on the code under test).
const slugs = ["privacy", "terms", "refunds", "shipping", "contact"];
const i18n = await readFile(path.join(root, "packages/i18n/src/index.ts"), "utf8");
const locales = [...(i18n.match(/export const locales = \[([^\]]+)\]/)?.[1] ?? "").matchAll(/'([^']+)'/g)].map((m) => m[1]);
assert.deepEqual([...locales].sort(), ["en", "zh-CN", "zh-TW"], "the storefront supports exactly three locales");

const results = [], notRun = [], failures = [], hashes = {}, markers = [];
const pass = (name) => { results.push(name); console.log(`PASS ${name}`); };
const fail = (name, why) => { failures.push(`${name}: ${why}`); console.log(`FAIL ${name}: ${why}`); };
const check = (name, fn) => Promise.resolve().then(fn).then(() => pass(name), (e) => fail(name, String(e.message ?? e).split("\n")[0]));

async function freePort() {
  const s = net.createServer();
  s.listen(0, "127.0.0.1");
  await once(s, "listening");
  const port = s.address().port;
  await new Promise((r) => s.close(r));
  return port;
}

const nextBin = path.join(storefront, "node_modules/next/dist/bin/next");
assert(existsSync(nextBin), "apps/storefront/node_modules/next is missing: run pnpm install");
assert(existsSync(path.join(storefront, ".next")), "no production build: run pnpm build:storefront first");
const port = await freePort();
const origin = `http://127.0.0.1:${port}`;
const env = { ...process.env, NODE_ENV: "production", NEXT_TELEMETRY_DISABLED: "1" };
const server = spawn(process.execPath, [nextBin, "start", "--hostname", "127.0.0.1", "--port", String(port)], { cwd: storefront, env, stdio: ["ignore", "pipe", "pipe"] });
let serverLog = "";
server.stdout.on("data", (d) => (serverLog += d));
server.stderr.on("data", (d) => (serverLog += d));
let browser;
async function stop() {
  if (browser) await browser.close().catch(() => {});
  if (server.exitCode === null) {
    server.kill("SIGTERM");
    await Promise.race([once(server, "exit"), new Promise((r) => setTimeout(r, 5000))]);
    if (server.exitCode === null) server.kill("SIGKILL");
  }
}
process.on("SIGINT", () => stop().finally(() => process.exit(130)));

try {
  for (let i = 0, ready = false; !ready; i++) {
    if (server.exitCode !== null) throw new Error(`next start exited: ${serverLog.slice(-400)}`);
    if (i > 200) throw new Error("next start did not become ready");
    try { ready = (await fetch(`${origin}/en/legal/privacy`)).status === 200; } catch { await new Promise((r) => setTimeout(r, 100)); }
  }
  await mkdir(evidenceDir, { recursive: true });
  browser = await launch();
  const viewports = { desktop: { width: 1440, height: 900 }, mobile: { width: 390, height: 844 } };
  const expectedLinks = (locale) => [...slugs.map((s) => `/${locale}/legal/${s}`), `/${locale}/data-deletion`].sort();

  for (const locale of locales) {
    for (const slug of slugs) {
      for (const [vp, size] of Object.entries(viewports)) {
        const name = `${locale}/${slug} @${vp}`;
        await check(name, async () => {
          const context = await browser.newContext(ctxOpts({ viewport: size }));
          const page = await context.newPage();
          const foreign = [];
          page.on("request", (r) => {
            const u = new URL(r.url());
            if (!["data:", "blob:", "about:"].includes(u.protocol) && u.origin !== origin) foreign.push(r.url());
          });
          try {
            const response = await page.goto(`${origin}/${locale}/legal/${slug}`, { waitUntil: "networkidle" });
            assert.equal(response.status(), 200, "HTTP status");
            assert.equal(await page.locator("h1").count(), 1, "exactly one h1");
            assert((await page.locator("h1").innerText()).trim().length > 0, "empty h1");
            assert((await page.locator("h2").count()) >= 1, "no h2 section");
            assert.equal(await page.evaluate(() => document.documentElement.lang), locale, "<html lang>");
            const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
            assert(overflow <= 0, `horizontal scroll by ${overflow}px`);
            const found = await page.locator("[data-owner-text]").evaluateAll((els) => els.map((e) => e.getAttribute("data-owner-text")));
            if (vp === "desktop") markers.push({ locale, slug, pending: found.filter((k) => k === "pending").length, draft: found.filter((k) => k === "draft").length });
            const shot = await page.screenshot({ fullPage: true });
            const file = `${locale}-${slug}-${vp}.png`;
            await writeFile(path.join(evidenceDir, file), shot);
            hashes[file] = createHash("sha256").update(shot).digest("hex");
            const footer = page.locator("footer");
            assert((await footer.count()) >= 1, "no footer (LegalFooter is not mounted in the storefront layout)");
            const links = await footer.locator("a[href]").evaluateAll((as) => as.map((a) => a.getAttribute("href")));
            assert.deepEqual([...links].sort(), expectedLinks(locale), "footer links");
            for (const text of await footer.locator("a").allInnerTexts()) assert(text.trim().length > 0, "empty footer link label");
            assert.deepEqual(foreign, [], "requests to another origin");
          } finally {
            await context.close();
          }
        });
      }
    }
  }

  // Unknown slug, case variants, extra segment and unknown locale are all 404 (notFound), for every locale.
  for (const locale of locales) {
    for (const bad of ["nope", "PRIVACY", "Privacy", "privacy/extra", "privacy.html", "data-deletion-x", ""]) {
      await check(`404 /${locale}/legal/${bad}`, async () => {
        const r = await fetch(`${origin}/${locale}/legal/${bad}`); // a trailing-slash redirect is followed to its final answer
        assert.equal(r.status, 404);
      });
    }
  }
  for (const bad of ["xx", "EN", "zh", "zh-tw"]) {
    await check(`404 /${bad}/legal/privacy (unknown locale)`, async () => {
      const r = await fetch(`${origin}/${bad}/legal/privacy`, { redirect: "manual" });
      assert.equal(r.status, 404);
    });
  }

  // data-deletion belongs to customers-billing-ui: 200 once its route file is merged, else NOT_RUN (never PASS).
  const routeDir = path.join(storefront, "app/[locale]/data-deletion");
  if (existsSync(routeDir)) {
    for (const locale of locales) {
      await check(`data-deletion 200 /${locale}/data-deletion`, async () => {
        const r = await fetch(`${origin}/${locale}/data-deletion`);
        assert.equal(r.status, 200);
      });
    }
  } else {
    notRun.push("data-deletion 200: apps/storefront/app/[locale]/data-deletion is absent (customers-billing-ui has not merged); the footer link is expected to 404 until then");
    console.log(`NOT_RUN ${notRun.at(-1)}`);
  }

  // The owner's to-do list: how many markers remain, per page; LC_LEGAL_REQUIRE_FINAL=1 fails while any does.
  const pending = markers.reduce((n, m) => n + m.pending, 0), draft = markers.reduce((n, m) => n + m.draft, 0);
  console.log(`OWNER-TEXT markers: pending=${pending} draft=${draft} (per page, desktop render)`);
  await writeFile(path.join(evidenceDir, "owner-text-markers.txt"), markers.map((m) => `${m.locale}/${m.slug} pending=${m.pending} draft=${m.draft}`).join("\n") + "\n");
  if (requireFinal) await check("LC_LEGAL_REQUIRE_FINAL=1: no [data-owner-text] marker remains", () => assert.equal(pending + draft, 0, `${pending} pending + ${draft} draft owner-text markers remain`));
  else notRun.push("LC_LEGAL_REQUIRE_FINAL=1 (the policy_pages attestation check) was not requested in this run");

  const summary = { commit: process.env.LC_EVIDENCE_COMMIT ?? "", origin: "loopback", locales, slugs, pass: results.length, fail: failures.length, not_run: notRun, markers: { pending, draft }, failures, screenshots: hashes };
  await writeFile(path.join(evidenceDir, "lg01.json"), JSON.stringify(summary, null, 2) + "\n");
  console.log(`LG01 summary: PASS=${results.length} FAIL=${failures.length} NOT_RUN=${notRun.length} screenshots=${Object.keys(hashes).length}`);
} catch (e) {
  fail("harness", String(e.message ?? e));
} finally {
  await stop();
}
process.exit(failures.length ? 1 : 0);
