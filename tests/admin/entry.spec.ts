import { expect, test } from "@playwright/test";
import { createServer, type IncomingMessage } from "node:http";
import { mkdir, writeFile } from "node:fs/promises";

const apiOrigin = "http://127.0.0.1:19111";
const publicOrigin = "http://127.0.0.1:3100";
const issuer = "https://provider.example/realm/";
const bffKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA";
const binding = `${"B".repeat(42)}A`;
const session = `${"C".repeat(42)}A`;
const otherSession = `${"D".repeat(42)}A`;
const storeID = "11111111-1111-4111-8111-111111111111";
const warehouseID = "33333333-3333-4333-8333-333333333333";
const receipt = {
  tenant_id: "22222222-2222-4222-8222-222222222222",
  store_id: storeID,
  warehouse_id: warehouseID,
};

type OnboardCall = { key: string; body: string };
let storeCreated = false;
let onboarding: "success" | "unknown" | "unauthorized" = "success";
let onboardingCalls: OnboardCall[] = [];
let logoutCalls = 0;

const server = createServer(async (request, response) => {
  let body = "";
  for await (const chunk of request) body += chunk;
  // Serve the redirect destination for real on loopback: page.route only
  // handles the first request of a redirect chain, not the target URL.
  if (
    request.method === "GET" &&
    request.url === "/authorize?state=state-one"
  ) {
    response.setHeader("Content-Type", "text/html; charset=utf-8");
    response.end(
      "<!doctype html><title>Mock provider</title><body>provider mock</body>",
    );
    return;
  }
  response.setHeader("Content-Type", "application/json");
  response.setHeader("Cache-Control", "no-store");
  const identity = request.url?.startsWith("/v1/identity/");
  if (identity) {
    if (
      request.headers["x-commerce-bff-key"] !== bffKey ||
      request.headers.origin ||
      request.headers.cookie
    ) {
      response.writeHead(403).end(JSON.stringify({ code: "forbidden" }));
      return;
    }
    if (request.url === "/v1/identity/login/start") {
      response.end(
        JSON.stringify({
          authorization_url: `${apiOrigin}/authorize?state=state-one`,
          binding,
          expires_at: new Date(Date.now() + 300_000).toISOString(),
        }),
      );
      return;
    }
    if (request.url === "/v1/identity/login/complete") {
      const input = JSON.parse(body);
      if (
        input.binding !== binding ||
        input.state !== "state-one" ||
        input.code !== "code-one"
      ) {
        response.writeHead(401).end(JSON.stringify({ code: "unauthorized" }));
        return;
      }
      response.end(
        JSON.stringify({
          token: session,
          expires_at: new Date(Date.now() + 3_600_000).toISOString(),
        }),
      );
      return;
    }
    if (request.url === "/v1/identity/logout") {
      logoutCalls++;
      response.writeHead(401).end(JSON.stringify({ code: "unauthorized" }));
      return;
    }
    if (request.url === "/v1/identity/initial-store") {
      onboardingCalls.push({
        key: String(request.headers["idempotency-key"] ?? ""),
        body,
      });
      if (onboarding === "unauthorized") {
        response.writeHead(401).end(JSON.stringify({ code: "unauthorized" }));
        return;
      }
      if (onboarding === "unknown" && onboardingCalls.length === 1) {
        response.writeHead(503).end(JSON.stringify({ code: "retry_later" }));
        return;
      }
      storeCreated = true;
      response.end(JSON.stringify(receipt));
      return;
    }
  }
  if (
    ![`Bearer ${session}`, `Bearer ${otherSession}`].includes(
      String(request.headers.authorization),
    ) ||
    request.headers["x-commerce-bff-key"]
  ) {
    response.writeHead(401).end(JSON.stringify({ code: "unauthorized" }));
    return;
  }
  if (request.url === "/v1/admin/stores") {
    response.end(
      JSON.stringify({
        items: storeCreated
          ? [{ id: storeID, name: "Wizard store", currency: "TWD" }]
          : [],
      }),
    );
    return;
  }
  if (request.url === `/v1/admin/stores/${storeID}/warehouses`) {
    response.end(
      JSON.stringify({
        items: [{ id: warehouseID, name: "Main warehouse" }],
        next_cursor: "",
      }),
    );
    return;
  }
  if (request.url?.startsWith(`/v1/admin/stores/${storeID}/catalog-ledger?`)) {
    response.end(JSON.stringify({ items: [], next_cursor: "" }));
    return;
  }
  response.writeHead(404).end(JSON.stringify({ code: "not_found" }));
});

test.beforeAll(async () => {
  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(new URL(apiOrigin).port, "127.0.0.1", resolve);
  });
});

test.afterAll(async () => {
  await new Promise<void>((resolve, reject) =>
    server.close((error) => (error ? reject(error) : resolve())),
  );
});

test.beforeEach(() => {
  storeCreated = false;
  onboarding = "success";
  onboardingCalls = [];
  logoutCalls = 0;
});

async function login(page: import("@playwright/test").Page) {
  await page.goto("/en/?auth=failed");
  await expect(page.locator('.entry-message[role="alert"]')).toContainText(
    "Sign-in did not complete",
  );
  await Promise.all([
    // A request event precedes navigation commit. Wait for the fulfilled mock
    // page before starting callback navigation, or the two navigations race.
    page.waitForURL(`${apiOrigin}/authorize?state=state-one`, {
      waitUntil: "load",
    }),
    page.getByRole("button", { name: "Sign in with identity service" }).click(),
  ]);
  await expect(page.locator("body")).toHaveText("provider mock");
  await page.goto(
    `/api/auth/callback?state=state-one&code=code-one&iss=${encodeURIComponent(issuer)}`,
  );
  await expect(page).toHaveURL(/\/en\/?$/);
  await expect(
    page.getByRole("heading", { name: "Create your first workspace" }),
  ).toBeVisible();
}

async function reachLastStep(page: import("@playwright/test").Page) {
  await page.getByRole("button", { name: "Next: store settings" }).click();
  await expect(page.getByLabel("Merchant name")).toBeFocused();
  expect(
    await page
      .getByLabel("Merchant name")
      .evaluate((input) => (input as HTMLInputElement).checkValidity()),
  ).toBe(false);
  await page.getByLabel("Merchant name").fill("Browser Merchant");
  await page.getByRole("button", { name: "Next: store settings" }).click();
  await page.getByLabel("Store name").fill("Browser Store");
  await page.getByLabel("Transaction currency").selectOption("TWD");
  await page.getByLabel("Language", { exact: true }).selectOption("zh-TW");
  await expect(page).toHaveURL(/\/zh-TW\/?$/);
  await expect(page.getByLabel("商店名稱")).toHaveValue("Browser Store");
  await page.getByRole("button", { name: "上一步" }).click();
  await expect(page.getByLabel("商戶名稱")).toHaveValue("Browser Merchant");
  await page.getByRole("button", { name: "下一步：商店設定" }).click();
  await page.getByRole("button", { name: "下一步：庫存倉" }).click();
  await page.getByLabel("初始庫存倉名稱").fill("Browser Warehouse");
}

test("approved wizard step two matches desktop and mobile compositions", async ({
  page,
}) => {
  await login(page);
  await page.getByLabel("Merchant name").fill("南岛生活");
  await page.getByRole("button", { name: "Next: store settings" }).click();
  await page.getByLabel("Store name").fill("南岛选物");
  await page.getByLabel("Transaction currency").selectOption("TWD");
  await page.getByLabel("Language", { exact: true }).selectOption("zh-CN");
  await expect(page.getByLabel("店铺名称")).toHaveValue("南岛选物");
  await mkdir("output/playwright/ledger-review", { recursive: true });
  await page.setViewportSize({ width: 1585, height: 992 });
  await page.screenshot({
    path: "output/playwright/ledger-review/t03-entry-desktop.png",
    animations: "disabled",
  });
  await page.screenshot({
    path: "output/playwright/ledger-review/t03-hero-repro.png",
    animations: "disabled",
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.screenshot({
    path: "output/playwright/ledger-review/t03-entry-mobile.png",
    fullPage: true,
    animations: "disabled",
  });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
});

test("wizard preserves draft and recovers an unknown result with exact bytes", async ({
  page,
}) => {
  onboarding = "unknown";
  await login(page);
  await reachLastStep(page);
  await page.getByRole("button", { name: "建立內部工作區" }).dblclick();
  await expect(page.locator('.entry-message[role="alert"]')).toContainText(
    "伺服器結果未知",
  );
  expect(onboardingCalls).toHaveLength(1);
  await expect(page.getByLabel("初始庫存倉名稱")).toBeDisabled();
  await page.getByLabel("語言", { exact: true }).selectOption("en");
  await expect(page).toHaveURL(/\/en\/?$/);
  await expect(page.locator('.entry-message[role="alert"]')).toContainText(
    "server result is unknown",
  );
  await page.getByRole("button", { name: "Retry the same request" }).click();
  await expect(page.getByRole("status")).toContainText(
    "Internal workspace created",
  );
  expect(onboardingCalls).toHaveLength(2);
  expect(onboardingCalls[1]).toEqual(onboardingCalls[0]);
  await page.getByRole("button", { name: "Open workspace" }).click();
  await expect(
    page.getByRole("heading", { name: "Products & inventory" }),
  ).toBeVisible();
});

test("three-locale mobile long names and normal text meet the finish gate", async ({
  page,
}) => {
  await login(page);
  await page.getByLabel("Merchant name").fill("A".repeat(120));
  await page.getByRole("button", { name: "Next: store settings" }).click();
  await page.locator('select[name="currency"]').selectOption("TWD");
  await page.setViewportSize({ width: 390, height: 844 });
  for (const locale of ["en", "zh-TW", "zh-CN"]) {
    await page.locator(".entry-language select").selectOption(locale);
    await expect(page).toHaveURL(new RegExp(`/${locale}/?$`));
    await expect(page.locator('select[name="currency"]')).toHaveValue("TWD");
    const label = await page
      .locator('select[name="currency"] option:checked')
      .textContent();
    expect(label).toMatch(/^TWD · .+/);
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      ),
    ).toBe(true);
    expect(
      await page
        .locator(".entry-completed-row")
        .evaluate((node) => node.scrollWidth <= node.clientWidth),
    ).toBe(true);
    await page.screenshot({
      path: `output/playwright/ledger-review/t03-entry-long-name-${locale}.png`,
      fullPage: true,
      animations: "disabled",
    });
  }
  // Check actual computed foreground/background pairs, including inherited
  // transparent backgrounds, rather than merely asserting selected hex tokens.
  const ratios = await page.evaluate(() => {
    function luminance(color: string) {
      const rgb = color
        .match(/[\d.]+/g)!
        .slice(0, 3)
        .map(Number)
        .map((v) => {
          const channel = v / 255;
          return channel <= 0.04045
            ? channel / 12.92
            : ((channel + 0.055) / 1.055) ** 2.4;
        });
      return rgb[0] * 0.2126 + rgb[1] * 0.7152 + rgb[2] * 0.0722;
    }
    return [
      ".entry-form small",
      ".entry-steps li:last-child",
      ".entry-steps li.current .entry-step-number",
      ".entry-steps li:last-child .entry-step-number",
      ".entry-completed-row",
    ].map((selector) => {
      const node = document.querySelector(selector)!;
      let backgroundNode: Element | null = node;
      let background = "rgb(255, 255, 255)";
      while (backgroundNode) {
        const color = getComputedStyle(backgroundNode).backgroundColor;
        if (color !== "rgba(0, 0, 0, 0)" && color !== "transparent") {
          background = color;
          break;
        }
        backgroundNode = backgroundNode.parentElement;
      }
      const foreground = getComputedStyle(node).color;
      const a = luminance(foreground),
        b = luminance(background);
      return {
        selector,
        foreground,
        background,
        ratio: (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05),
      };
    });
  });
  for (const sample of ratios)
    expect(sample.ratio, sample.selector).toBeGreaterThanOrEqual(4.5);
  await writeFile(
    "output/playwright/ledger-review/t03-entry-contrast.json",
    JSON.stringify(ratios, null, 2),
  );
  expect(
    await page
      .locator('template[data-commerce-design-contract="57bb98dc"]')
      .evaluate((node) => {
        const contract = (node as HTMLTemplateElement).content.firstChild;
        // Next/React inserts an empty hidden Suspense boundary before authored
        // body children. Accept only that exact inert prefix, not visible UI.
        const prefix = node.previousElementSibling;
        const firstAuthored =
          !prefix ||
          (prefix === document.body.firstElementChild &&
            prefix.tagName === "DIV" &&
            prefix.hasAttribute("hidden") &&
            prefix.children.length === 0 &&
            prefix.textContent === "");
        return (
          node.parentElement === document.body &&
          firstAuthored &&
          contract?.nodeType === Node.COMMENT_NODE &&
          contract.textContent?.includes("FIRST VIEWPORT:")
        );
      }),
  ).toBe(true);
  expect(onboardingCalls).toHaveLength(0);
});

test("401 clears the old session-bound draft before reauthentication", async ({
  page,
}) => {
  onboarding = "unauthorized";
  await login(page);
  await reachLastStep(page);
  expect(
    await page.evaluate(() =>
      Object.keys(sessionStorage).some((key) =>
        key.startsWith("commerce-onboarding:"),
      ),
    ),
  ).toBe(true);
  await page.getByRole("button", { name: "建立內部工作區" }).click();
  await expect(page).toHaveURL(/\/zh-TW\/?\?auth=expired$/);
  await expect(page.locator('.entry-message[role="alert"]')).toContainText(
    "工作階段已過期",
  );
  expect(
    await page.evaluate(() =>
      Object.keys(sessionStorage).some((key) =>
        key.startsWith("commerce-onboarding:"),
      ),
    ),
  ).toBe(false);
  await expect(
    page.getByRole("button", { name: "透過身分服務登入" }),
  ).toBeVisible();
});

test("a stale tab never sends its draft after another account replaces the cookies", async ({
  context,
  page,
}) => {
  await login(page);
  await reachLastStep(page);
  const oldKeys = await page.evaluate(() => Object.keys(sessionStorage));
  expect(oldKeys.some((key) => key.startsWith("commerce-onboarding:"))).toBe(
    true,
  );
  await context.addCookies([
    {
      name: "__Host-commerce_session",
      value: otherSession,
      domain: "127.0.0.1",
      path: "/",
      secure: true,
      httpOnly: true,
      sameSite: "Lax",
    },
    {
      name: "__Host-commerce_csrf",
      value: `${"E".repeat(42)}A`,
      domain: "127.0.0.1",
      path: "/",
      secure: true,
      httpOnly: false,
      sameSite: "Lax",
    },
  ]);
  await page.getByRole("button", { name: "建立內部工作區" }).click();
  await expect(page.getByLabel("商戶名稱")).toHaveValue("");
  expect(onboardingCalls).toHaveLength(0);
  expect(logoutCalls).toBe(0);
  const journals = await page.evaluate(() =>
    Object.entries(sessionStorage)
      .filter(([key]) => key.startsWith("commerce-onboarding:"))
      .map(([, value]) => JSON.parse(value)),
  );
  expect(journals).toHaveLength(1);
  expect(journals[0].draft.tenant_name).toBe("");
  expect(JSON.stringify(journals)).not.toContain("Browser Merchant");
});
