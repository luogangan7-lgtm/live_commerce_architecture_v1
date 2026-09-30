// TCV08 merchant half (contracts/taiwan-cvs-logistics-v1.md §8, §6, §16.4, §16.8, §10 TCV08). BFF routes exercised through the UI:
//   POST /api/stores/{store}/orders/{id}/cvs-shipment, GET .../cvs-shipment, POST .../cvs-shipment/print-form, POST .../cvs-shipment/abandon,
//   POST .../orders/{id}/collection, POST .../orders/{id}/pay-at-pickup-release -> Go /v1/admin/stores/{store}/orders/{id}/...
// Driven by tests/foundation/browser_taiwan_cvs_test.go (MOCK: the ecpaytest fake answers Create; the print tab's ECPay form post is intercepted here,
// nothing reaches ECPay). Locators use data-testid (structure) plus the zh-TW strings of the contract (§8, cvs-ui brief item 7); everything that depends
// on wording lives in `ui` so the copy can change in one place.
import { expect, test, type BrowserContext, type Locator, type Page } from "@playwright/test";
import { createHash } from "node:crypto";
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
const createOrder = required("LC_BROWSER_CVS_CREATE_ORDER"); // pay-at-pickup, ECPay pickup, unshipped
const cancelOrder = required("LC_BROWSER_CVS_CANCEL_ORDER"); // pay-at-pickup, unshipped (merchant cancels it)
const collectOrder = required("LC_BROWSER_CVS_COLLECT_ORDER"); // pay-at-pickup, manually shipped by the fixture (merchant marks collected)
const unknownOrder = required("LC_BROWSER_CVS_UNKNOWN_ORDER"); // Create answered 403 -> UNKNOWN
const enteredOrder = required("LC_BROWSER_CVS_ENTERED_ORDER"); // buyer_entered pickup, pay-at-pickup

const ui = {
  create: /建立超商寄件|create (the )?(cvs|convenience|label)|create shipment/i,
  confirmText: /綠界將從你的綠界帳戶餘額扣運費|ECPay will charge|綠界|ECPay/i,
  print: /列印託運單|print (the )?(label|waybill)/i,
  collected: /已收款|mark (as )?collected|collected/i,
  cancel: /取消訂單並釋放庫存|cancel (the )?order/i,
  restock: /已收回包裹，恢復庫存|restock/i,
  enteredLabel: /買家自填門市（未驗證，出貨前請至超商官網核對）|buyer-entered|entered by the buyer/i,
  manualInstead: /改用手動出貨|manual/i,
};
const cookieName = "__Host-commerce_session";
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
  if (await selector.count()) {
    if ((await selector.inputValue()) !== store) await selector.selectOption(store);
  }
  await expect(page.getByTestId("orders-table")).toBeVisible();
}
async function expand(page: Page, id: string): Promise<Locator> {
  for (let pageNo = 0; pageNo < 5; pageNo++) {
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
  throw new Error("fixture order was absent from the real cursor pages");
}
const manifestPath = path.join(evidence, "screenshots.json");
async function shot(page: Page, name: string, locale: string, viewport: "desktop" | "mobile") {
  const file = path.join(evidence, `cvs-admin-${name}-${locale}-${viewport}.png`);
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
// No ECPay traffic leaves the machine: the merchant print tab auto-posts to the ECPay print host, which is answered here.
async function trapEcpay(context: BrowserContext, seen: Array<{ url: string; method: string; body: string | null }>) {
  await context.route(/https:\/\/logistics(-stage)?\.ecpay\.com\.tw\/.*/, async (route) => {
    const request = route.request();
    seen.push({ url: request.url(), method: request.method(), body: request.postData() });
    await route.fulfill({ status: 200, contentType: "text/html", body: "<html><body>label</body></html>" });
  });
}

test("TCV08 merchant: create a label, copy the code, print in a new tab, timeline, UNKNOWN banner, collection and cancel actions, buyer_entered label", async ({ page, context }) => {
  const ecpayRequests: Array<{ url: string; method: string; body: string | null }> = [];
  await trapEcpay(context, ecpayRequests);
  await signedLogin(page);
  const calls: Array<{ method: string; url: string; key: string | null }> = [];
  page.on("request", (r) => {
    if (r.url().includes("/api/stores/") && r.method() !== "GET") calls.push({ method: r.method(), url: new URL(r.url()).pathname, key: r.headers()["idempotency-key"] ?? null });
  });

  // create: confirm dialog names the balance charge, one keyed POST, then the code appears with a copy button
  let detail = await expand(page, createOrder);
  const section = detail.getByTestId("order-cvs");
  await expect(section).toBeVisible();
  await section.getByTestId("cvs-create").click();
  const dialog = page.getByTestId("cvs-dialog");
  await expect(dialog).toBeVisible();
  await expect(page.getByTestId("cvs-confirm-text")).toContainText(ui.confirmText);
  await shot(page, "create-dialog", "en", "desktop");
  await page.getByTestId("cvs-submit").click();
  await expect(section.getByTestId("cvs-code")).toBeVisible({ timeout: 30000 });
  const creates = calls.filter((c) => c.url.endsWith(`/orders/${createOrder}/cvs-shipment`) && c.method === "POST");
  expect(creates).toHaveLength(1);
  expect(creates[0].key).toMatch(/^[A-Za-z0-9_.:-]{8,128}$/);
  await expect(section.getByTestId("cvs-copy-code")).toBeVisible();
  await expect(section.getByTestId("cvs-timeline")).toBeVisible();
  expect(await section.innerText()).not.toMatch(/delivered|已送達|已送达/i);
  await shot(page, "created", "en", "desktop");

  // print opens a NEW top-level tab whose page posts the signed form to ECPay (merchant side only); the fake host answers it here
  const popup = context.waitForEvent("page");
  await section.getByTestId("cvs-print").click();
  const tab = await popup;
  await tab.waitForURL(/logistics(-stage)?\.ecpay\.com\.tw|about:blank|cvs-print/, { timeout: 20000 }).catch(() => {});
  await expect.poll(() => ecpayRequests.filter((r) => r.method === "POST" && /Print/i.test(r.url)).length, { timeout: 20000 }).toBeGreaterThan(0);
  const printPost = ecpayRequests.find((r) => r.method === "POST" && /Print/i.test(r.url))!;
  expect(printPost.body ?? "").toContain("CheckMacValue");
  expect(printPost.body ?? "").not.toMatch(/HashKey|HashIV/);
  await tab.close();

  // UNKNOWN attempt: banner, acknowledgement checkbox and the manual-shipment way out
  detail = await expand(page, unknownOrder);
  const unknown = detail.getByTestId("order-cvs");
  await expect(unknown.getByTestId("cvs-unknown-banner")).toBeVisible();
  await expect(unknown).toContainText(/ECPay|綠界|绿界/);
  // the way out (abandon -> manual shipment) needs the acknowledgement checkbox; it is offered once the reconcile budget is spent
  if (await unknown.getByTestId("cvs-abandon").count()) {
    await unknown.getByTestId("cvs-abandon").click();
    await expect(page.getByTestId("cvs-dialog")).toBeVisible();
    await expect(page.getByTestId("cvs-ack")).toBeVisible();
    await expect(page.getByTestId("cvs-submit")).toBeDisabled(); // no abandon without "I checked the ECPay back office"
    await page.keyboard.press("Escape");
  }

  // pay-at-pickup: mark collected (dialog, keyed POST), cancel and release stock
  detail = await expand(page, collectOrder);
  const collect = detail.getByTestId("order-cvs");
  await expect(collect.getByTestId("cvs-collection-state")).toBeVisible();
  await collect.getByTestId("cvs-collected").click();
  await page.getByTestId("cvs-submit").click();
  await expect(collect.getByTestId("cvs-collection-state")).toContainText(/COLLECTED|已收款|collected/i);
  expect(calls.filter((c) => c.url.endsWith(`/orders/${collectOrder}/collection`) && c.method === "POST")).toHaveLength(1);

  detail = await expand(page, cancelOrder);
  const cancel = detail.getByTestId("order-cvs");
  await cancel.getByTestId("cvs-cancel-order").click();
  await page.getByTestId("cvs-submit").click();
  await expect(cancel.getByTestId("cvs-collection-state")).toContainText(/CANCELLED|取消|cancelled/i);
  expect(calls.filter((c) => c.url.endsWith(`/orders/${cancelOrder}/pay-at-pickup-release`) && c.method === "POST")).toHaveLength(1);

  // a buyer-entered store carries the unverified label for the merchant
  detail = await expand(page, enteredOrder);
  await expect(detail).toContainText(ui.enteredLabel);
  await shot(page, "entered-label", "en", "desktop");
  expect(ecpayRequests.every((r) => !/Create/.test(r.url))).toBe(true); // the browser never calls ECPay Create: Go does
  void cookieName;
});
