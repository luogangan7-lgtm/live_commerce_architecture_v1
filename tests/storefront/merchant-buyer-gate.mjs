// Causal gate: actual merchant UI -> admin BFF/Go/PG -> configured URL -> buyer
// production Next. The only host mapping is this disposable TLS/CONNECT edge.
import assert from "node:assert/strict";
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
import { launch, ctxOpts } from "./browser-engine.mjs"; // LC_BROWSER_ENGINE=chromium|webkit; chromium behaviour is unchanged

const root = process.cwd(), evidence = process.env.LC_JOINT_EVIDENCE;
const adminOrigin = process.env.COMMERCE_PUBLIC_ORIGIN, buyerOrigin = "https://buyer.example";
// http://127.0.0.1 (chromium) or the https TLS front browserFront() builds for WebKit, which refuses `__Host-` cookies on http.
assert(evidence && /^https?:\/\/127\.0\.0\.1:\d+$/.test(adminOrigin));
assert(/^http:\/\/127\.0\.0\.1:\d+$/.test(process.env.LC_JOINT_CONTROL));
const certDir = await mkdtemp(path.join(tmpdir(), "lc-merchant-buyer-edge-"));
const children = new Set(), sockets = new Set(), logs = [];
const running = child => child.exitCode === null && child.signalCode === null;
let browser, edge, proxy, cases = 0;
const pass = name => { cases++; console.log(`PASS ${name}`); };
const wait = ms => new Promise(resolve => setTimeout(resolve, ms));
const listen = async server => { server.listen(0, "127.0.0.1"); await once(server, "listening"); return server.address().port; };
async function control(resource, method = "GET") {
  const response = await fetch(`${process.env.LC_JOINT_CONTROL}/${resource}`, {method, headers: {"X-Gate-Key": process.env.LC_JOINT_CONTROL_KEY}});
  assert.equal(response.status, 200, `fixture ${resource}`);
  return response.json();
}
function relay(port, request, body = Buffer.alloc(0)) {
  return new Promise((resolve, reject) => {
    const headers = {...request.headers};
    delete headers.connection; delete headers["transfer-encoding"];
    if (body.length) headers["content-length"] = String(body.length); else delete headers["content-length"];
    const call = http.request({hostname: "127.0.0.1", port, path: request.url, method: request.method, headers}, response => {
      const chunks = [];
      response.on("data", chunk => chunks.push(chunk));
      response.on("error", reject);
      response.on("end", () => resolve({status: response.statusCode, headers: response.headers, body: Buffer.concat(chunks)}));
    });
    call.setTimeout(15000, () => call.destroy(new Error("fixture relay timeout")));
    call.on("error", reject); call.end(body);
  });
}
async function startNext(app, port) {
  if (!port) { const reserve = net.createServer(); port = await listen(reserve); await new Promise(resolve => reserve.close(resolve)); }
  const log = createWriteStream(path.join(evidence, `${app}.log`), {flags: "wx", mode: 0o600});
  logs.push(log); await once(log, "open");
  // The runner-only privileged fixture key is never inherited by either app.
  const env = {...process.env, NODE_ENV: "production", NEXT_TELEMETRY_DISABLED: "1"};
  for (const name of Object.keys(env)) if (name.startsWith("LC_JOINT_")) delete env[name];
  const args = app === "admin"
    ? [path.join(root, "apps/admin/.next/standalone/apps/admin/server.js")]
    : [path.join(root, "apps/storefront/node_modules/next/dist/bin/next"), "start", "--hostname", "127.0.0.1", "--port", String(port)];
  env.HOSTNAME = "127.0.0.1"; env.PORT = String(port);
  const child = spawn(process.execPath, args, {
    cwd: path.join(root, `apps/${app}`), env, stdio: ["ignore", log, log],
  });
  children.add(child);
  for (let i = 0; i < 100; i++) {
    if (!running(child)) throw new Error(`owned ${app} exited before readiness`);
    try {
      const response = await relay(port, {url: app === "admin" ? "/api/stores" : "/api/buyer/session", method: "GET", headers: {host: app === "admin" ? new URL(adminOrigin).host : "buyer.example"}});
      if (response.status === (app === "admin" ? 401 : 200)) return port;
    } catch {}
    await wait(50);
  }
  throw new Error(`owned ${app} readiness deadline`);
}
const noPurchaseEffects = (before, after) => {
  for (const table of ["storefront.cart_lines", "storefront.quotes", "storefront.events", "checkout.orders", "checkout.command_results", "checkout.payment_attempts", "integration.operations", "integration.operation_events", "inventory.reservations", "inventory.ledger", "ops.command_results"])
    assert.equal(after[table], before[table], `unexpected ${table} effect`);
};
try {
  execFileSync("openssl", ["req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", path.join(certDir, "key.pem"), "-out", path.join(certDir, "cert.pem"), "-days", "1", "-subj", "/CN=buyer.example"], {stdio: "ignore"});
  const [, buyerPort] = await Promise.all([startNext("admin", Number(process.env.LC_JOINT_ADMIN_PORT)), startNext("storefront")]);
  edge = https.createServer({key: await readFile(path.join(certDir, "key.pem")), cert: await readFile(path.join(certDir, "cert.pem"))}, async (request, response) => {
    try {
      const chunks = []; for await (const chunk of request) chunks.push(chunk);
      const out = await relay(buyerPort, request, Buffer.concat(chunks));
      const headers = {...out.headers}; delete headers.connection; delete headers["transfer-encoding"];
      response.writeHead(out.status, headers); response.end(out.body);
    } catch { response.writeHead(502); response.end(); }
  });
  const edgePort = await listen(edge);
  proxy = http.createServer((_, response) => { response.writeHead(403); response.end(); });
  proxy.on("connect", (request, socket, head) => {
    if (request.url !== "buyer.example:443") { socket.destroy(); return; }
    const upstream = net.connect(edgePort, "127.0.0.1", () => {
      socket.write("HTTP/1.1 200 Connection Established\r\n\r\n");
      if (head.length) upstream.write(head);
      socket.pipe(upstream).pipe(socket);
    });
    for (const connection of [socket, upstream]) {
      sockets.add(connection); connection.on("close", () => sockets.delete(connection));
      connection.on("error", () => { socket.destroy(); upstream.destroy(); });
    }
  });
  const proxyPort = await listen(proxy);
  browser = await launch({headless: true, proxy: {server: `http://127.0.0.1:${proxyPort}`, bypass: "127.0.0.1"}});
  const context = await browser.newContext(ctxOpts({ignoreHTTPSErrors: true, viewport: {width: 390, height: 844}}));
  const merchant = await context.newPage();
  const uiErrors = [];
  context.on("page", page => page.on("pageerror", error => uiErrors.push(error.name)));
  merchant.on("pageerror", error => uiErrors.push(error.name));
  let sawIssuer = false;
  merchant.on("request", request => { if (new URL(request.url()).origin === process.env.COMMERCE_OIDC_ISSUER) sawIssuer = true; });
  await merchant.goto(`${adminOrigin}/en`);
  await merchant.getByRole("button", {name: "Sign in with identity service", exact: true}).click();
  await expect(merchant.getByRole("button", {name: "Add product", exact: true})).toBeVisible();
  assert(sawIssuer, "real signed MOCK IdP browser redirect required");
  const name = `Joint browser product ${Date.now()}`, code = `JOINT-${Date.now()}`;
  await merchant.getByRole("button", {name: "Add product", exact: true}).click();
  await merchant.getByRole("textbox", {name: "Product name", exact: true}).fill(name);
  await merchant.getByRole("textbox", {name: "Description", exact: true}).fill("Synthetic merchant-created joint acceptance product");
  const productResponse = merchant.waitForResponse(response => response.request().method() === "POST" && new URL(response.url()).pathname === `/api/stores/${process.env.LC_JOINT_STORE}/products`);
  await merchant.locator(".create-panel").getByRole("button", {name: "Add product", exact: true}).click();
  const productReply = await productResponse; assert.equal(productReply.status(), 200); const product = await productReply.json();
  await expect(merchant.getByRole("heading", {name: "Add first SKU", exact: true})).toBeVisible();
  await merchant.getByRole("textbox", {name: "SKU code", exact: true}).fill(code);
  await merchant.getByRole("spinbutton", {name: "Price in minor units", exact: true}).fill("12345");
  const skuResponse = merchant.waitForResponse(response => response.request().method() === "POST" && new URL(response.url()).pathname === `/api/stores/${process.env.LC_JOINT_STORE}/skus`);
  await merchant.locator(".create-panel").getByRole("button", {name: "Add first SKU", exact: true}).click();
  const skuReply = await skuResponse; assert.equal(skuReply.status(), 200); const sku = await skuReply.json();
  assert.equal(sku.product_id, product.id); assert.equal(sku.price_minor, 12345);
  await expect(merchant.locator(".create-panel")).toHaveCount(0);
  await expect(merchant.locator("tbody tr").filter({has: merchant.getByRole("button", {name, exact: true})}).locator(".available-value")).toHaveText("0");
  await expect(merchant.getByTestId("purchase-entry").locator("input")).toHaveValue(`${buyerOrigin}/en/products/${product.id}`);
  pass("signed MOCK IdP and actual merchant product/SKU saves returned real scoped receipts");
  const afterSaves = await control("facts");
  const locales = ["en", "zh-CN", "zh-TW"], urls = {};
  for (const locale of locales) {
    if (locale !== "en") {
      await merchant.goto(`${adminOrigin}/${locale}`);
      await merchant.getByRole("button", {name, exact: true}).click();
    }
    const input = merchant.getByTestId("purchase-entry").locator("input");
    await expect(input).toHaveValue(`${buyerOrigin}/${locale}/products/${product.id}`);
    urls[locale] = await input.inputValue();
    const projected = await merchant.evaluate(async target => { const response = await fetch(target); return {status: response.status, body: await response.json()}; }, `/api/stores/${process.env.LC_JOINT_STORE}/products/${product.id}/purchase-entry?locale=${locale}`);
    assert.equal(projected.status, 200);
    assert.deepEqual(projected.body, {product_id: product.id, locale, state: "configured", url: urls[locale]});
    // Actual document GET with scripts disabled models a link preview/crawler.
    const crawler = await browser.newContext(ctxOpts({ignoreHTTPSErrors: true, javaScriptEnabled: false}));
    const preview = await crawler.newPage();
    assert.equal((await preview.goto(urls[locale])).status(), 200);
    await crawler.close();
  }
  assert.deepEqual(await control("facts"), afterSaves);
  pass("all three configured locale URLs serve actual Next documents with zero crawler effects");
  await merchant.goto(`${adminOrigin}/en`);
  await merchant.getByRole("button", {name, exact: true}).click();
  await expect(merchant.getByTestId("purchase-entry").locator("input")).toHaveValue(urls.en);
  const buyer = await context.newPage();
  for (const locale of locales) {
    let page = buyer;
    if (locale === "en") {
      // The real merchant Open control fresh-reads projection and navigates.
      await merchant.getByRole("button", {name: "Open purchase page", exact: true}).click();
      page = merchant;
      await page.waitForURL(urls.en);
    } else await page.goto(urls[locale]);
    await expect(page.getByRole("heading", {name, exact: true})).toBeVisible();
    await expect(page.getByRole("radio", {name: code, exact: true})).toBeChecked();
    await expect(page.getByRole("radio")).toHaveCount(1);
    assert.equal(await page.getByRole("radio").inputValue(), sku.id);
    await expect(page.locator(".unit-price")).toHaveText(new Intl.NumberFormat(locale, {style: "currency", currency: sku.currency}).format(123.45));
    const state = await page.evaluate(async () => (await fetch("/api/buyer/session")).json());
    assert.equal(state.state, "active");
    const catalog = await page.evaluate(async ({product, context}) => { const response = await fetch(`/api/buyer/catalog?product_id=${product}`, {headers: {"X-Buyer-Context": context}}); return {status: response.status, body: await response.json()}; }, {product: product.id, context: state.context});
    assert.equal(catalog.status, 200);
    assert.deepEqual(catalog.body.items.map(item => [item.product_id, item.sku_id, item.sku_code, item.price_minor, item.currency]), [[product.id, sku.id, code, 12345, sku.currency]]);
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
    if (locale === "en") await page.screenshot({path: path.join(evidence, "buyer-from-merchant-mobile.png"), fullPage: true});
    pass(`${locale} exact configured URL has persisted product, SKU, price and mobile scope`);
  }
  await buyer.goto(`${buyerOrigin}/en/products/${process.env.LC_JOINT_FOREIGN_PRODUCT}`);
  await expect(buyer.locator("main[aria-busy=false]")).toBeVisible();
  await expect(buyer.getByRole("radio")).toHaveCount(0);
  await expect(buyer.getByRole("heading", {name: "BCAT foreign product", exact: true})).toHaveCount(0);
  const foreign = await buyer.evaluate(async product => {
    const state = await (await fetch("/api/buyer/session")).json();
    const response = await fetch(`/api/buyer/catalog?product_id=${product}`, {headers: {"X-Buyer-Context": state.context}});
    return {status: response.status, body: await response.json()};
  }, process.env.LC_JOINT_FOREIGN_PRODUCT);
  assert.equal(foreign.status, 200);
  assert.deepEqual(foreign.body, {items: [], next_cursor: ""});
  noPurchaseEffects(afterSaves, await control("facts"));
  pass("foreign tenant product is absent and viewing creates no purchase facts");
  await buyer.goto(urls.en);
  await expect(buyer.getByRole("radio", {name: code, exact: true})).toBeChecked();
  const activeState = await buyer.evaluate(async () => (await fetch("/api/buyer/session")).json());
  assert.equal(activeState.state, "active");
  const beforeRevocation = await control("facts");
  await control("unpublish", "POST");
  const denied = await buyer.evaluate(async ({sku, context}) => {
    const response = await fetch("/api/buyer/cart", {method: "PUT", headers: {"Content-Type": "application/json", "X-Buyer-Context": context, "Idempotency-Key": "joint-unpublished-cart"}, body: JSON.stringify({items: [{sku_id: sku, quantity: 1}]})});
    return {status: response.status, body: await response.json()};
  }, {sku: sku.id, context: activeState.context});
  assert.equal(denied.status, 404, "valid existing buyer must hit publication denial");
  assert.equal(denied.body.code, "not_found");
  await buyer.reload();
  await expect(buyer.locator("main").getByRole("alert")).toBeVisible();
  await expect(buyer.getByRole("radio")).toHaveCount(0);
  await merchant.goto(`${adminOrigin}/en`);
  await merchant.getByRole("button", {name, exact: true}).click();
  await expect(merchant.getByTestId("purchase-entry").getByRole("status")).toHaveText("No verified, published storefront address is available.");
  await expect(merchant.getByTestId("purchase-entry").locator("input")).toHaveCount(0);
  await expect(merchant.getByRole("button", {name: "Open purchase page", exact: true})).toHaveCount(0);
  pass("unpublish removes merchant URL and rejects existing-buyer new purchase and fresh page");
  const after = await control("facts");
  assert.deepEqual(after, beforeRevocation, "unpublish denial must create no buyer or purchase facts");
  noPurchaseEffects(afterSaves, after);
  assert.deepEqual(uiErrors, []);
  pass("independent database facts remain unchanged for orders, holds, quotes, providers and merchant writes");
  await writeFile(path.join(evidence, "result.json"), JSON.stringify({cases, product_id: product.id, sku_id: sku.id, name, code, locales, urls, after_saves: afterSaves, after, boundary: "production Next; signed MOCK IdP; synthetic local TLS/CONNECT and publication evidence; no deployment DNS/TLS or provider acceptance"}, null, 2), {mode: 0o600});
} catch (error) {
  if (browser) for (const context of browser.contexts()) for (const [index, page] of context.pages().entries()) {
    await page.screenshot({path: path.join(evidence, `failure-${context.pages().length}-${index}.png`), fullPage: true}).catch(() => {});
  }
  throw error;
} finally {
  if (browser) await browser.close();
  for (const socket of sockets) socket.destroy();
  for (const server of [proxy, edge]) if (server) await new Promise(resolve => server.close(resolve));
  // A child killed by a signal keeps exitCode === null (signalCode is set instead); awaiting "exit" for a
  // child that already exited hangs until Node aborts with "unsettled top-level await" (exit 13).
  for (const child of children) { if (running(child)) child.kill("SIGKILL"); }
  for (const child of children) if (running(child)) await once(child, "exit");
  for (const log of logs) log.end();
  await rm(certDir, {recursive: true, force: true});
}
