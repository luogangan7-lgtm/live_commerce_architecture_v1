// MF07 buyer half (contracts/manual-fulfilment-v1.md §5.2, §6): production storefront Next -> private buyerhttp ->
// isolated PG. The buyer session is the SEALED cookie apps/storefront/lib/buyer-server.ts verifies, forged here with the
// test-only cookie key that Go handed us; the order was paid through the real capture path and shipped by the admin
// half (tests/admin/manual-fulfilment.spec.ts) just before. Driven by tests/foundation/browser_refund_fulfilment_test.go.
// BFF -> Go: GET /api/buyer/orders/{id} -> /v1/buyer/orders/{id} (`shipment` null | SHIPPED object).
// NOT_RUN until refund-fulfilment-ui adds the shipment display: the wording below is the contract's.
import assert from "node:assert/strict";
import { createHash, createHmac } from "node:crypto";
import http from "node:http";
import https from "node:https";
import net from "node:net";
import { spawn, execFileSync } from "node:child_process";
import { once } from "node:events";
import { readFile, writeFile, mkdtemp, rm } from "node:fs/promises";
import { createWriteStream } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { expect } from "@playwright/test";
import { launch, ctxOpts, phone } from "./browser-engine.mjs"; // LC_BROWSER_ENGINE=chromium|webkit; chromium behaviour is unchanged

const env = (name) => {
  const value = process.env[name];
  assert(value, `${name} is required`);
  return value;
};
const root = process.cwd(), evidence = env("LC_RF_EVIDENCE"), origin = env("LC_RF_ORIGIN");
const productID = env("LC_RF_PRODUCT"), orderID = env("LC_RF_ORDER"), buyerToken = env("LC_RF_BUYER_TOKEN");
const tracking = env("LC_RF_TRACKING"), carrierEN = env("LC_RF_CARRIER"), urlHost = env("LC_RF_URL_HOST");
const cookieKey = env("COMMERCE_BUYER_COOKIE_KEY");
const children = new Set(), sockets = new Set(), contexts = [], logs = [];
const pause = (ms) => new Promise((r) => setTimeout(r, ms));
const listen = async (s) => { s.listen(0, "127.0.0.1"); await once(s, "listening"); return s.address().port; };
const certDir = await mkdtemp(path.join(tmpdir(), "lc-shipment-edge-"));
const delivered = /in transit|delivered|已送達|已送达|運送中|运送中/i;
const carrier = { en: carrierEN, "zh-TW": "7-ELEVEN 交貨便", "zh-CN": "7-ELEVEN 交货便" };
let browser, edge, proxy;

function seal(token) {
  const iat = Math.floor(Date.now() / 1000);
  const envelope = JSON.stringify({ v: 1, token, iat, exp: iat + 3000, origin }); // key order is part of the verifier
  const payload = Buffer.from(envelope).toString("base64url");
  const mac = createHmac("sha256", Buffer.from(cookieKey, "base64url")).update("buyer-cookie-v1").update("\0").update(payload).digest("base64url");
  return `${payload}.${mac}`;
}
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
  const log = createWriteStream(path.join(evidence, "shipment-next.log"), { flags: "wx", mode: 0o600 }); logs.push(log); await once(log, "open");
  const childEnv = { ...process.env, NODE_ENV: "production", NEXT_TELEMETRY_DISABLED: "1" };
  for (const key of Object.keys(childEnv)) if (key.startsWith("LC_RF_")) delete childEnv[key];
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
async function context(mobile) {
  const c = await browser.newContext(ctxOpts(mobile
    ? { ...phone, viewport: { width: 390, height: 844 }, screen: { width: 390, height: 844 }, ignoreHTTPSErrors: true }
    : { ignoreHTTPSErrors: true, viewport: { width: 1440, height: 900 } }));
  contexts.push(c);
  await c.addCookies([{ name: "__Host-commerce_buyer", value: seal(buyerToken), url: origin, secure: true, httpOnly: true, sameSite: "Lax" }]);
  return c;
}
const manifest = path.join(evidence, "screenshots.json");
async function shot(page, name, locale, viewport) {
  const file = path.join(evidence, `${name}-${locale}-${viewport}.png`);
  await page.screenshot({ path: file, fullPage: true });
  assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), `horizontal overflow at ${viewport} ${locale}`);
  let list = []; try { list = JSON.parse(await readFile(manifest, "utf8")); } catch { /* first */ }
  list.push({ File: path.basename(file), Sha256: createHash("sha256").update(await readFile(file)).digest("hex"), Locale: locale, Viewport: viewport });
  await writeFile(manifest, JSON.stringify(list, null, 2));
}
async function openOrder(page, locale) {
  await page.goto(`${origin}/${locale}/products/${productID}`);
  await page.getByTestId("toggle-order-history").click();
  const detailResponse = page.waitForResponse((r) => new URL(r.url()).pathname === `/api/buyer/orders/${orderID}` && r.request().method() === "GET");
  await page.locator(`button[data-order-id="${orderID}"]`).click();
  const response = await detailResponse; assert.equal(response.status(), 200);
  await expect(page.getByTestId("order-id")).toHaveText(orderID);
  return response.json();
}
try {
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
  browser = await launch({ headless: true, proxy: { server: `http://127.0.0.1:${await listen(proxy)}` } });

  for (const locale of ["en", "zh-TW", "zh-CN"]) {
    for (const mobile of [false, true]) {
      const c = await context(mobile);
      await c.grantPermissions(["clipboard-read", "clipboard-write"], { origin }).catch(() => {});
      const page = await c.newPage();
      const body = await openOrder(page, locale);
      // wire: the buyer detail carries exactly the six buyer keys, never merchant-only fields
      assert(body.shipment && typeof body.shipment === "object", "shipment missing from the buyer order detail");
      assert.deepEqual(Object.keys(body.shipment).sort(), ["carrier_code", "carrier_name", "recorded_at", "status", "tracking_number", "tracking_url"]);
      assert.equal(body.shipment.status, "SHIPPED"); assert.equal(body.shipment.tracking_number, tracking);
      for (const banned of ["note", "void_reason", "principal_id", "version"]) assert(!(banned in body.shipment), `${banned} leaked to the buyer`);
      const text = await page.locator("body").innerText();
      assert(text.includes(tracking), "tracking number not shown"); // leading zeroes kept
      assert(text.includes(carrier[locale]), `carrier display name for ${locale}`);
      assert(!delivered.test(text), "the buyer page claims transit or delivery (I13)");
      assert(!(await page.content()).includes("MF07 internal note"), "merchant-only note reached the storefront");
      const link = page.locator(`a[href^="https://${urlHost}/"]`).first();
      await expect(link).toBeVisible();
      assert.equal(await link.getAttribute("target"), "_blank");
      const rel = (await link.getAttribute("rel")) ?? "";
      for (const token of ["noopener", "noreferrer", "nofollow"]) assert(rel.split(/\s+/).includes(token), `rel lacks ${token}: ${rel}`);
      assert((await link.locator("xpath=..").innerText()).includes(urlHost), "the link host is not visible next to the link");
      if (locale === "en") {
        assert(/Shipped by the seller/i.test(text), "missing 'Shipped by the seller'");
        const copy = page.getByRole("button", { name: /copy/i }).first();
        await expect(copy).toBeVisible();
        if (!mobile) { await copy.click(); assert.equal(await page.evaluate(() => navigator.clipboard.readText()), tracking); }
      }
      await shot(page, "shipment-buyer", locale, mobile ? "mobile" : "desktop");
      await c.close();
    }
  }
  console.log("PASS shipment-buyer: 3 locales x desktop/mobile");
} finally {
  for (const c of contexts) await c.close().catch(() => {});
  await browser?.close().catch(() => {});
  for (const s of sockets) s.destroy();
  for (const server of [proxy, edge]) if (server) await new Promise((r) => server.close(r));
  for (const child of children) child.kill("SIGKILL");
  for (const log of logs) log.end();
  await rm(certDir, { recursive: true, force: true });
}
