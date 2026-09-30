// MF07 admin half (contracts/manual-fulfilment-v1.md §5.1, §5.3, §5.4, §6). BFF routes exercised through the UI:
// PUT /api/stores/{store}/orders/{id}/shipment, GET .../shipment/history, GET .../orders/unshipped.csv,
// GET .../order-actions -> Go /v1/admin/stores/{store}/... (driven by tests/foundation/browser_refund_fulfilment_test.go).
//
// The admin shipment section is built by the refund-fulfilment-ui unit; these locators are derived from the contract
// and the UI brief (carrier names per Q3, "Mark shipped"/"Correct"/"Void", <details> history, "Export unshipped (CSV)").
// Everything that depends on UI wording lives in `ui` below so the UI unit can align it in one place. Until that unit
// merges the spec is NOT_RUN by construction (the sections do not exist yet).
import { expect, test, type Locator, type Page } from "@playwright/test";
import { createHash, randomBytes } from "node:crypto";
import { readFile, writeFile } from "node:fs/promises";
import path from "node:path";

const required = (name: string) => {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required`);
  return value;
};
const origin = required("LC_BROWSER_PUBLIC_ORIGIN");
const evidence = required("LC_BROWSER_EVIDENCE");
const store = required("LC_BROWSER_STORE");
const shipOrder = required("LC_BROWSER_SHIP_ORDER");
const reshipOrder = required("LC_BROWSER_RESHIP_ORDER");
const draftOrder = required("LC_BROWSER_DRAFT_ORDER");
const restrictedToken = required("LC_BROWSER_RESTRICTED_TOKEN");
const tracking = required("LC_BROWSER_TRACKING"); // leading zeroes on purpose
const cookieName = "__Host-commerce_session";
const csvHeader =
  "order_id,created_at_utc,service_code,destination_kind,recipient_name,phone,country,region,city,postal_code,line1,line2,pickup_namespace,pickup_code,pickup_name,pickup_address,items,total_minor,currency,pickup_source";

const carrierNames = {
  en: ["7-ELEVEN", "FamilyMart", "Hi-Life", "OK mart", "SF Express", "Chunghwa Post", "Other"],
  "zh-TW": ["7-ELEVEN 交貨便", "全家 店到店", "萊爾富", "OK mart", "順豐速運", "中華郵政", "其他"],
  "zh-CN": ["7-ELEVEN 交货便", "全家 店到店", "莱尔富", "OK mart", "顺丰速运", "中华邮政", "其他"],
} as const;
const carrierCodes = ["seven_eleven_cvs", "familymart_cvs", "hilife_cvs", "okmart_cvs", "sf_express", "chunghwa_post", "other"];

// All UI wording in one place (regexes admit the three locales; tighten when the UI unit publishes its copy).
const ui = {
  carrier: /carrier|物流|貨運|货运|承運|承运/i,
  carrierName: /carrier name|物流名稱|物流名称|承運商名稱|承运商名称/i,
  trackingNumber: /tracking number|物流單號|物流单号|追蹤單號|追踪单号|運單|运单/i,
  trackingUrl: /tracking (url|link)|追蹤連結|追踪链接|追蹤網址|追踪网址/i,
  note: /note|備註|备注/i,
  markShipped: /mark shipped|標記已出貨|标记已发货|標記為已出貨|标记为已发货/i,
  correct: /^(correct|更正|修正)/i,
  save: /save|update|儲存|保存|更新/i,
  void: /^(void|作廢|作废)/i,
  voidConfirm: /confirm|void|確認|确认|作廢|作废/i,
  voidReason: /reason|原因/i,
  history: /history|歷史|历史|紀錄|记录/i,
  exportButton: /export unshipped|匯出未出貨|导出未发货|匯出未寄出|导出未寄出/i,
  exportHint: /text|文字|文本/i,
  state: /state|status|filter|篩選|筛选|狀態|状态/i,
  delivered: /delivered|in transit|已送達|已送达|運送中|运送中/i,
};

test.use({ baseURL: origin, trace: "retain-on-failure", screenshot: "only-on-failure" });

async function signedLogin(page: Page) {
  await page.goto(new URL("/en/", origin).toString());
  await page.getByRole("button", { name: "Sign in with identity service" }).click();
  await expect(page.getByTestId("nav-orders")).toBeVisible();
  await openOrders(page, "en");
}

async function openOrders(page: Page, locale: string) {
  await page.goto(new URL(`/${locale}/orders?store=${store}`, origin).toString());
  await expect(page.getByTestId("merchant-orders")).toBeVisible();
  const selector = page.getByTestId("store-selector");
  // The selector only renders for members of more than one store (MerchantOrders.tsx); with a single
  // store the ?store= query above is what scopes the page, and the BFF rejects a foreign store.
  if (await selector.count()) {
    if ((await selector.inputValue()) !== store) await selector.selectOption(store);
  }
  await expect(page.getByTestId("orders-table")).toBeVisible();
}

async function expand(page: Page, id: string): Promise<Locator> {
  for (let pageNo = 0; pageNo < 3; pageNo++) {
    const button = page.getByTestId(`order-expand-${id}`);
    if (await button.count()) {
      if ((await button.getAttribute("aria-expanded")) !== "true") await button.click();
      const detail = page.getByTestId("order-detail");
      await expect(detail).toHaveAttribute("aria-label", new RegExp(id));
      return detail;
    }
    await expect(page.getByTestId("orders-next")).toBeEnabled();
    await page.getByTestId("orders-next").click();
    await expect(page.getByTestId("orders-table")).toBeVisible();
  }
  throw new Error("fixture order was absent from three real cursor pages");
}

async function asToken(context: import("@playwright/test").BrowserContext, token: string) {
  await context.clearCookies();
  // The admin UI's session fence hashes the readable companion cookie (settings-client.sessionBoundary), so a
  // swapped-in session needs its own __Host-commerce_csrf value exactly as the real callback would set it.
  const csrf = randomBytes(32).toString("base64url");
  const url = origin.replace(/^http:/, "https:");
  await context.addCookies([
    { name: cookieName, value: token, url, secure: true, httpOnly: true, sameSite: "Lax" },
    { name: "__Host-commerce_csrf", value: csrf, url, secure: true, httpOnly: false, sameSite: "Lax" },
  ]);
}

const manifestPath = path.join(evidence, "screenshots.json");
async function shot(page: Page, name: string, locale: string, viewport: "desktop" | "mobile") {
  const file = path.join(evidence, `${name}-${locale}-${viewport}.png`);
  await page.screenshot({ path: file, fullPage: false });
  expect(await page.evaluate(() => document.documentElement.scrollWidth - innerWidth)).toBeLessThanOrEqual(1);
  let manifest: unknown[] = [];
  try {
    manifest = JSON.parse(await readFile(manifestPath, "utf8"));
  } catch {
    /* first screenshot */
  }
  manifest.push({ File: path.basename(file), Sha256: createHash("sha256").update(await readFile(file)).digest("hex"), Locale: locale, Viewport: viewport });
  await writeFile(manifestPath, JSON.stringify(manifest, null, 2));
}

async function fillShipment(detail: Locator, code: string, number: string, url = "", note = "") {
  await detail.getByLabel(ui.carrier).first().selectOption(code);
  await detail.getByLabel(ui.trackingNumber).fill(number);
  if (url) await detail.getByLabel(ui.trackingUrl).fill(url);
  if (note) await detail.getByLabel(ui.note).fill(note);
}

test("MF07 merchant marks a paid order shipped, corrects, voids and re-ships; buyer-visible facts never say delivered", async ({ page }) => {
  await signedLogin(page);
  const requests: Array<{ method: string; url: string; key: string | null; body: string | null }> = [];
  page.on("request", (r) => {
    if (r.url().includes(`/orders/${shipOrder}/shipment`) && r.method() === "PUT")
      requests.push({ method: r.method(), url: r.url(), key: r.headers()["idempotency-key"] ?? null, body: r.postData() });
  });
  const detail = await expand(page, shipOrder);
  // the carrier select lists all seven §3.1 codes including 萊爾富 (hilife) and OK mart
  const select = detail.getByLabel(ui.carrier).first();
  await expect(select).toBeVisible();
  const values = await select.locator("option").evaluateAll((options) => options.map((o) => (o as HTMLOptionElement).value).filter(Boolean));
  expect(values).toEqual(expect.arrayContaining(carrierCodes));
  expect(values.filter((v) => carrierCodes.includes(v))).toHaveLength(7);
  const labels = await select.locator("option").allTextContents();
  for (const name of carrierNames.en) expect(labels.join("|")).toContain(name);
  // record: 7-ELEVEN, leading-zero tracking number, https link, merchant-only note
  await fillShipment(detail, "seven_eleven_cvs", tracking, `https://track.example.com/t?n=${tracking}`, "MF07 internal note");
  await detail.getByRole("button", { name: ui.markShipped }).click();
  await expect(detail.locator('[data-state="MERCHANT_SHIPPED"]').first()).toBeVisible();
  await expect(detail).toContainText(tracking);
  await expect(detail).not.toContainText(ui.delivered);
  // exactly one PUT, keyed, strict body: eight keys, currency-free, nulls explicit
  expect(requests).toHaveLength(1);
  expect(requests[0].key).toMatch(/^[A-Za-z0-9_.:-]{8,128}$/);
  const first = JSON.parse(requests[0].body ?? "{}") as Record<string, unknown>;
  expect(Object.keys(first).sort()).toEqual(["carrier_code", "carrier_name", "expected_version", "note", "status", "tracking_number", "tracking_url", "void_reason"]);
  expect(first).toMatchObject({ expected_version: 0, status: "SHIPPED", carrier_code: "seven_eleven_cvs", tracking_number: tracking, carrier_name: null, void_reason: null });
  // correct the carrier/tracking (typo): a new version, still MERCHANT_SHIPPED
  await detail.getByRole("button", { name: ui.correct }).click();
  await fillShipment(detail, "sf_express", "SF7777");
  await detail.getByRole("button", { name: ui.save }).click();
  await expect(detail).toContainText("SF7777");
  await expect(detail.locator('[data-state="MERCHANT_SHIPPED"]').first()).toBeVisible();
  expect(requests).toHaveLength(2);
  expect(JSON.parse(requests[1].body ?? "{}")).toMatchObject({ expected_version: 1, status: "SHIPPED", carrier_code: "sf_express" });
  expect(requests[1].key).not.toBe(requests[0].key); // one Idempotency-Key per submission
  // void: the void body carries no carrier/tracking/note, only a reason
  await detail.getByRole("button", { name: ui.void }).click();
  await detail.getByLabel(ui.voidReason).selectOption("wrong_tracking");
  await detail.getByRole("button", { name: ui.voidConfirm }).last().click();
  await expect(detail.locator('[data-state="MANUAL_UNASSIGNED"]').first()).toBeVisible();
  expect(requests).toHaveLength(3);
  expect(JSON.parse(requests[2].body ?? "{}")).toMatchObject({ expected_version: 2, status: "VOIDED", void_reason: "wrong_tracking", carrier_code: null, carrier_name: null, tracking_number: null, tracking_url: null, note: null });
  // re-ship
  await fillShipment(detail, "seven_eleven_cvs", tracking, `https://track.example.com/t?n=${tracking}`, "MF07 internal note");
  await detail.getByRole("button", { name: ui.markShipped }).click();
  await expect(detail.locator('[data-state="MERCHANT_SHIPPED"]').first()).toBeVisible();
  expect(requests).toHaveLength(4);
  expect(JSON.parse(requests[3].body ?? "{}")).toMatchObject({ expected_version: 3, status: "SHIPPED" });
  // history disclosure: all four versions, merchant-only note/void reason visible to the merchant
  const history = detail.locator("details").filter({ hasText: ui.history }).first();
  await history.locator("summary").click();
  await expect(history).toContainText("MF07 internal note");
  await expect(history).toContainText(/wrong[ _]tracking|錯誤單號|错误单号/i);
  await expect(history).toContainText(/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/); // principal UUID, merchants only
  await expect(page.locator("body")).not.toContainText(ui.delivered);
  // the list filters follow the head
  const filter = page.getByLabel(ui.state).first();
  if (await filter.count()) {
    await filter.selectOption("shipped");
    await expect(page.getByTestId(`order-expand-${shipOrder}`)).toBeVisible();
    await expect(page.getByTestId(`order-expand-${reshipOrder}`)).toHaveCount(0);
    await filter.selectOption("unshipped");
    await expect(page.getByTestId(`order-expand-${reshipOrder}`)).toBeVisible();
    await expect(page.getByTestId(`order-expand-${shipOrder}`)).toHaveCount(0);
    await expect(page.getByTestId(`order-expand-${draftOrder}`)).toHaveCount(0);
    await filter.selectOption("all");
  }
});

test("MF07 export downloads a CSV that opens with the expected header and lists only unshipped paid orders", async ({ page }) => {
  await signedLogin(page);
  const button = page.getByRole("link", { name: ui.exportButton }); // a plain download <a>, streamed by the BFF
  await expect(button).toBeVisible();
  // the text-column import hint sits next to the button (§5.3)
  await expect(button.locator("xpath=..")).toContainText(ui.exportHint);
  const [download] = await Promise.all([page.waitForEvent("download"), button.click()]);
  expect(download.suggestedFilename()).toMatch(/^unshipped-[0-9a-f]{8}-\d{12}\.csv$/);
  const target = path.join(evidence, "unshipped-export.csv");
  await download.saveAs(target);
  const bytes = await readFile(target);
  expect([...bytes.subarray(0, 3)]).toEqual([0xef, 0xbb, 0xbf]); // BOM
  const text = bytes.toString("utf8").replace(/^﻿/, "");
  expect(text.startsWith(csvHeader + "\r\n")).toBe(true);
  expect(text).toContain(reshipOrder); // paid, unshipped
  expect(text).not.toContain(shipOrder); // shipped in the previous test
  expect(text).not.toContain(draftOrder); // a DRAFT order is never eligible
  // the file is not kept by the page
  const storage = await page.evaluate(() => JSON.stringify({ l: { ...localStorage }, s: { ...sessionStorage } }));
  expect(storage).not.toContain("recipient_name");
  expect(storage).not.toContain(csvHeader);
});

test("MF07 a member without fulfillment:write / orders:export sees no shipment action and no export", async ({ page, context }) => {
  await signedLogin(page);
  await asToken(context, restrictedToken);
  await openOrders(page, "en");
  const detail = await expand(page, reshipOrder);
  await expect(detail).toBeVisible();
  await expect(detail.getByRole("button", { name: ui.markShipped })).toHaveCount(0);
  await expect(detail.getByLabel(ui.trackingNumber)).toHaveCount(0);
  await expect(detail.getByRole("button", { name: ui.correct })).toHaveCount(0);
  await expect(detail.getByRole("button", { name: ui.void })).toHaveCount(0);
  await expect(page.getByRole("link", { name: ui.exportButton })).toHaveCount(0);
  await expect(page.getByRole("button", { name: ui.exportButton })).toHaveCount(0);
  // the BFF still refuses writes and export for that session (server is the authority)
  const put = await page.evaluate(async ({ store, order }) => {
    const r = await fetch(`/api/stores/${store}/orders/${order}/shipment`, { method: "PUT", headers: { "content-type": "application/json", "Idempotency-Key": "mf07-restricted-key", "X-CSRF-Token": document.cookie.split("; ").find((c) => c.startsWith("__Host-commerce_csrf="))?.slice(21) ?? "" }, body: "{}" });
    return r.status;
  }, { store, order: reshipOrder });
  expect([403, 422, 401]).toContain(put);
  const csv = await page.evaluate(async (s) => (await fetch(`/api/stores/${s}/orders/unshipped.csv`)).status, store);
  expect(csv).toBe(403);
});

for (const locale of ["en", "zh-TW", "zh-CN"] as const) {
  test(`MF07 ${locale}: carrier names and no overflow at desktop and 390 px`, async ({ page }) => {
    await signedLogin(page);
    await openOrders(page, locale);
    const detail = await expand(page, reshipOrder);
    const select = detail.getByLabel(ui.carrier).first();
    await expect(select).toBeVisible();
    const labels = (await select.locator("option").allTextContents()).join("|");
    for (const name of carrierNames[locale]) expect(labels).toContain(name);
    await page.setViewportSize({ width: 1586, height: 992 });
    await shot(page, "shipment", locale, "desktop");
    await page.setViewportSize({ width: 390, height: 844 });
    await expect(detail).toBeVisible();
    await shot(page, "shipment", locale, "mobile");
    await page.setViewportSize({ width: 1586, height: 992 });
  });
}
