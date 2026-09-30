// RF11 buyer half (contracts/stripe-refund-v1.md §7.2, F-R8, §9): production storefront Next -> private buyerhttp ->
// isolated PG. Same sealed-cookie technique as shipment-buyer.mjs. Two phases (LC_RF_PHASE):
//   processing — the merchant's partial refund is held PENDING at the fake provider: the order page says
//                "Refund processing", shows no refunded amount, payment state still CAPTURED; the script then settles
//                the refund through the Go control endpoint and waits for
//                "Refunded X — your bank may take 5–10 business days" (only for succeeded amounts).
//   final      — the order is fully refunded (REFUNDED); 3 locales x desktop/mobile screenshots (hashed).
// BFF -> Go: GET /api/buyer/orders/{id}/payment -> /v1/buyer/orders/{id}/payment (hosted_payment_view_v2 successor).
// NOT_RUN until refund-fulfilment-ui adds the buyer refund display.
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
const control = env("LC_RF_CONTROL"), phase = env("LC_RF_PHASE");
const captured = Number(env("LC_RF_CAPTURED")), partial = Number(env("LC_RF_PARTIAL"));
const cookieKey = env("COMMERCE_BUYER_COOKIE_KEY");
assert(["processing", "final"].includes(phase));
const money = (minor) => (minor / 100).toFixed(2);
const children = new Set(), sockets = new Set(), contexts = [], logs = [];
const pause = (ms) => new Promise((r) => setTimeout(r, ms));
const listen = async (s) => { s.listen(0, "127.0.0.1"); await once(s, "listening"); return s.address().port; };
const certDir = await mkdtemp(path.join(tmpdir(), "lc-refund-edge-"));
let browser, edge, proxy;

function seal(token) {
  const iat = Math.floor(Date.now() / 1000);
  const payload = Buffer.from(JSON.stringify({ v: 1, token, iat, exp: iat + 3000, origin })).toString("base64url");
  return `${payload}.${createHmac("sha256", Buffer.from(cookieKey, "base64url")).update("buyer-cookie-v1").update("\0").update(payload).digest("base64url")}`;
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
  const log = createWriteStream(path.join(evidence, `refund-next-${phase}.log`), { flags: "wx", mode: 0o600 }); logs.push(log); await once(log, "open");
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
  await page.locator(`button[data-order-id="${orderID}"]`).click();
  await expect(page.getByTestId("order-id")).toHaveText(orderID);
}
// the buyer payment view through the storefront BFF (the same request the page makes)
async function paymentView(page) {
  return page.evaluate(async (id) => {
    const session = await (await fetch("/api/buyer/session", { cache: "no-store" })).json();
    const response = await fetch(`/api/buyer/orders/${id}/payment`, { headers: { "X-Buyer-Context": session.context }, cache: "no-store" });
    return { status: response.status, body: await response.json() };
  }, orderID);
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

  if (phase === "processing") {
    const c = await context(false);
    const page = await c.newPage();
    await openOrder(page, "en");
    let view = await paymentView(page);
    assert.equal(view.status, 200);
    assert.equal(view.body.payment_state, "CAPTURED", "a PENDING refund must not change the payment state (RD7)");
    assert.deepEqual(view.body.refund, { refunded_minor: 0, pending_minor: partial });
    await expect(page.getByText(/Refund processing/i)).toBeVisible();
    assert(!/Refunded\s/i.test(await page.locator("body").innerText()) || !/business days/i.test(await page.locator("body").innerText()), "the refunded message appeared before any refund succeeded");
    assert(!JSON.stringify(view.body).match(/re_|stripe|failure|reason/i), "buyer refund projection leaks ids or reasons");
    await shot(page, "refund-buyer-processing", "en", "desktop");
    // settle at the provider (Go control endpoint), then the page must move to the succeeded message
    const settled = await fetch(`${control}/settle`, { method: "POST" });
    assert.equal(settled.status, 204);
    let text = "";
    for (let i = 0; i < 60; i++) {
      await page.reload(); await page.getByTestId("toggle-order-history").click(); await page.locator(`button[data-order-id="${orderID}"]`).click();
      // The payment block (status + refund lines) renders after its own fetch; read the DOM only once it is there.
      await page.getByTestId("payment-status").waitFor({ timeout: 10_000 });
      text = await page.locator("body").innerText();
      if (/Refunded/i.test(text) && /5.10 business days/i.test(text)) break;
      await pause(1000);
    }
    assert(/Refunded/i.test(text) && /5.10 business days/i.test(text), "no 'Refunded X — your bank may take 5–10 business days' after the refund succeeded");
    assert(text.includes(money(partial)), "the refunded amount is missing");
    view = await paymentView(page);
    assert.equal(view.body.payment_state, "PARTIALLY_REFUNDED");
    assert.deepEqual(view.body.refund, { refunded_minor: partial, pending_minor: 0 });
    assert(!/Refund processing/i.test(text), "still 'processing' after settlement");
    await c.close();
    console.log("PASS refund-buyer processing -> refunded");
  } else {
    for (const locale of ["en", "zh-TW", "zh-CN"]) {
      for (const mobile of [false, true]) {
        const c = await context(mobile);
        const page = await c.newPage();
        await openOrder(page, locale);
        const view = await paymentView(page);
        assert.equal(view.body.payment_state, "REFUNDED");
        assert.deepEqual(view.body.refund, { refunded_minor: captured, pending_minor: 0 });
        assert.deepEqual(Object.keys(view.body.refund).sort(), ["pending_minor", "refunded_minor"]);
        await expect(page.getByTestId("payment-status")).toHaveAttribute("data-state", "REFUNDED");
        assert((await page.locator("body").innerText()).includes(money(captured)), `refunded total for ${locale}`);
        await shot(page, "refund-buyer", locale, mobile ? "mobile" : "desktop");
        await c.close();
      }
    }
    console.log("PASS refund-buyer final: 3 locales x desktop/mobile");
  }
} finally {
  for (const c of contexts) await c.close().catch(() => {});
  await browser?.close().catch(() => {});
  for (const s of sockets) s.destroy();
  for (const server of [proxy, edge]) if (server) await new Promise((r) => server.close(r));
  for (const child of children) child.kill("SIGKILL");
  for (const log of logs) log.end();
  await rm(certDir, { recursive: true, force: true });
}
