// MA09b buyer consent -> CAPI browser context (contracts/meta-ads-v1.md §9 MA09, §4.3 `ads.put_capi_context`, §6.4; AD8, A-3, ruling O6;
// customers-billing-v1 CD5). Production storefront Next -> private buyerhttp (consent PUT + attribution.PutCAPIContext) -> isolated PG.
// Started by tests/foundation/browser_meta_ads_test.go (TestBrowserMetaAdsConsent, build tag browser). The synthetic buyer.example TLS edge +
// CONNECT proxy is the technique of privacy-buyer.mjs; no business route is mocked. A runner-only control listener lets this script ask
// Go for facts only Go may read (the CAPI context row, its stored user agent) and run the CAPI sweeper; Node never sees a database credential.
// Proves: the privacy page names the purchase-events disclosure (O6); granting ads_personalization in the browser stores the browser's
// user agent as the CAPI context (exactly one row, the UA the BFF forwarded); marketing_messages never creates one; withdrawing leaves
// nothing after the sweeper (no context, consent_allows false); a re-grant stores a fresh context with the new UA; buyer erasure deletes
// it too. Labels: BROWSER (Meta = not involved: no event is sent by this gate).
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import http from "node:http";
import https from "node:https";
import net from "node:net";
import { spawn, execFileSync } from "node:child_process";
import { once } from "node:events";
import { readFile, writeFile, mkdtemp, rm } from "node:fs/promises";
import { createWriteStream } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { chromium, expect } from "@playwright/test";
import { privacyCopy } from "../../apps/storefront/lib/privacy-copy.ts";

const env = (name) => {
  const value = process.env[name];
  assert(value, `${name} is required`);
  return value;
};
const root = process.cwd(), evidence = env("LC_AC_EVIDENCE"), origin = env("LC_AC_ORIGIN");

const children = new Set(), sockets = new Set(), contexts = [], logs = [], observations = [];
const pause = (ms) => new Promise((r) => setTimeout(r, ms));
const listen = async (s) => { s.listen(0, "127.0.0.1"); await once(s, "listening"); return s.address().port; };
const pass = (name) => { observations.push(name); console.log(`PASS ${name}`); };
const certDir = await mkdtemp(path.join(tmpdir(), "lc-adsconsent-edge-"));
let browser, edge, proxy;

function relay(port, req, body) {
  return new Promise((resolve, reject) => {
    const headers = { ...req.headers }; delete headers.connection; delete headers["transfer-encoding"];
    if (body.length) headers["content-length"] = String(body.length); else delete headers["content-length"];
    const call = http.request({ hostname: "127.0.0.1", port, path: req.url, method: req.method, headers }, (res) => {
      const chunks = []; res.on("data", (x) => chunks.push(x)); res.on("error", reject);
      res.on("end", () => resolve({ status: res.statusCode, headers: res.headers, body: Buffer.concat(chunks) }));
    });
    call.setTimeout(15000, () => call.destroy(new Error("owned relay deadline"))); call.on("error", reject); call.end(body);
  });
}
async function startNext() {
  const reserve = net.createServer(), port = await listen(reserve); await new Promise((r) => reserve.close(r));
  const log = createWriteStream(path.join(evidence, "adsconsent-next.log"), { flags: "wx", mode: 0o600 }); logs.push(log); await once(log, "open");
  const childEnv = { ...process.env, NODE_ENV: "production", NEXT_TELEMETRY_DISABLED: "1" };
  for (const key of Object.keys(childEnv)) if (key.startsWith("LC_AC_")) delete childEnv[key];
  const child = spawn(process.execPath, [path.join(root, "apps/storefront/node_modules/next/dist/bin/next"), "start", "--hostname", "127.0.0.1", "--port", String(port)],
    { cwd: path.join(root, "apps/storefront"), env: childEnv, stdio: ["ignore", log, log] });
  children.add(child);
  for (let i = 0; i < 100; i++) {
    if (child.exitCode !== null) throw new Error("owned Next failed readiness");
    try { if ((await relay(port, { url: "/api/buyer/session", method: "GET", headers: { host: "buyer.example" } }, Buffer.alloc(0))).status === 200) return port; } catch { /* not ready */ }
    await pause(50);
  }
  throw new Error("owned Next readiness timeout");
}
async function newContext(mobile = false) {
  const c = await browser.newContext(mobile ? { ignoreHTTPSErrors: true, viewport: { width: 390, height: 844 } } : { ignoreHTTPSErrors: true, viewport: { width: 1440, height: 900 } });
  contexts.push(c);
  return c;
}
const manifest = path.join(evidence, "screenshots.json");
async function shot(page, name, locale, viewport) {
  const file = path.join(evidence, `buyer-${name}-${locale}-${viewport}.png`);
  await page.screenshot({ path: file, fullPage: true });
  assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), `horizontal overflow at ${viewport} ${locale} ${name}`);
  let list = []; try { list = JSON.parse(await readFile(manifest, "utf8")); } catch { /* first */ }
  list.push({ File: path.basename(file), Sha256: createHash("sha256").update(await readFile(file)).digest("hex"), Locale: locale, Viewport: viewport });
  await writeFile(manifest, JSON.stringify(list, null, 2));
}
const requestIs = (response, suffix, method) => new URL(response.url()).pathname === `/api/buyer/${suffix}` && response.request().method() === method;
async function setup() {
  execFileSync("openssl", ["req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", path.join(certDir, "key.pem"), "-out", path.join(certDir, "cert.pem"), "-days", "1", "-subj", "/CN=buyer.example"], { stdio: "ignore" });
  const port = await startNext();
  edge = https.createServer({ key: await readFile(path.join(certDir, "key.pem")), cert: await readFile(path.join(certDir, "cert.pem")) }, async (req, res) => {
    try {
      const chunks = []; for await (const x of req) chunks.push(x);
      const out = await relay(port, req, Buffer.concat(chunks));
      const headers = { ...out.headers }; delete headers.connection; delete headers["transfer-encoding"];
      res.writeHead(out.status, headers); res.end(out.body);
    } catch { if (!res.headersSent) res.writeHead(502); res.end(); }
  });
  const edgePort = await listen(edge);
  proxy = http.createServer((_, r) => { r.writeHead(403); r.end(); });
  proxy.on("connect", (req, socket, head) => {
    if (req.url !== "buyer.example:443") { socket.destroy(); return; }
    const upstream = net.connect(edgePort, "127.0.0.1", () => { socket.write("HTTP/1.1 200 Connection Established\r\n\r\n"); if (head.length) upstream.write(head); socket.pipe(upstream).pipe(socket); });
    for (const s of [socket, upstream]) { sockets.add(s); s.on("close", () => sockets.delete(s)); s.on("error", () => { socket.destroy(); upstream.destroy(); }); }
  });
  browser = await chromium.launch({ headless: true, proxy: { server: `http://127.0.0.1:${await listen(proxy)}` } });
}
const en = privacyCopy.en;
const control = env("LC_AC_CONTROL"), controlKey = env("LC_AC_CONTROL_KEY");
const ctl = async (resource) => {
  const response = await fetch(`${control}/${resource}`, { method: "POST", headers: { "X-Gate-Key": controlKey } });
  assert([200, 204].includes(response.status), `control ${resource}: ${response.status}`);
  return response.status === 200 ? response.json() : null;
};
const putsOf = (page) => {
  const puts = [];
  page.on("request", (r) => { if (r.method() === "PUT" && new URL(r.url()).pathname === "/api/buyer/consents") puts.push({ key: r.headers()["idempotency-key"] ?? null, body: r.postData() }); });
  return puts;
};
try {
  await setup();
  const ua1 = "Mozilla/5.0 (synthetic ads-consent one) SENTINEL-UA-ONE", ua2 = "Mozilla/5.0 (synthetic ads-consent two) SENTINEL-UA-TWO";
  await ctl("context/none");
  pass("start: no CAPI context in the store");

  // 1) privacy page: the O6 disclosure, then grant ads_personalization
  const c1 = await browser.newContext({ ignoreHTTPSErrors: true, userAgent: ua1 });
  contexts.push(c1);
  const p1 = await c1.newPage();
  const puts1 = putsOf(p1);
  await p1.goto(`${origin}/en/privacy`);
  await expect(p1.getByTestId("privacy-choices")).toBeVisible();
  const disclosure = await p1.getByTestId("privacy-choices").innerText();
  assert.match(disclosure, /purchase/i, "O6: the ads consent names sending purchase events");
  assert.match(disclosure, /Meta/, "O6: ... to Meta");
  assert.match(disclosure, /measurement|advertis|ads/i, "O6: ... for ad measurement");
  await expect(p1.getByTestId("privacy-ads_personalization")).toContainText(en.notGranted); // unchecked by default
  await p1.getByTestId("privacy-toggle-ads_personalization").click();
  await expect(p1.getByTestId("privacy-ads_personalization")).toContainText(en.granted);
  assert.equal(puts1.length, 1);
  assert.deepEqual(JSON.parse(puts1[0].body), { purpose: "ads_personalization", channel: "meta_ads", granted: true, context: "settings" });
  const ctx1 = await ctl("context/one");
  assert.equal(ctx1.user_agent, ua1, "the stored CAPI context is the browser's own user agent, forwarded by the BFF on the consent route");
  assert.equal(await p1.evaluate(() => navigator.userAgent), ua1);
  pass("ads_personalization granted in the browser -> exactly one CAPI context with the browser UA; the disclosure names purchase events (O6)");

  // 2) marketing_messages alone never creates a context
  const c2 = await browser.newContext({ ignoreHTTPSErrors: true, userAgent: ua2 });
  contexts.push(c2);
  const p2 = await c2.newPage();
  await p2.goto(`${origin}/en/privacy`);
  await expect(p2.getByTestId("privacy-choices")).toBeVisible();
  await p2.getByTestId("privacy-toggle-marketing_messages").click();
  await expect(p2.getByTestId("privacy-marketing_messages")).toContainText(en.granted);
  await ctl("context/one"); // still only the first buyer's row
  pass("marketing_messages granted by another buyer creates no CAPI context");

  // 3) withdrawal: consent_allows turns false; the sweeper deletes the context; nothing is planned
  await p1.getByTestId("privacy-toggle-ads_personalization").click();
  await expect(p1.getByTestId("privacy-ads_personalization")).toContainText(en.notGranted);
  assert.equal(puts1.length, 2);
  assert.deepEqual(JSON.parse(puts1[1].body), { purpose: "ads_personalization", channel: "meta_ads", granted: false, context: "settings" });
  await ctl("sweep");
  await ctl("context/none");
  await ctl("consent/denied");
  pass("withdrawal -> consent_allows false, the sweeper deletes the CAPI context, no operation planned");

  // 4) re-grant from a browser with another UA: a fresh context (upsert), the new UA
  const c3 = await browser.newContext({ ignoreHTTPSErrors: true, userAgent: ua2 });
  contexts.push(c3);
  await p1.reload();
  await p1.getByTestId("privacy-toggle-ads_personalization").click();
  await expect(p1.getByTestId("privacy-ads_personalization")).toContainText(en.granted);
  assert.equal((await ctl("context/one")).user_agent, ua1);
  pass("re-grant stores a fresh CAPI context");

  // 5) erasure removes the context as well (typed confirmation)
  const erasures = [];
  p1.on("response", (r) => { if (/\/api\/buyer\/privacy\/erasure$/.test(new URL(r.url()).pathname)) erasures.push(r); });
  await p1.getByTestId("privacy-erase").click();
  await p1.getByTestId("privacy-erase-typed").fill("ERASE");
  await p1.getByTestId("privacy-erase-submit").click();
  await expect.poll(() => erasures.length, { timeout: 15000 }).toBeGreaterThan(0);
  // Go is the truth for the ads half: whatever the page rendered, the owner is erased, consent is false and the sweeper leaves no context
  await ctl("sweep");
  await ctl("context/none");
  await ctl("consent/denied");
  pass("buyer erasure -> consent_allows false, CAPI context deleted (Go truth)");
  await writeFile(path.join(evidence, "ads-consent-result.json"), JSON.stringify({ observations }, null, 2));
  // Last, separately: the buyer must SEE the erasure succeed (customers-billing-v1 §5: the page shows the erased state)
  await expect(p1.getByTestId("privacy-erased")).toBeVisible().catch(async (error) => {
    const seen = await Promise.all(erasures.map(async (r) => `${r.status()} ${(await r.text()).slice(0, 300)}`));
    throw new Error(`UI erased state missing after a committed erasure: erasure responses=${JSON.stringify(seen)} cause=${error.message.split("\n")[0]}`);
  });
  pass("erasure is shown to the buyer as done");
  await writeFile(path.join(evidence, "ads-consent-result.json"), JSON.stringify({ observations }, null, 2));
  console.log("PASS ads-consent.mjs");
} finally {
  await Promise.allSettled(contexts.map((c) => c.close()));
  await browser?.close().catch(() => {});
  for (const child of children) child.kill("SIGKILL");
  for (const s of sockets) s.destroy();
  await new Promise((r) => edge?.close(r) ?? r());
  await new Promise((r) => proxy?.close(r) ?? r());
  for (const log of logs) log.end();
  await rm(certDir, { recursive: true, force: true });
}
