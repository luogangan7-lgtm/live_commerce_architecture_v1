// T12 (docs/delivery/units/t12-e2e.md): the core deal loop in one real-browser chain. Started only by
// tests/foundation/browser_e2e_test.go (TestBrowserE2EDealLoop), which owns every process (admin + storefront production
// Next builds, Go APIs, PostgreSQL, payment worker, Meta consumer/poller/dispatcher, fake Stripe, fake Graph, signed MOCK
// IdP, the disposable buyer.example TLS edge + CONNECT proxy) and the runner-only control listener.
//
// Two browser contexts: the merchant (admin origin on loopback) and the buyer (https://buyer.example through the proxy).
// The browser never touches Go directly; things the outside world does (a Meta comment arrives, Stripe pays, the provider
// settles a refund) and every PostgreSQL assertion go through POST {control}/act?name=... which Go executes on its test
// goroutine (a failed Go assertion fails the run and the answer never comes).
//
// BFF routes exercised: /api/onboarding/initial-store, /api/stores/{store}/live-sessions, .../claim-source, .../claim-window
// and offers, .../orders, .../orders/{id}/shipment, .../orders/{id}/refunds (admin); /api/buyer/session, cart, quotes,
// destinations, orders, orders/{id}/payment/* (storefront). Go endpoints sit behind them.
import { expect, test, type Locator, type Page } from "@playwright/test";
import { createHash } from "node:crypto";
import { readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { claimCopy } from "../../apps/storefront/lib/claim-copy";
import { carrierNames, orderCopy } from "../../apps/storefront/lib/order-copy";
import { paymentCopy } from "../../apps/storefront/lib/payment-copy";
import { purchaseCopy } from "../../apps/storefront/lib/purchase-copy";

const required = (name: string) => {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required`);
  return value;
};
const admin = required("LC_E2E_ADMIN_ORIGIN");
const buyerOrigin = required("LC_E2E_BUYER_ORIGIN");
const evidence = required("LC_E2E_EVIDENCE");
const control = required("LC_E2E_CONTROL");
const controlKey = required("LC_E2E_CONTROL_KEY");

test.use({ launchOptions: { proxy: { server: required("LC_E2E_PROXY"), bypass: "127.0.0.1" } } });
test.setTimeout(1_500_000);

const L = "zh-TW" as const;
const pii = { recipient_name: "Synthetic Gate Recipient", phone: "+886900000091", region: "Synthetic Region", city: "Synthetic City", postal_code: "99991",
  line1: "Synthetic Address Ninety One", line2: "Synthetic Unit Ninety Two" };
const refundMinor = 1000;
const money = (minor: number) => (minor / 100).toFixed(2);

async function act(name: string, body: Record<string, unknown> = {}) {
  const response = await fetch(`${control}/act?name=${name}`, { method: "POST", headers: { "X-Gate-Key": controlKey, "content-type": "application/json" }, body: JSON.stringify(body) });
  const answer = (await response.json().catch(() => ({}))) as Record<string, any>;
  expect(response.status, `control ${name}: ${JSON.stringify(answer)}`).toBe(200);
  return answer;
}

const manifestPath = path.join(evidence, "screenshots.json");
async function shot(page: Page, name: string, locale: string, viewport: "desktop" | "mobile", fullPage = true) {
  const file = path.join(evidence, `${name}-${locale}-${viewport}.png`);
  await page.screenshot({ path: file, fullPage, animations: "disabled" });
  expect(await page.evaluate(() => document.documentElement.scrollWidth - innerWidth), `horizontal overflow: ${name} ${viewport}`).toBeLessThanOrEqual(1);
  let manifest: unknown[] = [];
  try {
    manifest = JSON.parse(await readFile(manifestPath, "utf8"));
  } catch {
    /* first screenshot */
  }
  manifest.push({ File: path.basename(file), Sha256: createHash("sha256").update(await readFile(file)).digest("hex"), Locale: locale, Viewport: viewport });
  await writeFile(manifestPath, JSON.stringify(manifest, null, 2));
}
// Buyer pages are captured at desktop and at 390 px (the brief's rule), merchant pages at desktop.
async function buyerShots(page: Page, name: string) {
  await page.setViewportSize({ width: 1440, height: 900 });
  await shot(page, name, L, "desktop");
  await page.setViewportSize({ width: 390, height: 844 });
  await shot(page, name, L, "mobile");
  await page.setViewportSize({ width: 1440, height: 900 });
}

// The orders list re-renders after its first read, which collapses an already expanded row: expand until the detail holds.
async function expandOrder(page: Page, order: string): Promise<Locator> {
  const button = page.getByTestId(`order-expand-${order}`);
  await expect(button).toBeVisible();
  await expect(async () => {
    if ((await button.getAttribute("aria-expanded")) !== "true") await button.click();
    await expect(page.getByTestId("order-detail")).toHaveAttribute("aria-label", new RegExp(order), { timeout: 3_000 });
  }).toPass({ timeout: 30_000, intervals: [500, 1_000, 2_000] });
  return page.getByTestId("order-detail");
}

async function storageHas(page: Page, secrets: string[]) {
  const stored = await page.evaluate(() => JSON.stringify({ local: { ...localStorage }, session: { ...sessionStorage } }));
  return secrets.filter((secret) => stored.includes(secret)).length; // a count: never echo a secret into a failure message
}
async function storageLacks(page: Page, secrets: string[]) {
  expect(await storageHas(page, secrets)).toBe(0);
}
// The scan must be able to fail (PROCESS §2.4): a planted value is found, then removed.
async function storageScanSelfTest(page: Page, secret: string) {
  await page.evaluate((value) => localStorage.setItem("__t12_selftest", value), secret);
  expect(await storageHas(page, [secret])).toBe(1);
  await page.evaluate(() => localStorage.removeItem("__t12_selftest"));
  expect(await storageHas(page, [secret])).toBe(0);
}

test("T12 deal loop: wizard store, Studio, claim source, signed Meta comment, private reply, claim link, cart, checkout, Stripe (MOCK), ship, refund", async ({ browser }) => {
  const cases: string[] = [];
  const pass = (name: string) => {
    cases.push(name);
    console.log(`PASS ${name}`);
  };
  const pageErrors: string[] = [];
  // Product defects found by this chain. Each is recorded with evidence and the chain CONTINUES, so everything downstream of
  // it is still exercised; the test fails at the very end if any exist (a known defect never turns the gate green).
  const defects: { id: string; what: string }[] = [];

  // ------------------------------------------------------------------------------------------------ 1. merchant + store
  const merchantContext = await browser.newContext({ viewport: { width: 1586, height: 992 } });
  const merchant = await merchantContext.newPage();
  merchant.on("pageerror", (error) => pageErrors.push(error.message));
  await merchant.goto(`${admin}/en/`);
  await merchant.getByRole("button", { name: "Sign in with identity service", exact: true }).click();
  await merchant.getByLabel("Merchant name", { exact: true }).fill("T12 Merchant");
  await merchant.getByRole("button", { name: "Next: store settings", exact: true }).click();
  await merchant.getByLabel("Store name", { exact: true }).fill("T12 Store");
  await merchant.getByLabel("Transaction currency").selectOption("TWD");
  await merchant.getByRole("button", { name: "Next: warehouse", exact: true }).click();
  await merchant.getByLabel("Initial warehouse name", { exact: true }).fill("T12 Warehouse");
  const created = merchant.waitForResponse((r) => new URL(r.url()).pathname === "/api/onboarding/initial-store");
  await merchant.getByRole("button", { name: "Create internal workspace", exact: true }).click();
  const receipt = (await (await created).json()) as { store_id: string };
  expect((await created).status()).toBe(200);
  await shot(merchant, "merchant-store-created", "en", "desktop", false);
  const fixtures = await act("provision");
  expect(fixtures.store).toBe(receipt.store_id);
  const store = receipt.store_id as string;
  pass("merchant signs in (signed MOCK IdP), the wizard creates the store through create_initial_store; Go confirms the 0065 grants and provisions catalog, stock, pricing, delivery, Stripe, Meta and the buyer origin for THAT store");

  // ------------------------------------------------------------------------------------------------ 2. Studio: scene, window, offer, source
  await merchant.goto(`${admin}/en/studio?store=${store}`);
  await expect(merchant.getByTestId("merchant-studio")).toBeVisible();
  await merchant.getByRole("button", { name: /New scene/ }).click();
  await merchant.getByLabel("Scene name").fill("T12 deal loop scene");
  await merchant.getByRole("button", { name: "Create draft" }).click();
  await merchant.waitForURL(/scene=[0-9a-f-]{36}/);
  const scene = new URL(merchant.url()).searchParams.get("scene") as string;
  // R1 deploy shape (ruling G2): planning-only Studio, no rehearsal column or controls.
  await expect(merchant.locator(".studio-surface.studio-planning-only")).toBeVisible();
  await expect(merchant.getByRole("button", { name: /MOCK rehearsal|Request stop/ })).toHaveCount(0);
  await merchant.getByTestId("studio-open-claims").click();
  await expect(merchant.getByTestId("merchant-claims")).toBeVisible();
  await merchant.getByRole("button", { name: "Open claim window" }).click();
  await expect(merchant.getByTestId("claims-window-state")).toHaveText("Open");
  const offerForm = merchant.locator(".claims-offer-form");
  await offerForm.getByLabel("Keyword", { exact: true }).fill("A1");
  await offerForm.getByLabel("Product", { exact: true }).selectOption({ label: fixtures.product_name });
  await expect(offerForm.getByLabel("SKU", { exact: true })).toBeEnabled();
  await offerForm.getByLabel("SKU", { exact: true }).selectOption({ label: fixtures.sku_code });
  await offerForm.getByLabel("Max per claim", { exact: true }).fill("5");
  await offerForm.getByRole("button", { name: "Add offer" }).click();
  await expect(merchant.getByTestId("offer-A1")).toContainText(fixtures.sku_code);
  const source = merchant.getByTestId("claims-source");
  await source.getByLabel("Post or media link or ID", { exact: true }).fill(fixtures.post_url);
  await source.getByLabel("Reply language", { exact: true }).selectOption("zh-TW");
  await source.getByLabel("Send a private reply with the cart link").check();
  await source.getByRole("button", { name: "Save comment source" }).click();
  await expect(source.getByTestId("claims-source-object")).toHaveText(`${fixtures.page_asset}_${new URL(fixtures.post_url).pathname.split("/").pop()}`);
  await expect(source.getByRole("alert")).toHaveCount(0);
  await act("check", { name: "source" });
  await shot(merchant, "merchant-claims-source-bound", "en", "desktop", false);
  pass("Studio: a scene is created, the claim window opened, offer A1 bound to the SKU, the scene bound to the Facebook post with private replies (claim-source UI); Go readback matches");

  // ------------------------------------------------------------------------------------------------ 3. signed Meta comment
  await act("comment", { text: "A1+2", kind: "first" });
  const reply = await act("await-reply");
  expect(reply.sends).toBe(1);
  await act("check", { name: "claimed" });
  const bundle = merchant.locator('[data-testid^="bundle-"]').first();
  // MDEF-1 probe: the merchant must see the Meta-sourced claim (contracts/meta-claims-intake-v1.md §8: bundles.platform widens to
  // facebook/instagram, label NULL). Go answers 200 with platform "facebook"; the admin client validator (apps/admin/lib/
  // claims-model.ts, `platform: "manual"`) rejects it and the page shows "Claims are temporarily unavailable".
  async function merchantSeesClaim(name: string) {
    await merchant.getByRole("button", { name: "Refresh facts" }).click();
    try {
      await expect(bundle).toContainText("A1 × 2", { timeout: 8_000 });
      return true;
    } catch {
      await shot(merchant, name, "en", "desktop", false);
      if (!defects.some((d) => d.id === "MDEF-1"))
        defects.push({ id: "MDEF-1", what: `the merchant Claims page cannot show a Meta-sourced bundle: it renders "${(await merchant.getByTestId("merchant-claims").getByRole("alert").first().textContent().catch(() => merchant.locator("main").innerText()))?.toString().trim()}" although GET .../claims/bundles answered 200 (platform "facebook", empty label); apps/admin/lib/claims-model.ts validates platform === "manual"` });
      return false;
    }
  }
  if (await merchantSeesClaim("merchant-claims-after-comment")) {
    await expect(bundle).toContainText("Not opened yet");
    await shot(merchant, "merchant-claims-after-comment", "en", "desktop", false);
  }
  pass("a SIGNED Meta comment 'A1+2' entered the real ingress over HTTP; consumer + intake created one claim bundle (A1 x 2) and exactly one private reply reached the fake Graph with the claim link (Go-verified); merchant visibility is checked separately (MDEF-1)");

  // ------------------------------------------------------------------------------------------------ 4. negative loop
  const replay = await act("replay");
  expect(replay.replay_status).toBe(200);
  await act("negatives");
  await merchantSeesClaim("merchant-claims-after-negatives");
  pass("negative loop: an identical webhook replay creates nothing and sends no second reply; '不要A1' and 'A1是不是红色' create no claim and no send");

  // ------------------------------------------------------------------------------------------------ 5. buyer: claim link, cart
  const buyerContext = await browser.newContext({ ignoreHTTPSErrors: true, viewport: { width: 1440, height: 900 }, locale: "zh-TW" });
  let hostedVisits = 0;
  await buyerContext.route(/^https:\/\/checkout\.stripe\.com\//, (route) => {
    hostedVisits++;
    return route.fulfill({ status: 200, contentType: "text/html; charset=utf-8", body: "<!doctype html><title>Synthetic Stripe hosted page</title><main>Synthetic hosted page (MOCK)</main>" });
  });
  const buyer = await buyerContext.newPage();
  buyer.on("pageerror", (error) => pageErrors.push(error.message));
  const link = reply.link as string;
  expect(link).toMatch(/^https:\/\/buyer\.example\/zh-TW\/claim#t=[A-Za-z0-9_-]{43}$/);
  const token = link.split("#t=")[1];
  const claim = claimCopy[L];
  const buyerRequests: string[] = [];
  buyer.on("request", (request) => buyerRequests.push(request.url()));
  const opened = await buyer.goto(link);
  expect(opened?.status()).toBe(200);
  await expect(buyer.getByRole("heading", { level: 1, name: claim.title })).toBeVisible();
  await expect(buyer.getByTestId("claim-line-A1")).toContainText(`${claim.quantity} 2`);
  await expect(buyer.getByTestId("claim-line-A1")).toContainText(fixtures.sku_code);
  expect(new URL(buyer.url()).hash).toBe("");
  await storageScanSelfTest(buyer, token);
  await storageLacks(buyer, [token]);
  await buyerShots(buyer, "buyer-claim");
  await buyer.getByRole("button", { name: claim.add }).click();
  await expect(buyer.getByTestId("claim-added")).toHaveText(claim.added);
  await expect(buyer.getByTestId(`claim-cart-${fixtures.sku_id}`)).toContainText("× 2");
  await act("check", { name: "cart" });
  await buyerShots(buyer, "buyer-claim-added");
  pass("the buyer opens the private-reply link (zh-TW): prefilled claim A1 x 2, fragment dropped, token not stored; add-to-cart puts A1 x 2 in the server cart");

  // ------------------------------------------------------------------------------------------------ 6. checkout
  const purchase = purchaseCopy[L];
  await buyer.goto(`${buyerOrigin}/${L}/products/${fixtures.product_id}`);
  await expect(buyer.locator("#quantity")).toBeVisible();
  await expect(buyer.locator("#quantity")).toHaveValue("2");
  await buyer.getByRole("button", { name: purchase.delivery, exact: true }).click();
  const quotation = buyer.waitForResponse((r) => new URL(r.url()).pathname === "/api/buyer/quotes" && r.request().method() === "POST");
  await buyer.getByRole("button", { name: purchase.quote, exact: true }).click();
  expect((await quotation).status()).toBe(200);
  await expect(buyer.getByTestId("address-section")).toBeVisible();
  for (const [key, value] of Object.entries(pii)) await buyer.locator(`input[name="${key}"]`).fill(value);
  await buyer.getByTestId("confirm-address").click();
  await expect(buyer.getByTestId("create-order")).toBeEnabled();
  await buyer.getByTestId("create-order").click();
  await expect(buyer.getByTestId("order-section")).toBeVisible();
  const order = ((await buyer.getByTestId("order-id").innerText()) ?? "").trim();
  expect(order).toMatch(/^[0-9a-f-]{36}$/);
  await expect(buyer.getByTestId("payment-status")).toHaveAttribute("data-state", "NOT_STARTED");
  await act("check", { name: "ordered", order });
  await buyerShots(buyer, "buyer-order-created");
  pass("the buyer checks out the claimed cart (home delivery; the storefront states that convenience-store pickup is not connected yet): one unpaid order, total NT$25.00, 2 units held");

  // ------------------------------------------------------------------------------------------------ 7. pay (Stripe MOCK)
  const pay = paymentCopy[L];
  const popup = buyer.waitForEvent("popup");
  await buyer.getByRole("button", { name: pay.pay, exact: true }).click();
  const hosted = await popup;
  await hosted.waitForURL((u) => u.origin === "https://checkout.stripe.com", { waitUntil: "commit" });
  expect(await hosted.evaluate(() => window.opener)).toBeNull();
  expect(hostedVisits).toBeGreaterThan(0);
  expect(buyer.url()).not.toContain("stripe");
  await act("pay", { order });
  await hosted.close();
  await buyer.bringToFront();
  await expect(async () => {
    await buyer.getByTestId("refresh-order").click();
    await expect(buyer.getByTestId("payment-status")).toHaveAttribute("data-state", "CAPTURED", { timeout: 4_000 });
  }).toPass({ timeout: 90_000, intervals: [1_000, 3_000, 5_000] });
  await expect(buyer.getByTestId("payment-status")).toHaveText(`${pay.paymentState}: ${pay.CAPTURED}`);
  await expect(buyer.getByTestId("payment-commercial-status")).toHaveText(`${pay.orderState}: ${orderCopy[L].CONFIRMED}`);
  await act("check", { name: "paid" });
  await buyerShots(buyer, "buyer-paid");
  pass("Stripe (MOCK): the buyer pays in a new tab, the signed webhook captures it; the order is CONFIRMED and 2 units are allocated");

  // ------------------------------------------------------------------------------------------------ 8. merchant ships
  await merchant.goto(`${admin}/en/orders?store=${store}`);
  await expect(merchant.getByTestId("merchant-orders")).toBeVisible();
  await expect(merchant.getByTestId("orders-table")).toBeVisible();
  const detail = await expandOrder(merchant, order);
  await expect(detail.locator('[data-state="CONFIRMED"]').first()).toBeVisible();
  await expect(detail).toContainText(money(2500));
  await shot(merchant, "merchant-order-confirmed", "en", "desktop", false);
  await detail.getByLabel(/carrier|物流|承運/i).first().selectOption("seven_eleven_cvs");
  await detail.getByLabel(/tracking number|物流單號|追蹤單號/i).fill(fixtures.tracking);
  await detail.getByLabel(/tracking (url|link)|追蹤連結/i).fill(`https://track.example.com/t?n=${fixtures.tracking}`);
  await detail.getByRole("button", { name: /mark shipped|標記已出貨/i }).click();
  await expect(detail.locator('[data-state="MERCHANT_SHIPPED"]').first()).toBeVisible();
  await expect(detail).toContainText(fixtures.tracking);
  await expect(detail).not.toContainText(/delivered|in transit|已送達|運送中/i);
  await act("check", { name: "shipped" });
  await shot(merchant, "merchant-order-shipped", "en", "desktop", false);
  pass("the merchant marks the paid order shipped (7-ELEVEN + tracking) in the orders UI: shipment v1 SHIPPED, MERCHANT_SHIPPED, audited, no provider operation, stock unchanged");

  // ------------------------------------------------------------------------------------------------ 9. buyer sees the shipment
  async function openOrder(page: Page) {
    await page.goto(`${buyerOrigin}/${L}/products/${fixtures.product_id}`);
    await page.getByTestId("toggle-order-history").click();
    const detailResponse = page.waitForResponse((r) => new URL(r.url()).pathname === `/api/buyer/orders/${order}` && r.request().method() === "GET");
    await page.locator(`button[data-order-id="${order}"]`).click();
    expect((await detailResponse).status()).toBe(200);
    await expect(page.getByTestId("order-id")).toHaveText(order);
  }
  await openOrder(buyer);
  await expect(buyer.getByTestId("order-shipment")).toBeVisible();
  await expect(buyer.getByTestId("shipment-carrier")).toContainText(carrierNames[L].seven_eleven_cvs);
  await expect(buyer.getByTestId("shipment-tracking")).toHaveText(fixtures.tracking);
  expect(await buyer.locator("body").innerText()).not.toMatch(/delivered|in transit|已送達|運送中/i);
  await buyerShots(buyer, "buyer-shipped");
  pass("the buyer sees the carrier and tracking number of the merchant's shipment, never 'delivered'");

  // ------------------------------------------------------------------------------------------------ 10. refund
  await merchant.reload();
  await expect(merchant.getByTestId("orders-table")).toBeVisible();
  const refundDetail = await expandOrder(merchant, order);
  await refundDetail.getByRole("button", { name: /^(refund|退款|發起退款|发起退款|issue refund)/i }).click();
  const dialog = merchant.locator("dialog[open]");
  await expect(dialog).toBeVisible();
  const amount = dialog.getByLabel(/amount|金額|金额/i);
  await expect(amount).toHaveValue(/^25(\.00)?$/);
  await amount.fill(String(refundMinor / 100));
  await dialog.getByLabel(/reason|原因/i).selectOption("requested_by_customer");
  await dialog.getByRole("button", { name: /review|核對|核对/i }).click();
  await expect(dialog).toContainText(String(refundMinor / 100));
  await dialog.getByRole("button", { name: /confirm|確認|确认/i }).last().click();
  await expect(dialog).toHaveCount(0);
  await act("settle-refund");
  await expect(async () => {
    const refresh = refundDetail.getByRole("button", { name: /refresh|重新整理|刷新/i });
    if (await refresh.count()) await refresh.first().click();
    await expect(refundDetail.locator('[data-state="SUCCEEDED"]').first()).toBeVisible({ timeout: 3_000 });
  }).toPass({ timeout: 60_000, intervals: [1_000, 3_000, 5_000] });
  await expect(refundDetail).toContainText(/re_[A-Za-z0-9_]+/);
  await expect(refundDetail).toContainText(money(2500 - refundMinor));
  await act("check", { name: "refunded" });
  await shot(merchant, "merchant-refund-succeeded", "en", "desktop", false);
  await openOrder(buyer);
  await expect(buyer.getByTestId("refund-succeeded")).toContainText(pay.refunded.split("{amount}")[0].trim());
  await expect(buyer.getByTestId("payment-status")).toHaveAttribute("data-state", "PARTIALLY_REFUNDED");
  await buyerShots(buyer, "buyer-refunded");
  pass("the merchant issues a partial refund (NT$10.00); the real worker settles it at the fake Stripe; the merchant sees SUCCEEDED and the re_ id, the buyer sees the refunded amount, the order and the stock are unchanged");

  // ------------------------------------------------------------------------------------------------ 11. isolation, storage, console
  await act("isolation");
  for (const page of [buyer]) {
    await storageLacks(page, [token, pii.phone, pii.line1]);
    expect(buyerRequests.every((url) => !url.includes(token))).toBe(true);
  }
  await merchant.setViewportSize({ width: 390, height: 844 });
  await shot(merchant, "merchant-refund-succeeded", "en", "mobile", false);
  expect(pageErrors).toEqual([]);
  pass("another tenant reads nothing (real admin API); no claim token or buyer PII in browser storage or URLs; no page errors");

  await writeFile(path.join(evidence, "defects.json"), JSON.stringify(defects, null, 2), { mode: 0o600 });
  await writeFile(path.join(evidence, "result.json"), JSON.stringify({ cases, defects: defects.map((d) => d.id), locales: ["en", L],
    boundary: "production Next builds; signed MOCK IdP; MOCK Meta (signed synthetic webhook, fake Graph); MOCK Stripe (stripetest fake, Chromium-routed hosted page); synthetic buyer.example TLS edge; no provider, deployment or SANDBOX acceptance" }, null, 2), { mode: 0o600 });
  expect(defects, "product defects found by the chain (see defects.json)").toEqual([]);
});
