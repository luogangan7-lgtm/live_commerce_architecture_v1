// TCV08 buyer half + TCV14 buyer_entered UI half (contracts/taiwan-cvs-logistics-v1.md §5.2, §5.3, §16.1, §16.2, §16.4, §10 TCV08/TCV14).
// Real production storefront Next -> private buyerhttp -> isolated PG, driven by tests/foundation/browser_taiwan_cvs_test.go (MOCK variant).
// The browser reaches only synthetic hosts through one CONNECT proxy and one self-signed edge:
//   buyer.example / buyer2.example / buyer3.example -> the storefront Next  (three stores, three ACTIVE domains)
//   logistics-stage.ecpay.com.tw                     -> the ecpaytest fake over HTTP (the stage map: answers the auto-post form with its fixed store)
//   hooks.tcv.example                                -> the Go hooks handler (POST /v1/cvs/ecpay/map-return/{id}, the unauthenticated provider return)
// Nothing else is reachable. BFF routes exercised: /api/buyer/{checkout-options,quotes,cvs-selections,cvs-selections/{id}/verify,cvs-stores,destination,
// checkout,orders/{id}} -> Go /v1/buyer/*. Evidence: screenshots (desktop 1440x900 and 390x844, three locales) hashed into screenshots.json.
// Wording: contract strings for zh-TW (§5.3/§16.1/§16.4, cvs-ui U2/U3/U4); the rest is located through data-testid.
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import http from "node:http";
import https from "node:https";
import net from "node:net";
import { spawn, execFileSync } from "node:child_process";
import { once } from "node:events";
import { readFile, writeFile, mkdtemp } from "node:fs/promises";
import { createWriteStream } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { expect } from "@playwright/test";
import { engine, launch, ctxOpts, phone, iosZoomOffenders } from "./browser-engine.mjs"; // LC_BROWSER_ENGINE=chromium|webkit; chromium behaviour is unchanged

const env = (name) => {
  const value = process.env[name];
  assert(value, `${name} is required`);
  return value;
};
const root = process.cwd(), evidence = env("LC_CVS_EVIDENCE");
const stores = {
  map1: { origin: env("LC_CVS_ORIGIN1"), product: env("LC_CVS_PRODUCT1") },
  map2: { origin: env("LC_CVS_ORIGIN2"), product: env("LC_CVS_PRODUCT2") },
  entered: { origin: env("LC_CVS_ORIGIN3"), product: env("LC_CVS_PRODUCT3") },
};
const fakeURL = env("LC_CVS_FAKE_URL"), hooksURL = env("LC_CVS_HOOKS_URL");
const hosts = new Set([...Object.values(stores).map((s) => new URL(s.origin).host), "logistics-stage.ecpay.com.tw", "hooks.tcv.example"]);
const children = new Set(), sockets = new Set(), contexts = [], logs = [];
const pause = (ms) => new Promise((r) => setTimeout(r, ms));
const listen = async (s) => { s.listen(0, "127.0.0.1"); await once(s, "listening"); return s.address().port; };
const certDir = await mkdtemp(path.join(tmpdir(), "lc-cvs-edge-"));
const pass = (name) => console.log(`PASS ${name}`);
let browser, edge, proxy;
const iframeSeen = [];
const mapPosts = []; // every form the browser POSTed to the stage map: { mobile, device }

const copy = {
  "zh-TW": {
    delivery: "選擇配送", quote: "取得目前總額", pick: "選擇門市", change: "更換門市",
    consent: "取件人姓名與手機將提供給物流商與門市，用於通知與取貨。", enteredLabel: "你填寫的門市",
    comingSoon: "即將開放", chains: { cvs_711: "7-ELEVEN", cvs_familymart: "全家", cvs_hilife: "萊爾富", cvs_okmart: "OK超商" },
  },
  "zh-CN": {
    delivery: "选择配送", quote: "获取当前总额", pick: /选择门[市店]/, change: /更换门[市店]/,
    consent: "取件人姓名与手机将提供给物流商与门店，用于通知与取货。", comingSoon: "即将开放",
    chains: { cvs_711: "7-ELEVEN", cvs_familymart: "全家", cvs_hilife: "莱尔富", cvs_okmart: "OK超商" },
  },
  en: {
    delivery: "Choose delivery", quote: "Get current total", pick: "Choose store", change: "Change store",
    consent: "The recipient's name and mobile are shared with the carrier and store for pickup notices.", comingSoon: "Coming soon",
    chains: { cvs_711: "7-ELEVEN", cvs_familymart: "FamilyMart", cvs_hilife: "Hi-Life", cvs_okmart: "OK mart" },
  },
};

function relayTo(target, req, body, extra = {}) {
  return new Promise((resolve, reject) => {
    const headers = { ...req.headers, ...extra }; delete headers.connection; delete headers["transfer-encoding"];
    if (body.length) headers["content-length"] = String(body.length); else delete headers["content-length"];
    const call = http.request({ hostname: target.hostname, port: target.port, path: req.url, method: req.method, headers }, (res) => {
      const chunks = []; res.on("data", (x) => chunks.push(x)); res.on("error", reject);
      res.on("end", () => resolve({ status: res.statusCode, headers: res.headers, body: Buffer.concat(chunks) }));
    });
    call.setTimeout(20000, () => call.destroy(new Error("owned relay deadline"))); call.on("error", reject); call.end(body);
  });
}
async function startNext() {
  const reserve = net.createServer(), port = await listen(reserve); await new Promise((r) => reserve.close(r));
  const log = createWriteStream(path.join(evidence, "cvs-next.log"), { flags: "wx", mode: 0o600 }); logs.push(log); await once(log, "open");
  const childEnv = { ...process.env, NODE_ENV: "production", NEXT_TELEMETRY_DISABLED: "1" };
  for (const key of Object.keys(childEnv)) if (key.startsWith("LC_CVS_")) delete childEnv[key];
  const child = spawn(process.execPath, [path.join(root, "apps/storefront/node_modules/next/dist/bin/next"), "start", "--hostname", "127.0.0.1", "--port", String(port)],
    { cwd: path.join(root, "apps/storefront"), env: childEnv, stdio: ["ignore", log, log] });
  children.add(child);
  for (let i = 0; i < 100; i++) {
    if (child.exitCode !== null) throw new Error("owned Next failed readiness");
    try { if ((await relayTo({ hostname: "127.0.0.1", port }, { url: "/api/buyer/session", method: "GET", headers: { host: new URL(stores.map1.origin).host } }, Buffer.alloc(0))).status === 200) return port; } catch { /* not ready */ }
    await pause(50);
  }
  throw new Error("owned Next readiness timeout");
}
const manifest = path.join(evidence, "screenshots.json");
async function shot(page, name, locale, viewport) {
  const file = path.join(evidence, `cvs-${name}-${locale}-${viewport}.png`);
  await page.screenshot({ path: file, fullPage: true });
  assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), `horizontal overflow at ${viewport} ${locale}`);
  // Engine-specific by nature: the focus-zoom rule exists only on iOS Safari, so it is asserted on the iPhone profile (webkit, phone viewport) only.
  if (engine === "webkit" && viewport === "mobile") assert.deepEqual(await iosZoomOffenders(page), [], `iOS focus-zoom: form controls under 16px at ${viewport} ${locale}`);
  let list = []; try { list = JSON.parse(await readFile(manifest, "utf8")); } catch { /* first */ }
  list.push({ File: path.basename(file), Sha256: createHash("sha256").update(await readFile(file)).digest("hex"), Locale: locale, Viewport: viewport });
  await writeFile(manifest, JSON.stringify(list, null, 2));
}
async function newContext(mobile) {
  const c = await browser.newContext(ctxOpts(mobile
    ? { ...phone, viewport: { width: 390, height: 844 }, screen: { width: 390, height: 844 }, ignoreHTTPSErrors: true }
    : { ignoreHTTPSErrors: true, viewport: { width: 1440, height: 900 } }));
  contexts.push(c);
  c.on("page", (p) => {
    p.on("frameattached", (f) => { if (f !== p.mainFrame()) iframeSeen.push(f.url()); });
    p.on("pageerror", (e) => console.log("PAGEERROR", e.message));
    p.on("response", async (r) => { if (r.url().includes("/api/buyer/") && r.status() >= 400) console.log("HTTP", r.status(), r.request().method(), new URL(r.url()).pathname, (await r.text().catch(() => "")).slice(0, 200)); });
  });
  return c;
}
// Opens the product page, chooses the delivery option by chain and asks for the quotation.
async function toQuote(page, store, locale, kind, quantity = 1) {
  await page.goto(`${store.origin}/${locale}/products/${store.product}`);
  // a pay-at-pickup total must be a whole TWD amount (F20: GoodsAmount/CollectionAmount are integers); the fixture SKU costs TWD 12.50
  for (let i = 1; i < quantity; i++) await page.getByRole("button", { name: "Increase quantity", exact: true }).click();
  await page.getByRole("button", { name: copy[locale].delivery, exact: true }).click();
  await page.locator("#delivery").selectOption({ label: `${copy[locale].chains[kind]} · TW` });
  await page.getByRole("button", { name: copy[locale].quote, exact: true }).click();
  await expect(page.getByTestId("address-section")).toBeVisible();
}
async function mapRoundTrip(page, ctx, store, locale, kind, label) {
  await page.getByTestId("cvs-recipient-name").fill("王小明");
  await page.getByTestId("cvs-recipient-phone").fill("0912345678");
  const before = page.url();
  await page.getByTestId("cvs-pick-store").click();
  // the same tab leaves for the stage map and comes back to the storefront: no popup, no iframe
  await page.waitForURL((u) => u.toString().startsWith(store.origin) && u.searchParams.has("cvs_selection"), { timeout: 30000 });
  assert.equal(contexts.flatMap((c) => c.pages()).filter((p) => !p.isClosed()).length >= 1, true);
  assert(new URL(page.url()).pathname === new URL(before).pathname, "the map must return to the page it left");
  const cookies = await ctx.cookies(store.origin);
  assert(cookies.some((c) => c.name === "__Host-commerce_buyer"), `${label}: the __Host-commerce_buyer cookie must be present after the return on ${store.origin}`);
  const returned = new URL(page.url());
  assert.equal(returned.origin, store.origin, `${label}: the map return must land on the originating store's host`);
  for (const key of returned.searchParams.keys()) assert.equal(key, "cvs_selection", `no store data in the return URL (${key})`);
}
async function showsStoreCard(page, store, locale, kind, label) {
  // after the return the page re-verifies; if the delivery section is closed again, reopen it (cart and draft persist)
  const card = page.getByTestId("cvs-store-card");
  if (!(await card.isVisible().catch(() => false))) {
    try { await expect(card).toBeVisible({ timeout: 8000 }); } catch {
      await page.getByRole("button", { name: copy[locale].delivery, exact: true }).click().catch(() => {});
      await page.locator("#delivery").selectOption({ label: `${copy[locale].chains[kind]} · TW` }).catch(() => {});
      await page.getByRole("button", { name: copy[locale].quote, exact: true }).click().catch(() => {});
      await expect(card).toBeVisible({ timeout: 20000 });
    }
  }
  await expect(page.getByTestId("cvs-store-name")).toContainText("Stage 7-ELEVEN");
  await expect(page.getByTestId("cvs-store-code")).toContainText("131386");
  await expect(page.getByTestId("cvs-consent")).toHaveText(copy[locale].consent);
  await expect(page.getByTestId("cvs-change-store")).toBeVisible();
  assert.equal(await page.locator("iframe").count(), 0, `${label}: no iframe in the DOM`);
  const text = await card.innerText();
  assert(!/已驗證|已验证|verified/i.test(text), `${label}: a directory store card must not claim more than the contract says`);
}

try {
  execFileSync("openssl", ["req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", path.join(certDir, "key.pem"), "-out", path.join(certDir, "cert.pem"), "-days", "1", "-subj", "/CN=cvs.example"], { stdio: "ignore" });
  const nextPort = await startNext();
  const fake = new URL(fakeURL), hooks = new URL(hooksURL);
  edge = https.createServer({ key: await readFile(path.join(certDir, "key.pem")), cert: await readFile(path.join(certDir, "cert.pem")) }, async (req, res) => {
    try {
      const chunks = []; for await (const x of req) chunks.push(x);
      const body = Buffer.concat(chunks), host = String(req.headers.host).replace(/:443$/, "");
      let out;
      if (host === "logistics-stage.ecpay.com.tw") mapPosts.push({ mobile: /Mobile|Android|iPhone/i.test(String(req.headers["user-agent"])), device: new URLSearchParams(body.toString()).get("Device") });
      if (host === "logistics-stage.ecpay.com.tw") out = await relayTo(fake, req, body, { "x-ecpay-host": host, host: fake.host });
      else if (host === "hooks.tcv.example") out = await relayTo(hooks, req, body, { host: hooks.host });
      else out = await relayTo({ hostname: "127.0.0.1", port: nextPort }, req, body);
      const headers = { ...out.headers }; delete headers.connection; delete headers["transfer-encoding"];
      res.writeHead(out.status, headers); res.end(out.body);
    } catch { if (!res.headersSent) res.writeHead(502); res.end(); }
  });
  const edgePort = await listen(edge);
  proxy = http.createServer((_, r) => { r.writeHead(403); r.end(); });
  proxy.on("connect", (req, socket, head) => {
    const [h, p] = req.url.split(":");
    if (!hosts.has(req.url) && !(p === "443" && hosts.has(h))) { socket.destroy(); return; }
    const upstream = net.connect(edgePort, "127.0.0.1", () => { socket.write("HTTP/1.1 200 Connection Established\r\n\r\n"); if (head.length) upstream.write(head); socket.pipe(upstream).pipe(socket); });
    for (const s of [socket, upstream]) { sockets.add(s); s.on("close", () => sockets.delete(s)); s.on("error", () => { socket.destroy(); upstream.destroy(); }); }
  });
  browser = await launch({ headless: true, proxy: { server: `http://127.0.0.1:${await listen(proxy)}` } });

  // ---- store 1: ecpay_map, every locale, desktop and phone ------------------------------------------------------------
  for (const [locale, mobile] of [["zh-TW", false], ["en", true], ["zh-CN", false], ["zh-TW", true]]) {
    const viewport = mobile ? "mobile" : "desktop";
    const ctx = await newContext(mobile);
    const page = await ctx.newPage();
    await toQuote(page, stores.map1, locale, "cvs_711");
    if (locale === "zh-TW") await expect(page.getByTestId("cvs-pick-store")).toHaveText("選擇門市");
        await mapRoundTrip(page, ctx, stores.map1, locale, "cvs_711", `store1 ${locale}/${viewport}`);
    await showsStoreCard(page, stores.map1, locale, "cvs_711", `store1 ${locale}/${viewport}`);
    await shot(page, "store-card", locale, viewport);
    // the draft survived the round trip (the whole page was replaced), and the recipient rules (F5/C3) are field errors at confirm time
    await expect(page.getByTestId("cvs-recipient-name")).toHaveValue("王小明");
    await page.getByTestId("cvs-recipient-name").fill("A1");
    await page.getByTestId("cvs-recipient-phone").fill("0212345678");
    await page.getByTestId("confirm-address").click();
    await expect(page.getByTestId("cvs-recipient-error")).toBeVisible();
    await shot(page, "picker-errors", locale, viewport);
    await page.getByTestId("cvs-recipient-name").fill("王小明");
    await page.getByTestId("cvs-recipient-phone").fill("0912345678");
    await page.getByTestId("confirm-address").click();
    await expect(page.getByTestId("create-order")).toBeEnabled();
    if (locale === "zh-TW" && !mobile) {
      // change store, pick again: the second round trip works and the card returns
      await page.getByTestId("cvs-change-store").click();
      await expect(page.getByTestId("cvs-store-card")).toHaveCount(0);
      await expect(page.getByTestId("cvs-pick-store")).toBeVisible();
      await mapRoundTrip(page, ctx, stores.map1, locale, "cvs_711", "store1 change");
      await showsStoreCard(page, stores.map1, locale, "cvs_711", "store1 change");
    }
    pass(`store 1 map round trip ${locale}/${viewport}: same tab, __Host- cookie kept, store card, consent line, no iframe`);
    await ctx.close();
  }

  // ---- store 2 on its own host: the return lands there, with that host's cookie ---------------------------------------------
  {
    const ctx = await newContext(false), page = await ctx.newPage();
    await toQuote(page, stores.map2, "en", "cvs_711", 2);
    await mapRoundTrip(page, ctx, stores.map2, "en", "cvs_711", "store2");
    await showsStoreCard(page, stores.map2, "en", "cvs_711", "store2");
    assert.equal((await ctx.cookies(stores.map1.origin)).length, 0, "the store 2 buyer has no cookie of store 1's host");
    // pay at pickup: no Stripe step, the placed order shows the pending collection (§16.2, §16.4)
    await page.getByRole("radio", { name: /Pay at pickup/i }).check();
    await expect(page.getByTestId("cvs-pay-note")).toBeVisible();
    await page.getByTestId("confirm-address").click();
    await expect(page.getByTestId("create-order")).toBeEnabled();
    await page.getByTestId("create-order").click();
    try { await expect(page.getByTestId("order-section")).toBeVisible({ timeout: 30000 }); } catch (e) {
      console.log("ALERTS", JSON.stringify(await page.locator("[role=alert], .purchase-error, [data-testid=cvs-create-error]").allInnerTexts()));
      await shot(page, "pay-at-pickup-failed", "en", "desktop"); throw e;
    }
    await expect(page.getByTestId("order-collection")).toHaveAttribute("data-state", "PENDING");
    await expect(page.getByTestId("order-collection")).toContainText(/Pay at pickup/i);
    assert.equal(await page.getByTestId("pay-order").count(), 0, "a pay-at-pickup order has no card payment step");
    await shot(page, "pay-at-pickup-order", "en", "desktop");
    pass("store 2: the map return lands on the originating store's host with its own __Host- cookie and shows the store card");
    await ctx.close();
  }

  // ---- unverified chains are shown disabled "coming soon" -------------------------------------------------------------------
  {
    const ctx = await newContext(false), page = await ctx.newPage();
    await page.goto(`${stores.map1.origin}/zh-TW/products/${stores.map1.product}`);
    await page.getByRole("button", { name: copy["zh-TW"].delivery, exact: true }).click();
    for (const kind of ["cvs_hilife", "cvs_okmart"]) {
      const option = page.locator("#delivery option", { hasText: copy["zh-TW"].chains[kind] });
      await expect(option).toHaveCount(1);
      await expect(option).toBeDisabled();
      assert((await option.innerText()).includes(copy["zh-TW"].comingSoon), `${kind} must read ${copy["zh-TW"].comingSoon}`);
    }
    pass("OK mart and Hi-Life are disabled with 即將開放 until verified");
    await ctx.close();
  }

  // ---- buyer_entered store (no ECPay profile): TCV14 UI half ----------------------------------------------------------------
  for (const [locale, mobile] of [["zh-TW", false], ["en", true], ["zh-CN", false]]) {
    const viewport = mobile ? "mobile" : "desktop";
    const ctx = await newContext(mobile), page = await ctx.newPage();
    await toQuote(page, stores.entered, locale, "cvs_711");
    await expect(page.getByTestId("cvs-pick-store")).toHaveCount(0); // no map in buyer_entered mode
    const link = page.getByTestId("cvs-search-link");
    await expect(link).toBeVisible();
    assert.equal(await link.getAttribute("href"), "https://emap.pcsc.com.tw/");
    assert.equal(await link.getAttribute("target"), "_blank");
    const rel = ((await link.getAttribute("rel")) ?? "").split(/\s+/);
    for (const token of ["noopener", "noreferrer"]) assert(rel.includes(token), `search link rel lacks ${token}`);
    await page.getByTestId("cvs-recipient-name").fill("王小明");
    await page.getByTestId("cvs-recipient-phone").fill("0912345678");
    await page.getByTestId("cvs-entered-code").fill("12345"); // 7-ELEVEN needs 6 digits
    await page.getByTestId("cvs-entered-name").fill("測試門市");
    await page.getByTestId("cvs-entered-address").fill("台北市測試路1號");
    await page.getByTestId("confirm-address").click();
    await expect(page.getByTestId("cvs-entered-code")).toHaveAttribute("aria-invalid", "true"); // a field error from the code format, no request needed
    await page.getByTestId("cvs-entered-code").fill("123456");
    await page.getByTestId("confirm-address").click();
    await expect(page.getByTestId("create-order")).toBeEnabled();
    await shot(page, "entered", locale, viewport);
    const text = await page.getByTestId("address-section").innerText();
    assert(!/已驗證|已验证|\bverified\b/i.test(text), "a buyer-typed store is never labelled verified");
    if (locale === "zh-TW") assert(text.includes(copy["zh-TW"].enteredLabel), "the zh-TW label 你填寫的門市 must be shown");
    pass(`buyer_entered ${locale}/${viewport}: official search link (new tab, noopener), 422-style field errors, never labelled verified`);
    await ctx.close();
  }
  assert.equal(iframeSeen.length, 0, `an iframe was attached: ${iframeSeen}`);
  pass("no iframe was ever attached to any page (F4)");
  // B20: the buyer BFF sends Device=1 for mobile user agents and 0 otherwise (buyers arrive from Facebook / Instagram on phones)
  assert(mapPosts.length >= 6 && mapPosts.some((m) => m.mobile) && mapPosts.some((m) => !m.mobile), `map posts seen: ${JSON.stringify(mapPosts)}`);
  for (const m of mapPosts) assert.equal(m.device, m.mobile ? "1" : "0", `B20: Device=${m.device} for a ${m.mobile ? "mobile" : "desktop"} browser`);
  pass("B20 Device=1 for phones, 0 for desktops in every map form");
} finally {
  for (const c of contexts) await c.close().catch(() => {});
  await browser?.close().catch(() => {});
  for (const s of sockets) s.destroy();
  proxy?.close(); edge?.close();
  for (const child of children) child.kill("SIGKILL");
  for (const log of logs) log.end();
}
