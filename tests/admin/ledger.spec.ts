import { test, expect } from "@playwright/test";
import { mkdir } from "node:fs/promises";

test.describe.configure({ mode: "serial" });

test("approved ledger reproduction and mobile table remain usable", async ({
  page,
}) => {
  await page.goto("/zh-CN");
  await expect(page.getByRole("heading", { name: "商品与库存" })).toBeVisible();
  await expect(page.locator("tbody tr")).toHaveCount(9);
  await expect(
    page.getByText("隔离本地测试店铺 · 非客户真实库存"),
  ).toBeVisible();
  await page
    .getByRole("radio", { name: "选择 AC-002-BK", exact: true })
    .check();
  await mkdir("output/playwright/ledger-review", { recursive: true });
  await page.screenshot({
    path: "output/playwright/ledger-review/hero-repro.png",
    animations: "disabled",
  });
  await page.screenshot({
    path: "output/playwright/ledger-review/desktop.png",
    animations: "disabled",
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(page.getByRole("button", { name: "打开导航" })).toBeVisible();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  await page.screenshot({
    path: "output/playwright/ledger-review/mobile.png",
    fullPage: true,
    animations: "disabled",
  });
  await page.getByRole("button", { name: "打开导航" }).click();
  await page.getByRole("button", { name: "网站客服", exact: true }).click();
  await expect(page.getByRole("heading", { name: "网站客服" })).toBeVisible();
  await expect(
    page.getByText("网站客服、Meta 会话与平台支持使用独立的授权和数据域。"),
  ).toBeVisible();
});

test("locale routes preserve the scoped search and explicit choice", async ({
  page,
}) => {
  await page.goto("/zh-CN?q=HA-001-BE");
  await expect(page.locator("tbody tr")).toHaveCount(1);
  await page.getByRole("combobox", { name: "语言" }).selectOption("zh-TW");
  await expect(page).toHaveURL(/\/zh-TW\?q=HA-001-BE/);
  await expect(page.getByRole("heading", { name: "商品與庫存" })).toBeVisible();
  await expect(page.locator("html")).toHaveAttribute("lang", "zh-TW");
  await page.getByRole("combobox", { name: "語言" }).selectOption("en");
  await expect(
    page.getByRole("heading", { name: "Products & inventory" }),
  ).toBeVisible();
  await expect(page).toHaveURL(/\/en\?q=HA-001-BE/);
  await page.goto("/?q=AC-002-BK");
  await expect(page).toHaveURL(/\/en\?q=AC-002-BK/);
  await expect(page.locator("tbody tr")).toHaveCount(1);
  await page.goto("/en?q=%25");
  await expect(
    page.getByRole("heading", { name: "No matching SKUs" }),
  ).toBeVisible();
  await expect(page.locator('.message[role="alert"]')).toHaveCount(0);
});

test("lost mutation response retries original key and payload only once", async ({
  page,
}) => {
  await page.goto("/en?q=AC-002-BK");
  await page
    .getByRole("radio", { name: "Select AC-002-BK", exact: true })
    .check();
  const available = Number(
    await page.locator("tbody .available-value").innerText(),
  );
  const requests: { key: string | null; body: string | null }[] = [];
  await page.route("**/api/stores/*/inventory/adjustments", async (route) => {
    requests.push({
      key: route.request().headers()["idempotency-key"],
      body: route.request().postData(),
    });
    if (requests.length === 1) {
      const accepted = await route.fetch();
      expect(accepted.status()).toBe(200);
      await route.abort("connectionreset");
    } else await route.continue();
  });
  await page.getByRole("spinbutton", { name: "Adjustment quantity" }).fill("3");
  await page
    .getByRole("textbox", { name: "Reason (required)" })
    .fill("Browser acceptance: response loss");
  await page.getByRole("button", { name: "Confirm adjustment" }).click();
  await expect(
    page.getByRole("button", { name: "Retry", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("spinbutton", { name: "Adjustment quantity" }),
  ).toBeDisabled();
  await page.getByRole("button", { name: "Retry", exact: true }).click();
  await expect(
    page.getByText("Inventory updated", { exact: true }),
  ).toBeVisible();
  await expect(page.locator("tbody .available-value")).toHaveText(
    String(available + 3),
  );
  expect(requests).toHaveLength(2);
  expect(requests[1]).toEqual(requests[0]);
});

test("uncertain command survives closing a tab and storage denial sends no write", async ({
  page,
  context,
}) => {
  await page.goto("/en?q=CB-005-TC");
  await page
    .getByRole("radio", { name: "Select CB-005-TC", exact: true })
    .check();
  const available = Number(
    await page.locator("tbody .available-value").innerText(),
  );
  let originalKey = "";
  await page.route("**/api/stores/*/inventory/adjustments", async (route) => {
    originalKey = route.request().headers()["idempotency-key"];
    expect((await route.fetch()).status()).toBe(200);
    await route.abort("connectionreset");
  });
  await page.getByRole("spinbutton", { name: "Adjustment quantity" }).fill("4");
  await page
    .getByRole("textbox", { name: "Reason (required)" })
    .fill("Recover after closing tab");
  await page.getByRole("button", { name: "Confirm adjustment" }).click();
  await expect(
    page.getByRole("button", { name: "Retry", exact: true }),
  ).toBeVisible();
  await page.close();
  const recovered = await context.newPage();
  await recovered.goto("/en?q=CB-005-TC");
  await expect(
    recovered.getByRole("button", { name: "Retry", exact: true }),
  ).toBeVisible();
  const retried = recovered.waitForRequest("**/inventory/adjustments");
  await recovered.getByRole("button", { name: "Retry", exact: true }).click();
  expect((await retried).headers()["idempotency-key"]).toBe(originalKey);
  await expect(
    recovered.getByText("Inventory updated", { exact: true }),
  ).toBeVisible();
  await expect(recovered.locator("tbody .available-value")).toHaveText(
    String(available + 4),
  );
  await recovered
    .getByRole("radio", { name: "Select CB-005-TC", exact: true })
    .check();
  await recovered.evaluate(() => {
    Storage.prototype.setItem = () => {
      throw new DOMException("Blocked", "QuotaExceededError");
    };
  });
  let writes = 0;
  recovered.on("request", (request) => {
    if (request.method() === "POST") writes++;
  });
  await recovered
    .getByRole("spinbutton", { name: "Adjustment quantity" })
    .fill("2");
  await recovered
    .getByRole("textbox", { name: "Reason (required)" })
    .fill("Must not be sent without a journal");
  await recovered.getByRole("button", { name: "Confirm adjustment" }).click();
  await expect(recovered.locator('.message[role="alert"]')).toContainText(
    "command storage",
  );
  expect(writes).toBe(0);
});

test("stale balance fails closed and refresh enables a new command", async ({
  page,
  request,
}) => {
  await page.goto("/en?q=HA-001-BE");
  await page
    .getByRole("radio", { name: "Select HA-001-BE", exact: true })
    .check();
  const storeID = process.env.COMMERCE_FIXTURE_STORE_ID!;
  expect(storeID).toBeTruthy();
  const warehouseReply = await request.get(`/api/stores/${storeID}/warehouses`);
  const warehouse = (await warehouseReply.json()).items[0].id;
  const ledger = await request.get(
    `/api/stores/${storeID}/catalog-ledger?warehouse_id=${warehouse}&q=HA-001-BE`,
  );
  const row = (await ledger.json()).items[0];
  const changed = await request.post(
    `/api/stores/${storeID}/inventory/adjustments`,
    {
      headers: {
        Origin: "http://127.0.0.1:3100",
        "Idempotency-Key": crypto.randomUUID(),
      },
      data: {
        warehouse_id: warehouse,
        sku_id: row.sku_id,
        expected_version: row.balance_version,
        delta: 1,
        reason: "Concurrent independent acceptance command",
      },
    },
  );
  expect(changed.status()).toBe(200);
  await page.getByRole("spinbutton", { name: "Adjustment quantity" }).fill("2");
  await page
    .getByRole("textbox", { name: "Reason (required)" })
    .fill("Stale version must fail");
  await page.getByRole("button", { name: "Confirm adjustment" }).click();
  await expect(page.locator('.message[role="alert"]')).toContainText(
    "Inventory changed",
  );
  await page
    .getByRole("region", { name: "Products & inventory", exact: true })
    .getByRole("button", { name: "Refresh", exact: true })
    .click();
  await expect(page.locator("tbody .available-value")).toHaveText(
    String(row.available + 1),
  );
});

test("create product then first SKU and read persisted zero balance", async ({
  page,
}) => {
  const name = `Acceptance product ${Date.now()}`,
    code = `QA-${Date.now()}`;
  await page.goto("/en");
  await page.getByRole("button", { name: "Add product", exact: true }).click();
  await page
    .getByRole("textbox", { name: "Product name", exact: true })
    .fill(name);
  await page
    .getByRole("textbox", { name: "Description", exact: true })
    .fill("Isolated acceptance fixture");
  await page
    .locator(".create-panel")
    .getByRole("button", { name: "Add product", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: "Add first SKU" }),
  ).toBeVisible();
  await page.getByRole("textbox", { name: "SKU code" }).fill(code);
  await page
    .getByRole("spinbutton", { name: "Price in minor units" })
    .fill("12345");
  await page
    .locator(".create-panel")
    .getByRole("button", { name: "Add first SKU", exact: true })
    .click();
  await expect(page.locator(".create-panel")).toHaveCount(0);
  await page
    .getByRole("textbox", { name: "Search product name or SKU" })
    .fill(code);
  await page.getByRole("button", { name: "Search", exact: true }).click();
  await expect(page.locator("tbody tr")).toHaveCount(1);
  await expect(page.getByRole("button", { name, exact: true })).toBeVisible();
  await expect(page.locator("tbody .available-value")).toHaveText("0");
  await page.reload();
  await expect(page.getByRole("button", { name, exact: true })).toBeVisible();
});

test("BFF rejects cross-store, foreign origin, route injection and oversized writes", async ({
  request,
  page,
}) => {
  const storeID = process.env.COMMERCE_FIXTURE_STORE_ID!;
  expect(storeID).toBeTruthy();
  const cross = await request.get(
    "/api/stores/00000000-0000-0000-0000-000000000000/products",
  );
  expect(cross.status()).toBe(404);
  const foreign = await request.post(`/api/stores/${storeID}/products`, {
    headers: {
      Origin: "https://example.invalid",
      "Idempotency-Key": crypto.randomUUID(),
    },
    data: { name: "MUST NOT CREATE" },
  });
  expect(foreign.status()).toBe(403);
  const badRoute = await request.get(`/api/stores/${storeID}/secrets`);
  expect(badRoute.status()).toBe(404);
  const bad = await badRoute.json();
  expect(bad.request_id).toMatch(/^[0-9a-f]{32}$/);
  expect(badRoute.headers()["x-request-id"]).toBe(bad.request_id);
  const wrongMethod = await request.delete(`/api/stores/${storeID}/products`);
  expect(wrongMethod.status()).toBe(405);
  expect((await wrongMethod.json()).code).toBe("method_not_allowed");
  const huge = await request.post(`/api/stores/${storeID}/products`, {
    headers: {
      Origin: "http://127.0.0.1:3100",
      "Idempotency-Key": crypto.randomUUID(),
    },
    data: { name: "x".repeat(65537) },
  });
  expect(huge.status()).toBe(400);
  await page.goto("/en");
  const html = await page.content();
  expect(html).not.toContain(process.env.COMMERCE_FIXTURE_TOKEN!);
});

test("purchase-entry proxy only accepts the scoped GET locale query", async ({
  request,
}) => {
  const store = process.env.COMMERCE_FIXTURE_STORE_ID!;
  const warehouses = await request.get(`/api/stores/${store}/warehouses`);
  const warehouseID = (await warehouses.json()).items[0].id;
  const ledger = await request.get(
    `/api/stores/${store}/catalog-ledger?warehouse_id=${warehouseID}`,
  );
  const productID = (await ledger.json()).items[0].product_id;
  const path = `/api/stores/${store}/products/${productID}/purchase-entry`;
  const valid = await request.get(`${path}?locale=en`);
  expect(valid.status()).toBe(200);
  const projection = await valid.json();
  expect(Object.keys(projection).sort()).toEqual([
    "locale",
    "product_id",
    "state",
    "url",
  ]);
  expect(projection.product_id).toBe(productID);
  // Valid URL encoding is equivalent under the frozen locale contract.
  // Next normalizes the incoming query before this BFF receives Request.url;
  // invalid encodings remain a denial case below, not a locale alias.
  const encoded = await request.get(`${path}?locale=%65n`);
  expect(encoded.status()).toBe(200);
  expect(await encoded.json()).toEqual(projection);
  for (const suffix of [
    "",
    "?",
    "?locale=",
    "?locale=fr",
    "?locale=en&locale=en",
    "?locale=en&extra=1",
    "?locale=%zz",
  ]) {
    const denied = await request.get(path + suffix);
    expect(denied.status(), suffix).toBe(422);
  }
  const key = await request.get(`${path}?locale=en`, {
    headers: { "Idempotency-Key": "unexpected-key" },
  });
  expect(key.status()).toBe(422);
  const post = await request.post(`${path}?locale=en`, { data: {} });
  expect(post.status()).toBe(404);
});

test("purchase-entry read failure keeps product and SKU write receipts", async ({
  page,
}) => {
  const name = `Entry read failure ${Date.now()}`;
  const code = `PE-${Date.now()}`;
  let productWrites = 0;
  let skuWrites = 0;
  let reads = 0;
  page.on("request", (request) => {
    const path = new URL(request.url()).pathname;
    if (request.method() === "POST" && path.endsWith("/products"))
      productWrites++;
    if (request.method() === "POST" && path.endsWith("/skus")) skuWrites++;
  });
  await page.route(
    /\/api\/stores\/[^/]+\/products\/[^/]+\/purchase-entry\?locale=en$/,
    async (route) => {
      reads++;
      await route.fulfill({
        status: 503,
        contentType: "application/json",
        body: JSON.stringify({
          code: "unavailable",
          message: "",
          request_id: "",
          retryable: true,
          details: {},
        }),
      });
    },
  );
  await page.goto("/en");
  await page.getByRole("button", { name: "Add product", exact: true }).click();
  await page
    .getByRole("textbox", { name: "Product name", exact: true })
    .fill(name);
  await page
    .locator(".create-panel")
    .getByRole("button", { name: "Add product" })
    .click();
  await expect(
    page.getByRole("heading", { name: "Add first SKU" }),
  ).toBeVisible();
  await expect(
    page.getByTestId("purchase-entry").getByRole("alert"),
  ).toContainText("saved product is unchanged");
  await expect(page.locator(".message.success")).toContainText(
    "Product saved. Add its first SKU below.",
  );
  await page.getByRole("textbox", { name: "SKU code" }).fill(code);
  await page
    .getByRole("spinbutton", { name: "Price in minor units" })
    .fill("12345");
  await page
    .locator(".create-panel")
    .getByRole("button", { name: "Add first SKU" })
    .click();
  await expect(
    page.getByText("Product and SKU saved", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByTestId("purchase-entry").getByRole("alert"),
  ).toContainText("saved product is unchanged");
  expect(productWrites).toBe(1);
  expect(skuWrites).toBe(1);
  expect(reads).toBeGreaterThanOrEqual(2);
});

test("catalog write denial reports permission without claiming a product was saved", async ({
  page,
}) => {
  let writes = 0;
  await page.route(/\/api\/stores\/[^/]+\/products$/, async (route) => {
    if (route.request().method() !== "POST") return route.continue();
    writes++;
    await route.fulfill({
      status: 403,
      contentType: "application/json",
      body: JSON.stringify({
        code: "forbidden",
        message: "",
        request_id: "",
        retryable: false,
        details: {},
      }),
    });
  });
  await page.goto("/en");
  await page.getByRole("button", { name: "Add product", exact: true }).click();
  await page
    .getByRole("textbox", { name: "Product name", exact: true })
    .fill("Permission probe");
  await page
    .locator(".create-panel")
    .getByRole("button", { name: "Add product" })
    .click();
  await expect(page.locator('.message[role="alert"]')).toContainText(
    "cannot perform this action",
  );
  await expect(
    page.getByText("Product saved. Add its first SKU below.", { exact: true }),
  ).toHaveCount(0);
  expect(writes).toBe(1);
});

test("purchase controls recheck current state before copying or opening", async ({
  page,
}) => {
  let available = true;
  let reads = 0;
  await page.route(
    /\/api\/stores\/[^/]+\/products\/[^/]+\/purchase-entry\?locale=en$/,
    async (route) => {
      reads++;
      const requestURL = new URL(route.request().url());
      const productID = requestURL.pathname.split("/").at(-2)!;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          product_id: productID,
          locale: "en",
          state: available ? "configured" : "storefront_unavailable",
          url: available ? `https://shop.example/en/products/${productID}` : "",
        }),
      });
    },
  );
  await page.goto("/en");
  const panel = page.getByTestId("purchase-entry");
  await expect(
    panel.getByRole("button", { name: "Copy address" }),
  ).toBeVisible();
  available = false;
  await panel.getByRole("button", { name: "Copy address" }).click();
  await expect(panel).toContainText(
    "No verified, published storefront address is available.",
  );
  await expect(panel.getByRole("button", { name: "Copy address" })).toHaveCount(
    0,
  );
  expect(reads).toBeGreaterThanOrEqual(2);
  available = true;
  await panel.getByRole("button", { name: "Refresh" }).click();
  await expect(
    panel.getByRole("button", { name: "Open purchase page" }),
  ).toBeVisible();
  await page.route("https://shop.example/**", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "text/html",
      body: "<title>Buyer page</title>",
    });
  });
  await panel.getByRole("button", { name: "Open purchase page" }).click();
  await expect(page).toHaveURL(/https:\/\/shop\.example\/en\/products\//);
  expect(reads).toBeGreaterThanOrEqual(4);
});
