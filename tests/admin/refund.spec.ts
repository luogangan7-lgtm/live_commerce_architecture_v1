// RF11 admin half (contracts/stripe-refund-v1.md §7.1 "Admin UI", §9). BFF routes exercised through the UI:
// GET|POST /api/stores/{store}/orders/{id}/refunds, POST .../refunds/{refund}/refresh, GET .../order-actions
// -> Go /v1/admin/stores/{store}/orders/{id}/refunds[/{refund}/refresh]. Driven in two phases by
// tests/foundation/browser_refund_fulfilment_test.go (LC_BROWSER_PHASE = partial | full) because the buyer half
// must observe the PENDING window between them. Label: BROWSER(MOCK) — the order was paid through the fake Stripe
// and refunds go through the fake; the SANDBOX variant (card 4242) is a separate NOT_RUN until SP18 + keys exist.
//
// The refund section is built by the refund-fulfilment-ui unit. Locators derive from the contract and the UI brief
// (native <dialog>, two-step confirmation restating amount + currency, one Idempotency-Key per dialog open, state
// badges "處理中/处理中/Processing"). UI wording lives in `ui`; until that unit merges this spec is NOT_RUN.
import { expect, test, type BrowserContext, type Locator, type Page } from "@playwright/test";
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
const order = required("LC_BROWSER_ORDER");
const restrictedToken = required("LC_BROWSER_RESTRICTED_TOKEN");
const captured = Number(required("LC_BROWSER_CAPTURED")); // minor units, TWD 2500 = "25.00"
const partial = Number(required("LC_BROWSER_PARTIAL")); // minor units, a multiple of 100
const phase = required("LC_BROWSER_PHASE");
const cookieName = "__Host-commerce_session";
const money = (minor: number) => (minor / 100).toFixed(2);

const ui = {
  refundSection: /refund|退款/i,
  refundAction: /^(refund|退款|發起退款|发起退款|issue refund)/i,
  amount: /amount|金額|金额/i,
  reason: /reason|原因/i,
  review: /review|核對|核对/i, // step one of the two-step dialog ("Review refund")
  confirm: /confirm|確認|确认/i,
  refresh: /refresh|重新整理|刷新/i,
  processing: /processing|處理中|处理中/i,
  needsSupport: /needs support|需要支援|需要支持/i,
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
async function expand(page: Page): Promise<Locator> {
  for (let pageNo = 0; pageNo < 3; pageNo++) {
    const button = page.getByTestId(`order-expand-${order}`);
    if (await button.count()) {
      if ((await button.getAttribute("aria-expanded")) !== "true") await button.click();
      const detail = page.getByTestId("order-detail");
      await expect(detail).toHaveAttribute("aria-label", new RegExp(order));
      return detail;
    }
    await expect(page.getByTestId("orders-next")).toBeEnabled();
    await page.getByTestId("orders-next").click();
    await expect(page.getByTestId("orders-table")).toBeVisible();
  }
  throw new Error("fixture order was absent from three real cursor pages");
}
async function asToken(context: BrowserContext, token: string) {
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

// state badges carry data-state (the C row's badge pattern); text is the fallback for the "processing" mapping
const inProgress = (detail: Locator) =>
  detail.locator('[data-state="PENDING"],[data-state="SUBMITTING"],[data-state="REQUESTED"]').first().or(detail.getByText(ui.processing).first());
const settled = (detail: Locator) => detail.locator('[data-state="SUCCEEDED"]').first();

type Post = { key: string | null; body: string | null };
function watchRefundPosts(page: Page): Post[] {
  const posts: Post[] = [];
  page.on("request", (r) => {
    if (r.method() === "POST" && new URL(r.url()).pathname.endsWith(`/orders/${order}/refunds`)) posts.push({ key: r.headers()["idempotency-key"] ?? null, body: r.postData() });
  });
  return posts;
}

// Two-step native <dialog>: amount + reason, then a confirmation that restates amount and currency.
async function refundThroughDialog(page: Page, detail: Locator, minor: number, refundableBefore: number) {
  await detail.getByRole("button", { name: ui.refundAction }).click();
  const dialog = page.locator("dialog[open]");
  await expect(dialog).toBeVisible();
  const amount = dialog.getByLabel(ui.amount);
  // default = the refundable amount, formatted by the existing currency formatter (TWD whole dollars)
  await expect(amount).toHaveValue(new RegExp(`^${money(refundableBefore).replace(".", "\\.")}$|^${refundableBefore / 100}$`));
  await amount.fill(String(minor / 100));
  await dialog.getByLabel(ui.reason).selectOption("requested_by_customer");
  await dialog.getByRole("button", { name: ui.review }).click();
  // step two restates amount + currency before anything is sent
  await expect(dialog).toContainText(String(minor / 100));
  await expect(dialog).toContainText(/TWD|NT\$|\$/);
  await dialog.getByRole("button", { name: ui.confirm }).last().click();
  await expect(dialog).toHaveCount(0);
}

test.describe(() => {
  test.skip(phase !== "partial", "phase partial only");
  test("RF11 partial refund: dialog, one keyed strict POST, no optimistic success, processing badge, Stripe refund id shown", async ({ page }) => {
    await signedLogin(page);
    const posts = watchRefundPosts(page);
    const detail = await expand(page);
    // numbers before: captured / refunded / in progress / refundable
    await expect(detail).toContainText(money(captured));
    const before = posts.length;
    await refundThroughDialog(page, detail, partial, captured);
    expect(posts.length - before).toBe(1);
    const post = posts[posts.length - 1];
    expect(post.key).toMatch(/^[A-Za-z0-9_.:-]{8,128}$/);
    const body = JSON.parse(post.body ?? "{}") as Record<string, unknown>;
    expect(Object.keys(body).sort()).toEqual(["amount_minor", "expected_refundable_minor", "reason"]); // no currency: never client-supplied
    expect(body).toMatchObject({ amount_minor: partial, reason: "requested_by_customer", expected_refundable_minor: captured });
    // state comes from GET: the refund is in progress (held pending at the provider), refundable shrank
    await expect(inProgress(detail)).toBeVisible();
    await expect(detail).toContainText(money(captured - partial));
    await expect(settled(detail)).toHaveCount(0);
    // Refresh on a non-terminal refund is offered and answers without an error banner (throttle => retry later).
    // The page never polls (refund-fulfilment-ui brief: "Refresh button per non-terminal refund"); a refresh re-GETs the
    // list from the server, so that is the merchant action that surfaces the worker's pin.
    const refresh = detail.getByRole("button", { name: ui.refresh }).first();
    await expect(refresh).toBeVisible();
    // R-9: the merchant sees the Stripe refund id for Dashboard tracing once the worker has pinned it
    await expect(async () => {
      await refresh.click();
      // scoped to the order detail: Next's route announcer is a page-level role=alert ("Orders") and is not an error banner
      await expect(detail.getByRole("alert")).toHaveCount(0);
      await expect(detail).toContainText(/re_[A-Za-z0-9_]+/, { timeout: 3_000 });
    }).toPass({ timeout: 45_000, intervals: [1_000, 3_000, 5_000] });
    await expect(page.locator("body")).not.toContainText(/fraudulent/i);
  });
}
);

test.describe(() => {
  test.skip(phase !== "full", "phase full only");
  test("RF11 full refund of the remainder: SUCCEEDED, payment state REFUNDED, no further action", async ({ page }) => {
    await signedLogin(page);
    const posts = watchRefundPosts(page);
    const detail = await expand(page);
    // the earlier partial refund settled at the provider while the buyer half ran
    // (the page never polls, so Refresh until the worker's SUCCEEDED fact is read back)
    await expect(async () => {
      const refresh = detail.getByRole("button", { name: ui.refresh });
      if (await refresh.count()) await refresh.first().click();
      await expect(settled(detail)).toBeVisible({ timeout: 3_000 });
    }).toPass({ timeout: 60_000, intervals: [1_000, 3_000, 5_000] });
    await expect(detail).toContainText(money(captured - partial));
    await refundThroughDialog(page, detail, captured - partial, captured - partial);
    const body = JSON.parse(posts[posts.length - 1].body ?? "{}") as Record<string, unknown>;
    expect(body).toMatchObject({ amount_minor: captured - partial, expected_refundable_minor: captured - partial });
    // The page never polls: the merchant's Refresh re-GETs the refund list and the order, so drive that until the worker's
    // SUCCEEDED fact has made the payment state REFUNDED (the button disappears once no refund is non-terminal).
    await expect(async () => {
      const refresh = detail.getByRole("button", { name: ui.refresh });
      if (await refresh.count()) await refresh.first().click();
      await expect(detail.locator('[data-state="REFUNDED"]').first()).toBeVisible({ timeout: 3_000 });
    }).toPass({ timeout: 60_000, intervals: [1_000, 3_000, 5_000] });
    await expect(detail.getByRole("button", { name: ui.refundAction })).toHaveCount(0); // nothing refundable left
    await expect(detail).toContainText(money(captured));
    // stock and order state are untouched by a refund: still a confirmed order awaiting the merchant's fulfilment
    await expect(detail.locator('[data-state="CONFIRMED"]').first()).toBeVisible();
    await expect(detail.locator('[data-state="MANUAL_UNASSIGNED"]').first()).toBeVisible();
  });

  test("RF11 a member without payments:refund sees the refund section but no action", async ({ page, context }) => {
    await signedLogin(page);
    await asToken(context, restrictedToken);
    await openOrders(page, "en");
    const detail = await expand(page);
    await expect(detail).toContainText(ui.refundSection);
    await expect(detail).toContainText(money(captured));
    await expect(detail.getByRole("button", { name: ui.refundAction })).toHaveCount(0);
    await expect(detail.getByRole("button", { name: ui.refresh })).toHaveCount(0);
    const status = await page.evaluate(async ({ store: s, order: o }) => {
      const r = await fetch(`/api/stores/${s}/orders/${o}/refunds`, { method: "POST", headers: { "content-type": "application/json", "Idempotency-Key": "rf11-restricted-key", "X-CSRF-Token": document.cookie.split("; ").find((c) => c.startsWith("__Host-commerce_csrf="))?.slice(21) ?? "" }, body: JSON.stringify({ amount_minor: 100, reason: "duplicate", expected_refundable_minor: 0 }) });
      return r.status;
    }, { store, order });
    expect([401, 403]).toContain(status); // the server, not the hidden button, is the authority
  });

  for (const locale of ["en", "zh-TW", "zh-CN"] as const) {
    test(`RF11 ${locale}: refund section readable and no overflow at desktop and 390 px`, async ({ page }) => {
      await signedLogin(page);
      await openOrders(page, locale);
      const detail = await expand(page);
      await expect(detail).toContainText(money(captured));
      await page.setViewportSize({ width: 1586, height: 992 });
      await shot(page, "refund", locale, "desktop");
      await page.setViewportSize({ width: 390, height: 844 });
      await expect(detail).toBeVisible();
      await shot(page, "refund", locale, "mobile");
      await page.setViewportSize({ width: 1586, height: 992 });
    });
  }
});
