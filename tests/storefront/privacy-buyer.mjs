// CB11 buyer half (contracts/customers-billing-v1.md §5 Storefront + Buyer paragraph, CD4/CD5/CD7, §8 CB11;
// customers-billing-ui.md U7/U8): production storefront Next -> private buyerhttp -> isolated PG. Started by
// tests/foundation/browser_customers_billing_test.go (build tag browser) after the admin half. The synthetic
// buyer.example TLS edge + CONNECT proxy is the same technique as refund-buyer.mjs / order-gate.mjs; no business route
// is mocked (only the consent PUT of one guest is aborted and one GET is answered 410, both to observe the UI branch).
// BFF -> Go: PUT /api/buyer/consents -> /v1/buyer/consents; GET /api/buyer/privacy; POST /api/buyer/privacy/export|erasure.
// Proves: checkout shows two UNTICKED boxes + notice link carrying the notice version + the purchase-events
// disclosure; after the order exists exactly one PUT per ticked box with context "checkout" (never source or
// policy_version) and none for an unticked box; a consent failure never blocks the order; /privacy shows the current
// state, toggles with context "settings", downloads a file, erases with a typed confirmation (refused while an order
// is open), and a 410 renders the erased state; /data-deletion is public static instructions that call no API;
// en + zh-TW, desktop + 390 px, screenshots hashed, no horizontal overflow.
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
import { expect } from "@playwright/test";
import { launch, ctxOpts } from "./browser-engine.mjs"; // LC_BROWSER_ENGINE=chromium|webkit; chromium behaviour is unchanged
import { privacyCopy } from "../../apps/storefront/lib/privacy-copy.ts";

const env = (name) => {
  const value = process.env[name];
  assert(value, `${name} is required`);
  return value;
};
const root = process.cwd(), evidence = env("LC_PB_EVIDENCE"), origin = env("LC_PB_ORIGIN");
const product = env("LC_PB_PRODUCT"), policy = env("LC_PB_POLICY");
const children = new Set(), sockets = new Set(), contexts = [], logs = [], observations = [];
const pause = (ms) => new Promise((r) => setTimeout(r, ms));
const listen = async (s) => { s.listen(0, "127.0.0.1"); await once(s, "listening"); return s.address().port; };
const pass = (name) => { observations.push(name); console.log(`PASS ${name}`); };
const certDir = await mkdtemp(path.join(tmpdir(), "lc-privacy-edge-"));
const keyPattern = /^[A-Za-z0-9_.:-]{8,128}$/;
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
  const log = createWriteStream(path.join(evidence, "privacy-next.log"), { flags: "wx", mode: 0o600 }); logs.push(log); await once(log, "open");
  const childEnv = { ...process.env, NODE_ENV: "production", NEXT_TELEMETRY_DISABLED: "1" };
  for (const key of Object.keys(childEnv)) if (key.startsWith("LC_PB_")) delete childEnv[key];
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
  const c = await browser.newContext(ctxOpts(mobile ? { ignoreHTTPSErrors: true, viewport: { width: 390, height: 844 } } : { ignoreHTTPSErrors: true, viewport: { width: 1440, height: 900 } }));
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
const pii = { recipient_name: "Synthetic Privacy Buyer", phone: "+886900000077", region: "Synthetic Region", city: "Synthetic City", postal_code: "99977", line1: "Synthetic Address Seventy Seven", line2: "Synthetic Unit 77" };

// One consent-PUT log per page: the request event fires for aborted requests too.
function watchConsents(page) {
  const puts = [];
  page.on("request", (r) => {
    if (r.method() === "PUT" && new URL(r.url()).pathname === "/api/buyer/consents") puts.push({ key: r.headers()["idempotency-key"] ?? null, body: r.postData() });
  });
  return puts;
}
async function checkoutPage(context) {
  const p = await context.newPage();
  const puts = watchConsents(p);
  await p.goto(`${origin}/en/products/${product}`);
  await p.getByRole("button", { name: "Choose delivery", exact: true }).click();
  const pending = p.waitForResponse((r) => requestIs(r, "quotes", "POST"));
  await p.getByRole("button", { name: "Get current total", exact: true }).click();
  assert.equal((await pending).status(), 200);
  await expect(p.getByTestId("address-section")).toBeVisible();
  for (const [key, value] of Object.entries(pii)) await p.locator(`input[name="${key}"]`).fill(value);
  await p.getByTestId("confirm-address").click();
  await expect(p.getByTestId("create-order")).toBeEnabled();
  return { p, puts };
}
async function placeOrder(p) {
  await p.getByTestId("create-order").click();
  await expect(p.getByTestId("order-section")).toBeVisible();
  return (await p.getByTestId("order-id").innerText()).trim();
}
async function buyerApi(page, method, suffix) {
  return page.evaluate(async ({ method, suffix }) => {
    const session = await (await fetch("/api/buyer/session", { cache: "no-store" })).json();
    const response = await fetch(`/api/buyer/${suffix}`, { method, headers: { "X-Buyer-Context": session.context }, cache: "no-store" });
    return { status: response.status, body: await response.json().catch(() => null) };
  }, { method, suffix });
}
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
  browser = await launch({ headless: true, proxy: { server: `http://127.0.0.1:${await listen(proxy)}` } });
}
const en = privacyCopy.en;
try {
  await setup();
  const result = {};

  // ---- U7: the checkout boxes and exactly one PUT per ticked box, only after the order exists ----
  const c1 = await newContext();
  const { p: p1, puts: puts1 } = await checkoutPage(c1);
  await expect(p1.getByTestId("consent-marketing")).not.toBeChecked();
  await expect(p1.getByTestId("consent-ads")).not.toBeChecked();
  await expect(p1.getByTestId("consent-ads-disclosure")).toHaveText(en.adsDisclosure);
  await expect(p1.getByTestId("consent-choices")).toContainText(policy);
  const notice = await p1.getByTestId("consent-notice-link").getAttribute("href");
  assert.equal(new URL(notice, origin).pathname, "/en/privacy");
  assert.equal(new URL(notice, origin).searchParams.get("notice"), policy, "the notice link carries the policy version constant");
  assert.equal(await p1.getByTestId("consent-notice-link").getAttribute("target"), "_blank");
  assert.match((await p1.getByTestId("consent-notice-link").getAttribute("rel")) ?? "", /noopener/);
  await p1.getByTestId("consent-marketing").check();
  assert.equal(puts1.length, 0, "no consent request before the order exists");
  const order1 = await placeOrder(p1);
  await expect.poll(() => puts1.length).toBe(1);
  const body1 = JSON.parse(puts1[0].body);
  assert.deepEqual(Object.keys(body1).sort(), ["channel", "context", "granted", "purpose"], "the client never sends source or policy_version");
  assert.deepEqual(body1, { purpose: "marketing_messages", channel: "meta_dm", granted: true, context: "checkout" });
  assert.match(puts1[0].key, keyPattern);
  await pause(800);
  assert.equal(puts1.length, 1, "no PUT for the unticked ads box");
  const state1 = await buyerApi(p1, "GET", "privacy");
  assert.equal(state1.status, 200);
  assert.deepEqual(state1.body.consents, { marketing_messages: true, ads_personalization: false });
  result.order1 = order1;
  pass("U7 checkout: two unticked boxes, notice version, disclosure; one keyed PUT (context checkout) for the ticked box after the order; none for the other");

  const c2 = await newContext();
  const { p: p2, puts: puts2 } = await checkoutPage(c2);
  const order2 = await placeOrder(p2);
  await pause(1500);
  assert.equal(puts2.length, 0, "nothing ticked: no consent request at all (purchase never implies consent)");
  assert.deepEqual((await buyerApi(p2, "GET", "privacy")).body.consents, { marketing_messages: false, ads_personalization: false });
  result.order2 = order2;
  pass("U7 checkout: an order with both boxes unticked records no consent");

  const c3 = await newContext();
  await c3.route("**/api/buyer/consents", (route) => route.abort());
  const { p: p3, puts: puts3 } = await checkoutPage(c3);
  await p3.getByTestId("consent-marketing").check();
  await p3.getByTestId("consent-ads").check();
  const order3 = await placeOrder(p3);
  await expect.poll(() => puts3.length).toBe(2);
  await expect(p3.getByTestId("order-section")).toBeVisible();
  assert.equal((await p3.getByTestId("order-id").innerText()).trim(), order3);
  assert.deepEqual((await buyerApi(p3, "GET", "privacy")).body.consents, { marketing_messages: false, ads_personalization: false }, "a blocked consent write left nothing behind");
  result.order3 = order3;
  pass("U7 checkout: a failing consent request never blocks or undoes the order");

  // ---- U8: privacy page ----
  const c4 = await newContext();
  const p4 = await c4.newPage();
  const puts4 = watchConsents(p4);
  const posts4 = [];
  p4.on("request", (r) => { if (r.method() === "POST" && /\/api\/buyer\/privacy\/(export|erasure)$/.test(new URL(r.url()).pathname)) posts4.push({ path: new URL(r.url()).pathname, key: r.headers()["idempotency-key"] ?? null, body: r.postData() }); });
  await p4.goto(`${origin}/en/privacy?notice=${policy}`);
  await expect(p4.getByTestId("privacy-choices")).toBeVisible();
  await expect(p4.getByRole("heading", { level: 1 })).toHaveText(en.title);
  await expect(p4.getByTestId("privacy-notice")).toContainText(policy);
  for (const purpose of ["marketing_messages", "ads_personalization"]) await expect(p4.getByTestId(`privacy-${purpose}`)).toContainText(en.notGranted);
  await p4.getByTestId("privacy-toggle-marketing_messages").click();
  await expect(p4.getByTestId("privacy-marketing_messages")).toContainText(en.granted);
  await expect(p4.getByTestId("privacy-message")).toHaveText(en.saved);
  assert.equal(puts4.length, 1);
  assert.deepEqual(JSON.parse(puts4[0].body), { purpose: "marketing_messages", channel: "meta_dm", granted: true, context: "settings" });
  assert.match(puts4[0].key, keyPattern);
  await p4.getByTestId("privacy-toggle-ads_personalization").click();
  await expect(p4.getByTestId("privacy-ads_personalization")).toContainText(en.granted);
  await p4.getByTestId("privacy-toggle-ads_personalization").click();
  await expect(p4.getByTestId("privacy-ads_personalization")).toContainText(en.notGranted);
  assert.equal(puts4.length, 3);
  assert.equal(new Set(puts4.map((x) => x.key)).size, 3, "every toggle is its own request key");
  await p4.reload();
  await expect(p4.getByTestId("privacy-marketing_messages")).toContainText(en.granted); // state is server-side, not local storage
  assert.equal(await p4.evaluate(() => JSON.stringify({ ...localStorage, ...sessionStorage }).includes("marketing_messages")), false, "no privacy data in browser storage");
  // download: a JSON file, buyer envelope, no customer id
  const exportResponses = [];
  p4.on("response", (r) => { if (/\/api\/buyer\/privacy\/export$/.test(new URL(r.url()).pathname)) exportResponses.push(r); });
  const [download] = await Promise.all([p4.waitForEvent("download", { timeout: 15000 }), p4.getByTestId("privacy-download").click()]).catch(async (error) => {
    const seen = await Promise.all(exportResponses.map(async (r) => `${r.status()} ${(await r.text()).slice(0, 300)}`));
    throw new Error(`no download: message=${JSON.stringify(await p4.getByTestId("privacy-message").textContent().catch(() => null))} export responses=${JSON.stringify(seen)} cause=${error.message.split("\n")[0]}`);
  });
  const doc = JSON.parse(await readFile(await download.path(), "utf8"));
  assert.equal(doc.format, "lc.customer-export.v1");
  assert.equal("customer_id" in doc, false, "the buyer export has no customer id");
  assert.deepEqual(doc.consents.map((x) => x.granted).sort(), [false, true, true]);
  await expect(p4.getByTestId("privacy-message")).toHaveText(en.downloaded);
  assert.equal(posts4.filter((x) => x.path.endsWith("/export")).length, 1);
  assert.match(posts4[0].key, keyPattern);
  assert.ok(["", "{}"].includes(posts4[0].body ?? ""), "the browser-to-BFF export body is empty or the BFF `empty` shape {}; the Go hop takes none (internal/buyerhttp noBody)");
  pass("U8 privacy page: state from the server, toggles use context settings with their own keys, download is a keyed body-less POST");

  // erase: typed confirmation, one keyed request, the erased state; the reload starts a new anonymous session
  await p4.getByTestId("privacy-erase").click();
  const dialog = p4.getByTestId("privacy-erase-dialog");
  await expect(dialog).toBeVisible();
  const typed = p4.getByTestId("privacy-erase-typed"), submit = p4.getByTestId("privacy-erase-submit");
  await expect(dialog).toContainText(en.eraseKept);
  await expect(submit).toBeDisabled();
  for (const wrong of ["erase", "ERAS", "ERASE ", "Erase"]) { await typed.fill(wrong); await expect(submit).toBeDisabled(); }
  await p4.keyboard.press("Escape");
  await expect(dialog).toBeHidden();
  assert.equal(posts4.filter((x) => x.path.endsWith("/erasure")).length, 0);
  await p4.getByTestId("privacy-erase").click();
  await typed.fill("ERASE");
  const erasureResponses = [];
  p4.on("response", (r) => { if (/\/api\/buyer\/privacy\/erasure$/.test(new URL(r.url()).pathname)) erasureResponses.push(r); });
  await submit.click();
  await expect(p4.getByTestId("privacy-erased")).toBeVisible().catch(async (error) => {
    const seen = await Promise.all(erasureResponses.map(async (r) => `${r.status()} ${(await r.text()).slice(0, 300)}`));
    throw new Error(`no erased state: erasure responses=${JSON.stringify(seen)} cause=${error.message.split("\n")[0]}`);
  });
  await expect(p4.getByTestId("privacy-erased")).toContainText(en.erasedTitle);
  const erasures = posts4.filter((x) => x.path.endsWith("/erasure"));
  assert.equal(erasures.length, 1);
  assert.equal(erasures[0].body, JSON.stringify({ confirm: "ERASE" }));
  assert.match(erasures[0].key, keyPattern);
  await p4.reload();
  await expect(p4.getByTestId("privacy-choices")).toBeVisible();
  for (const purpose of ["marketing_messages", "ads_personalization"]) await expect(p4.getByTestId(`privacy-${purpose}`)).toContainText(en.notGranted);
  pass("U8 erase: typed ERASE, one keyed request, erased state; the capability is revoked, the next visit is a new anonymous session with nothing granted");

  // 410 erased -> the erased state (a buyer retry after a completed erasure)
  const c5 = await newContext();
  const p5 = await c5.newPage();
  await p5.route("**/api/buyer/privacy", (route) => route.request().method() === "GET" ? route.fulfill({ status: 410, contentType: "application/json", body: JSON.stringify({ code: "erased", message: "x", request_id: "", retryable: false, details: {} }) }) : route.continue());
  await p5.goto(`${origin}/en/privacy`);
  await expect(p5.getByTestId("privacy-erased")).toBeVisible();
  await expect(p5.getByTestId("privacy-choices")).toHaveCount(0);
  pass("U8: 410 erased renders the erased state and no controls");

  // erasure refused while an order is open (guest 1 has a DRAFT order): the message says why, nothing is erased
  const p1b = await c1.newPage();
  const eras1 = [];
  p1b.on("response", (r) => { if (r.request().method() === "POST" && new URL(r.url()).pathname.endsWith("/privacy/erasure")) eras1.push(r.status()); });
  await p1b.goto(`${origin}/en/privacy`);
  await expect(p1b.getByTestId("privacy-choices")).toBeVisible();
  await expect(p1b.getByTestId("privacy-marketing_messages")).toContainText(en.granted);
  await p1b.getByTestId("privacy-erase").click();
  await p1b.getByTestId("privacy-erase-typed").fill("ERASE");
  await p1b.getByTestId("privacy-erase-submit").click();
  await expect(p1b.getByTestId("privacy-erase-problem")).toHaveText(en.eraseBlocked);
  assert.deepEqual(eras1, [409]);
  await expect(p1b.getByTestId("privacy-erased")).toHaveCount(0);
  await p1b.keyboard.press("Escape");
  await p1b.reload();
  await expect(p1b.getByTestId("privacy-marketing_messages")).toContainText(en.granted);
  pass("U8 erase refused while an order is open: 409 erasure_blocked shown as text, consent intact");

  // ---- data deletion instructions: public, static, no API ----
  for (const locale of ["en", "zh-TW"]) {
    const c = await newContext();
    const p = await c.newPage();
    const urls = [];
    p.on("request", (r) => urls.push(r.url()));
    await p.goto(`${origin}/${locale}/data-deletion`);
    const words = privacyCopy[locale];
    await expect(p.getByRole("heading", { level: 1 })).toHaveText(words.deletionTitle);
    for (const step of words.deletionSteps) await expect(p.getByText(step, { exact: false }).first()).toBeVisible();
    await expect(p.locator(`a[href="/${locale}/privacy"]`).first()).toBeVisible();
    await p.waitForLoadState("networkidle");
    assert.deepEqual(urls.filter((u) => new URL(u).pathname.startsWith("/api/")), [], "the data-deletion page calls no API");
    assert.equal((await c.cookies(origin)).length, 0, "the public page sets no cookie");
    pass(`data-deletion instructions page (${locale}): public, static, no API call, no cookie`);
  }

  // ---- screenshots: en + zh-TW, desktop + 390 px ----
  for (const locale of ["en", "zh-TW"]) {
    for (const viewport of ["desktop", "mobile"]) {
      const c = await newContext(viewport === "mobile");
      const p = await c.newPage();
      await p.goto(`${origin}/${locale}/privacy`);
      await expect(p.getByTestId("privacy-choices")).toBeVisible();
      await expect(p.locator("html")).toHaveAttribute("lang", locale);
      await expect(p.getByRole("heading", { level: 1 })).toHaveText(privacyCopy[locale].title);
      await shot(p, "privacy", locale, viewport);
      await p.route("**/api/buyer/privacy", (route) => route.request().method() === "GET" ? route.fulfill({ status: 410, contentType: "application/json", body: JSON.stringify({ code: "erased", message: "x", request_id: "", retryable: false, details: {} }) }) : route.continue());
      await p.reload();
      await expect(p.getByTestId("privacy-erased")).toBeVisible();
      await shot(p, "privacy-erased", locale, viewport);
      await p.unroute("**/api/buyer/privacy");
      await p.goto(`${origin}/${locale}/data-deletion`);
      await expect(p.getByRole("heading", { level: 1 })).toHaveText(privacyCopy[locale].deletionTitle);
      await shot(p, "data-deletion", locale, viewport);
      // the checkout consent block in the locale under test (the in-memory form survives the locale switch)
      const { p: cp } = await checkoutPage(c);
      if (locale !== "en") {
        await cp.locator("header select").selectOption(locale);
        await expect(cp).toHaveURL(`${origin}/${locale}/products/${product}`);
      }
      await expect(cp.locator("html")).toHaveAttribute("lang", locale);
      await cp.getByTestId("consent-choices").scrollIntoViewIfNeeded();
      await expect(cp.getByTestId("consent-marketing")).not.toBeChecked();
      await expect(cp.getByTestId("consent-ads-disclosure")).toHaveText(privacyCopy[locale].adsDisclosure);
      await shot(cp, "checkout-consent", locale, viewport);
    }
  }
  await writeFile(path.join(evidence, "privacy-buyer-result.json"), JSON.stringify({ observations, ...result }, null, 2));
  console.log("PASS privacy-buyer.mjs");
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
