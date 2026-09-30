// CB11 admin half (contracts/customers-billing-v1.md §5 "Admin screens", §8 CB11; customers-billing-ui.md U1-U6, U9).
// BFF routes exercised through the UI: GET|POST /api/stores/{store}/customers*, GET finance/summary[.csv],
// GET billing[/standing], POST billing/checkout|portal -> Go /v1/admin/stores/{store}/{customers,finance,billing}*.
// Started only by tests/foundation/browser_customers_billing_test.go (build tag browser), which owns the isolated PG, the
// real worker, the real Go API (identity + admin routes, billing service against the independent billingtest fake), the
// production admin Next build, the signed mock IdP and the runner-only control listener. Labels: BROWSER; Stripe is the
// MOCK fake (checkout.stripe.com / billing.stripe.com are answered by page.route, never contacted).
// Locators use the UI unit's data-testid vocabulary and its copy files (customers-copy, billing-copy, claims-copy);
// every assertion restates the contract or a frozen UI default, not the implementation.
import { expect, test, type Page } from "@playwright/test";
import { createHash, randomBytes } from "node:crypto";
import { readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { billingCopy } from "../../apps/admin/lib/billing-copy";
import { claimsCopy } from "../../apps/admin/lib/claims-copy";
import { customersCopy } from "../../apps/admin/lib/customers-copy";

const required = (name: string) => {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required`);
  return value;
};
const origin = required("LC_BROWSER_PUBLIC_ORIGIN");
const evidence = required("LC_BROWSER_EVIDENCE");
const store = required("LC_BROWSER_STORE");
const main = required("LC_BROWSER_CUSTOMER");
const erasable = required("LC_BROWSER_CUSTOMER_ERASE");
const order = required("LC_BROWSER_ORDER");
const scene = required("LC_BROWSER_SESSION");
const price = required("LC_BROWSER_PRICE");
const control = required("LC_BROWSER_CONTROL");
const controlKey = required("LC_BROWSER_CONTROL_KEY");
const phoneTail = required("LC_BROWSER_PHONE_TAIL");
const phoneFull = required("LC_BROWSER_PHONE_FULL"); // digits only
const actorKey = required("LC_BROWSER_ACTOR_KEY");

test.use({ baseURL: origin, trace: "retain-on-failure", screenshot: "only-on-failure" });
test.describe.configure({ mode: "serial" });

const en = customersCopy.en;
const bc = billingCopy.en;
const cc = claimsCopy.en;

async function signedLogin(page: Page) {
  await page.goto(new URL("/en/", origin).toString());
  await page.getByRole("button", { name: "Sign in with identity service" }).click();
  await expect(page.getByTestId("nav-orders")).toBeVisible();
}
async function ctl(resource: string) {
  const response = await fetch(`${control}/${resource}`, { method: "POST", headers: { "X-Gate-Key": controlKey } });
  expect(response.status, `control ${resource}`).toBe(204);
}
const financeDay = (offset = 0) => new Date(Date.now() + 8 * 3600_000 + offset * 86_400_000).toISOString().slice(0, 10);
async function fitsWidth(page: Page) {
  expect(await page.evaluate(() => document.documentElement.scrollWidth - innerWidth)).toBeLessThanOrEqual(1);
}
const manifestPath = path.join(evidence, "screenshots.json");
async function shot(page: Page, name: string, locale: string, viewport: "desktop" | "mobile") {
  const file = path.join(evidence, `${name}-${locale}-${viewport}.png`);
  await page.screenshot({ path: file, fullPage: false, animations: "disabled" });
  await fitsWidth(page);
  let manifest: unknown[] = [];
  try {
    manifest = JSON.parse(await readFile(manifestPath, "utf8"));
  } catch {
    /* first screenshot */
  }
  manifest.push({ File: path.basename(file), Sha256: createHash("sha256").update(await readFile(file)).digest("hex"), Locale: locale, Viewport: viewport });
  await writeFile(manifestPath, JSON.stringify(manifest, null, 2));
}
async function noSecrets(page: Page) {
  const html = await page.content();
  for (const secret of [actorKey, phoneFull, phoneFull.slice(-9), "cs_test_", "cus_", "sub_", "acct_", "fakebearer"]) expect(html).not.toContain(secret);
}
async function storageLacks(page: Page, needles: string[]) {
  const stored = await page.evaluate(() => JSON.stringify({ local: { ...localStorage }, session: { ...sessionStorage } }));
  for (const needle of needles) expect(stored).not.toContain(needle);
}
const keyOf = (headers: Record<string, string>) => headers["idempotency-key"] ?? null;

test("CB11 customers list: table, phone last 3 only, search by name and phone digits, no actor or PSP data, nav entries", async ({ page }) => {
  await signedLogin(page);
  // nav entries (integrator hook of WorkspaceFrame): buttons in the rail, each leads to its page
  for (const section of ["customers", "finance", "billing"] as const)
    await expect(page.getByRole("navigation").getByRole("button", { name: en.nav[section], exact: true })).toBeVisible();
  await page.getByRole("navigation").getByRole("button", { name: en.nav.customers, exact: true }).click();
  await expect(page).toHaveURL(/\/en\/customers/);
  await page.goto(`/en/customers?store=${store}`);
  await expect(page.getByTestId("customers-table")).toBeVisible();
  await expect(page.getByRole("heading", { level: 1 })).toHaveText(en.title);
  const row = page.getByTestId(`customer-row-${main}`);
  await expect(row).toBeVisible();
  await expect(row).toContainText(`${en.phoneEnding} ${phoneTail}`);
  await expect(page.getByTestId(`customer-row-${erasable}`)).toBeVisible();
  // one table, no card grid (audit-first U1)
  await expect(page.locator('[data-testid="customers-table"]')).toHaveCount(1);
  await noSecrets(page);
  // name prefix (case-insensitive) and phone-digit suffix filter through the real q parameter
  await page.getByTestId("customers-search").fill("synth");
  await page.getByTestId("customers-search-submit").click();
  await expect(page.getByTestId(`customer-row-${main}`)).toBeVisible();
  await page.getByTestId("customers-search").fill(phoneFull.slice(-6));
  await page.getByTestId("customers-search-submit").click();
  await expect(page.getByTestId(`customer-row-${main}`)).toBeVisible();
  await page.getByTestId("customers-search").fill("zzzznomatch");
  await page.getByTestId("customers-search-submit").click();
  await expect(page.getByText(en.emptySearch)).toBeVisible();
  await expect(page.locator('[data-testid^="customer-row-"]')).toHaveCount(0);
  await noSecrets(page);
  // the input itself is bounded to 40 characters
  await expect(page.getByTestId("customers-search")).toHaveAttribute("maxlength", "40");
});

test("CB11 customer detail: facts, orders, claims, consent; withdrawal is one keyed strict request; export downloads and the page survives its refresh", async ({ page }) => {
  await signedLogin(page);
  await page.goto(`/en/customers?store=${store}`);
  await page.getByTestId(`customer-open-${main}`).click();
  await expect(page.getByTestId("customer-detail")).toBeVisible();
  await expect(page.getByTestId("customer-orders")).toBeVisible();
  await expect(page.getByTestId(`customer-order-${order}`)).toBeVisible();
  await expect(page.getByTestId(`customer-order-${order}`).getByRole("link").first()).toHaveAttribute("href", /\/en\/orders/);
  await expect(page.getByTestId("customer-claims").locator("tbody tr")).toHaveCount(1);
  await expect(page.getByTestId("consent-marketing_messages")).toContainText(en.consentGranted);
  await expect(page.getByTestId("consent-ads_personalization")).toContainText(en.consentGranted);
  await noSecrets(page);

  // withdrawal: exactly {purpose, channel}, one Idempotency-Key, 201, the UI shows the outcome after a re-read
  const withdrawals: { key: string | null; body: string | null; status: number }[] = [];
  page.on("response", (r) => {
    if (r.request().method() === "POST" && /\/consent-withdrawals$/.test(new URL(r.url()).pathname))
      withdrawals.push({ key: keyOf(r.request().headers()), body: r.request().postData(), status: r.status() });
  });
  await page.getByTestId("withdraw-marketing_messages").click();
  await expect(page.getByText(en.withdrawDone)).toBeVisible();
  expect(withdrawals).toHaveLength(1);
  expect(withdrawals[0].status).toBe(201);
  expect(withdrawals[0].key).toMatch(/^[A-Za-z0-9_.:-]{8,128}$/);
  expect(Object.keys(JSON.parse(withdrawals[0].body ?? "{}")).sort()).toEqual(["channel", "purpose"]);
  expect(JSON.parse(withdrawals[0].body ?? "{}")).toEqual({ purpose: "marketing_messages", channel: "meta_dm" });
  await expect(page.getByTestId("consent-marketing_messages")).toContainText(en.consentNone);
  await expect(page.getByTestId("consent-ads_personalization")).toContainText(en.consentGranted);
  await expect(page.getByTestId("consent-history").locator("tbody tr")).not.toHaveCount(0);
  // the merchant can withdraw, never grant: no grant control exists (CD4)
  await expect(page.getByRole("button", { name: /grant|授予|授權|授权/i })).toHaveCount(0);

  // export: POST with a key and no body, a JSON attachment named customer-<id>.json
  const exports: { key: string | null; body: string | null; status: number }[] = [];
  page.on("response", (r) => {
    if (r.request().method() === "POST" && /\/exports$/.test(new URL(r.url()).pathname))
      exports.push({ key: keyOf(r.request().headers()), body: r.request().postData(), status: r.status() });
  });
  const [download] = await Promise.all([page.waitForEvent("download"), page.getByTestId("customer-download").click()]);
  expect(download.suggestedFilename()).toMatch(new RegExp(`^customer-${main.slice(0, 8)}[0-9a-f-]*\\.json$`)); // the Go header names the full id (D9); the page may shorten it
  const file = (await download.path())!;
  const doc = JSON.parse(await readFile(file, "utf8")) as Record<string, unknown>;
  expect(doc.format).toBe("lc.customer-export.v1");
  expect(doc.customer_id).toBe(main);
  expect(JSON.stringify(doc)).not.toContain(actorKey);
  expect(Array.isArray(doc.orders) && (doc.orders as unknown[]).length >= 1).toBe(true);
  expect(exports).toHaveLength(1);
  expect(exports[0].status).toBe(200);
  expect(exports[0].key).toMatch(/^[A-Za-z0-9_.:-]{8,128}$/);
  expect(exports[0].body ?? "").toBe("");
  await expect(page.getByText(en.downloadDone)).toBeVisible();
  // the EXPORT is now a privacy action of this customer: the detail must still decode and list it after the re-read
  await expect(page.getByText(en.unavailable)).toHaveCount(0);
  await page.reload();
  await expect(page.getByTestId("customer-detail")).toBeVisible();
  await expect(page.getByText(en.unavailable)).toHaveCount(0);
  await expect(page.getByTestId("privacy-actions")).toBeVisible();
  await expect(page.getByTestId("privacy-actions").locator("tbody tr")).toHaveCount(1);
  await expect(page.getByTestId("privacy-actions")).toContainText(/export/i);
  await noSecrets(page);
});

test("CB11 erase: typed confirmation, what is kept, refused while a payment is open, done for an erasable customer", async ({ page }) => {
  await signedLogin(page);
  const posts: { key: string | null; body: string | null; status: number; url: string }[] = [];
  page.on("response", (r) => {
    if (r.request().method() === "POST" && /\/erasure$/.test(new URL(r.url()).pathname))
      posts.push({ key: keyOf(r.request().headers()), body: r.request().postData(), status: r.status(), url: r.url() });
  });
  // (a) the customer with a payment session that may still be open: the dialog works, the server refuses, nothing changes
  await page.goto(`/en/customers/${main}?store=${store}`);
  await expect(page.getByTestId("customer-detail")).toBeVisible();
  await page.getByTestId("customer-erase").click();
  const dialog = page.getByTestId("erase-dialog");
  await expect(dialog).toBeVisible();
  await expect(page.getByTestId("erase-kept")).toHaveText(en.eraseKept);
  const typed = page.getByTestId("erase-typed");
  const submit = page.getByTestId("erase-submit");
  await expect(submit).toBeDisabled();
  for (const wrong of ["erase", "ERAS", "ERASE ", " ERASE", "Erase"]) {
    await typed.fill(wrong);
    await expect(submit).toBeDisabled();
  }
  await page.keyboard.press("Escape");
  await expect(dialog).toBeHidden();
  expect(posts).toHaveLength(0);
  await page.getByTestId("customer-erase").click();
  await typed.fill("");
  await typed.fill("ERASE");
  await expect(submit).toBeEnabled();
  await submit.click();
  await expect(page.getByTestId("erase-problem")).toHaveText(en.errors.erasure_blocked);
  expect(posts).toHaveLength(1);
  expect(posts[0].status).toBe(409);
  expect(posts[0].body).toBe(JSON.stringify({ confirm: "ERASE" }));
  expect(posts[0].key).toMatch(/^[A-Za-z0-9_.:-]{8,128}$/);
  await expect(page.getByTestId("customer-erased")).toHaveCount(0);
  await page.keyboard.press("Escape");

  // (b) the erasable customer: one request, a summary, the erased state survives a reload and shows in the list
  await page.goto(`/en/customers/${erasable}?store=${store}`);
  await expect(page.getByTestId("customer-detail")).toBeVisible();
  await expect(page.getByTestId("customer-erased")).toHaveCount(0);
  await page.getByTestId("customer-erase").click();
  await expect(page.getByTestId("erase-kept")).toHaveText(en.eraseKept);
  await typed.fill("ERASE");
  posts.length = 0;
  await Promise.all([page.waitForResponse((r) => /\/erasure$/.test(new URL(r.url()).pathname)), submit.dblclick()]);
  await expect(page.getByTestId("customer-erased")).toHaveText(en.erasedState);
  expect(posts).toHaveLength(1); // a double click is one request (busy fence)
  expect(posts[0].status).toBe(200);
  expect(posts[0].url).toContain(`/customers/${erasable}/erasure`);
  await page.reload();
  await expect(page.getByTestId("customer-erased")).toBeVisible();
  await expect(page.getByTestId("consent-marketing_messages")).toContainText(en.consentNone);
  await page.goto(`/en/customers?store=${store}`);
  await expect(page.getByTestId(`customer-row-${erasable}`)).toContainText(en.erased);
  await expect(page.getByTestId(`customer-row-${erasable}`)).toContainText(en.consentNone);
  await noSecrets(page);
});

test("CB11 finance: native date inputs, the 91-day rule, one summary table, the CSV, the test-mode badge", async ({ page }) => {
  await signedLogin(page);
  await page.goto(`/en/finance?store=${store}`);
  await expect(page.getByTestId("finance-page")).toBeVisible();
  const from = page.getByTestId("finance-from"), to = page.getByTestId("finance-to");
  await expect(from).toHaveAttribute("type", "date");
  await expect(to).toHaveAttribute("type", "date");
  await from.fill(financeDay(-2));
  await to.fill(financeDay(0));
  await page.getByTestId("finance-show").click();
  await expect(page.getByTestId("finance-table")).toBeVisible();
  const day = financeDay(0);
  await expect(page.getByTestId(`finance-row-${day}-TWD`)).toBeVisible();
  await expect(page.getByTestId("finance-total-TWD")).toBeVisible();
  await expect(page.getByTestId("finance-test-badge")).toHaveText(en.testBadge);
  // the range rule (D13): start after end, and more than 91 days, are refused before any request
  const summaries: string[] = [];
  page.on("request", (r) => {
    const u = new URL(r.url());
    if (!u.pathname.endsWith("/finance/summary")) return;
    // only a request for an INVALID range counts (a late refresh of the already-valid range is not the subject)
    const span = (Date.parse(u.searchParams.get("to") ?? "") - Date.parse(u.searchParams.get("from") ?? "")) / 86_400_000;
    if (!(span >= 0 && span <= 91)) summaries.push(r.url());
  });
  await from.fill(financeDay(0));
  await to.fill(financeDay(-5));
  await expect(page.getByTestId("finance-range-invalid")).toHaveText(en.rangeInvalid);
  await expect(page.getByTestId("finance-show")).toBeDisabled();
  await from.fill(financeDay(-92));
  await to.fill(financeDay(0));
  await expect(page.getByTestId("finance-range-invalid")).toBeVisible();
  await expect(page.getByTestId("finance-show")).toBeDisabled();
  expect(summaries).toHaveLength(0);
  await from.fill(financeDay(-91));
  await expect(page.getByTestId("finance-range-invalid")).toHaveCount(0);
  await from.fill(financeDay(-2));
  await page.getByTestId("finance-show").click();
  await expect(page.getByTestId(`finance-row-${day}-TWD`)).toBeVisible();
  // the CSV: the link carries the same range, the response is an attachment with the frozen header, integers only
  const link = page.getByTestId("finance-csv");
  const href = (await link.getAttribute("href")) ?? "";
  expect(new URL(href, origin).pathname).toBe(`/api/stores/${store}/finance/summary.csv`);
  expect(new URL(href, origin).searchParams.get("from")).toBe(financeDay(-2));
  expect(new URL(href, origin).searchParams.get("to")).toBe(financeDay(0));
  // in-page fetch: the Secure __Host- session cookie is sent by the browser, not by the APIRequestContext jar over http
  const response = await page.evaluate(async (u) => {
    const r = await fetch(u, { credentials: "same-origin" });
    return { status: r.status, headers: Object.fromEntries(r.headers.entries()), text: await r.text() };
  }, new URL(href, origin).toString());
  expect(response.status).toBe(200);
  expect(response.headers["content-type"]).toContain("text/csv");
  expect(response.headers["content-disposition"]).toMatch(/^attachment; filename="finance-\d{4}-\d{2}-\d{2}-\d{4}-\d{2}-\d{2}\.csv"$/);
  expect(response.headers["cache-control"]).toContain("no-store");
  const csv = response.text.trim().split("\n");
  expect(csv[0]).toBe("day,currency,environment,captured_count,captured_minor,refunded_minor,net_minor");
  expect(csv.length).toBeGreaterThanOrEqual(2);
  for (const line of csv.slice(1)) expect(line).toMatch(/^\d{4}-\d{2}-\d{2},[A-Z]{3},[A-Z_]+,\d+,\d+,\d+,-?\d+$/);
  const [download] = await Promise.all([page.waitForEvent("download"), link.click()]);
  expect(download.suggestedFilename()).toMatch(/^finance-.*\.csv$/);
});

test("CB11 billing: standing, subscribe redirects to hosted Checkout (MOCK), the return updates, portal redirect, no URL kept", async ({ page }) => {
  await ctl("standing/unbilled");
  await signedLogin(page);
  await page.route("https://checkout.stripe.com/**", (route) => route.fulfill({ status: 200, contentType: "text/html", body: "<title>MOCK Stripe Checkout</title><p>MOCK</p>" }));
  await page.route("https://billing.stripe.com/**", (route) => route.fulfill({ status: 200, contentType: "text/html", body: "<title>MOCK Stripe portal</title><p>MOCK</p>" }));
  await page.goto(`/en/billing?store=${store}`);
  await expect(page.getByTestId("billing-page")).toBeVisible();
  await expect(page.getByTestId("billing-standing")).toHaveAttribute("data-standing", "UNBILLED");
  await expect(page.getByTestId("billing-test-note")).toHaveText(bc.testNote);
  await expect(page.getByTestId("billing-usage")).toBeVisible();
  await expect(page.getByTestId(`plan-${price}`)).toBeVisible();
  const checkouts: { key: string | null; body: string | null; status: number }[] = [];
  page.on("response", (r) => {
    if (r.request().method() === "POST" && new URL(r.url()).pathname.endsWith("/billing/checkout"))
      checkouts.push({ key: keyOf(r.request().headers()), body: r.request().postData(), status: r.status() });
  });
  await page.getByTestId(`plan-choose-${price}`).click();
  await page.waitForURL(/^https:\/\/checkout\.stripe\.com\/c\/pay\//);
  expect(checkouts).toHaveLength(1);
  expect(checkouts[0].status).toBe(200);
  expect(checkouts[0].key).toBeNull(); // no Idempotency-Key on billing POSTs (§5 step 3 / §6)
  expect(JSON.parse(checkouts[0].body ?? "{}")).toEqual({ price_id: price });
  // the bearer URL is used once and dropped: nothing of it in the admin origin's storage
  await page.goto(`/en/billing?store=${store}&checkout=cancel`);
  await expect(page.getByTestId("billing-cancelled")).toHaveText(bc.checkoutCancel);
  await storageLacks(page, ["checkout.stripe.com", "fakebearer", "cs_test_"]);
  // the customer completes payment at Stripe: the webhook (signed, real handler) mirrors the subscription
  await ctl("checkout-complete");
  await page.goto(`/en/billing?store=${store}&checkout=done`);
  await expect(page.getByTestId("billing-updating")).toBeVisible();
  await expect(page.getByTestId("billing-standing")).toHaveAttribute("data-standing", "GOOD", { timeout: 20_000 });
  await expect(page.getByTestId("billing-subscriptions")).toContainText(bc.subStatus.active);
  await expect(page.getByTestId(`plan-choose-${price}`)).toBeDisabled(); // a live subscription: manage it, do not buy a second (409 rule)
  await expect(page).toHaveURL(new RegExp(`/en/billing\\?store=${store}$`)); // the return marker is dropped
  const portals: { key: string | null; body: string | null; status: number }[] = [];
  page.on("response", (r) => {
    if (r.request().method() === "POST" && new URL(r.url()).pathname.endsWith("/billing/portal"))
      portals.push({ key: keyOf(r.request().headers()), body: r.request().postData(), status: r.status() });
  });
  await page.getByTestId("billing-portal").click();
  await page.waitForURL(/^https:\/\/billing\.stripe\.com\/p\/session\//);
  expect(portals).toHaveLength(1);
  expect(portals[0].status).toBe(200);
  expect(portals[0].key).toBeNull();
  expect(portals[0].body ?? "").toBe("");
  await page.goto(`/en/billing?store=${store}`);
  await storageLacks(page, ["billing.stripe.com", "fakebearer"]);
});

test("CB11 banner: GRACE and RESTRICTED show one banner on every page linking to billing; GOOD and UNBILLED show none", async ({ page }) => {
  await signedLogin(page);
  const pages = [`/en/customers?store=${store}`, `/en/finance?store=${store}`, `/en/orders?store=${store}`, `/en/billing?store=${store}`];
  const texts: Record<string, string> = {};
  for (const standing of ["good", "unbilled", "grace", "restricted"]) {
    await ctl(`standing/${standing}`);
    for (const url of pages) {
      await page.goto(url);
      await expect(page.locator("main, [data-testid$='-page'], [data-testid='merchant-orders']").first()).toBeVisible();
      const banner = page.getByTestId("billing-banner");
      if (standing === "grace" || standing === "restricted") {
        await expect(banner).toBeVisible();
        await expect(banner).toHaveCount(1);
        await expect(page.getByTestId("billing-banner-link")).toHaveAttribute("href", `/en/billing?store=${store}`);
        texts[standing] = (await banner.innerText()).trim();
      } else {
        await expect(banner).toHaveCount(0);
      }
    }
  }
  expect(texts.grace).not.toBe(texts.restricted); // one shape, two messages
  expect(texts.grace.length).toBeGreaterThan(10);
  // the banner links to billing and the link works
  await ctl("standing/restricted");
  await page.goto(`/en/customers?store=${store}`);
  await page.getByTestId("billing-banner-link").click();
  await expect(page.getByTestId("billing-page")).toBeVisible();
  await expect(page.getByTestId("billing-standing")).toHaveAttribute("data-standing", "RESTRICTED");
});

test("CB11 studio: opening a claim window under RESTRICTED says why and links to billing; nothing else changes; GOOD opens it", async ({ page }) => {
  await signedLogin(page);
  await ctl("standing/restricted");
  await page.goto(`/en/studio/claims?store=${store}&scene=${scene}`);
  await expect(page.getByTestId("merchant-claims")).toBeVisible();
  await expect(page.getByTestId("claims-window-state")).toHaveText("Closed");
  const posts: number[] = [];
  page.on("response", (r) => {
    if (r.request().method() === "POST" && new URL(r.url()).pathname.endsWith("/claims/window")) posts.push(r.status());
  });
  await page.getByRole("button", { name: cc.openWindow }).click();
  const alert = page.getByRole("alert").filter({ hasText: cc.billingRestricted });
  await expect(alert).toBeVisible();
  expect(posts).toEqual([402]);
  await expect(page.getByTestId("claims-billing-link")).toHaveAttribute("href", `/en/billing?store=${store}`);
  await expect(page.getByTestId("claims-window-state")).toHaveText("Closed");
  await expect(alert).not.toContainText(/permission|forbidden/i);
  // the rest of Studio is unaffected by standing
  await expect(page.getByRole("button", { name: cc.openWindow })).toBeEnabled();
  await ctl("standing/good");
  await page.getByRole("button", { name: cc.openWindow }).click();
  await expect(page.getByTestId("claims-window-state")).toHaveText("Open");
  expect(posts).toEqual([402, 200]);
  await page.getByRole("button", { name: cc.closeWindow }).click();
  await expect(page.getByTestId("claims-window-state")).toHaveText("Closed");
});

for (const locale of ["en", "zh-TW"] as const) {
  for (const viewport of ["desktop", "mobile"] as const) {
    test(`CB11 screenshots ${locale} ${viewport}: customers, detail, finance, billing with banner, Studio refusal (hashed, no overflow)`, async ({ browser }) => {
      const context = await browser.newContext(viewport === "mobile" ? { viewport: { width: 390, height: 844 } } : { viewport: { width: 1586, height: 992 } });
      const page = await context.newPage();
      try {
        await signedLogin(page);
        await ctl("standing/restricted");
        const c = customersCopy[locale];
        await page.goto(`/${locale}/customers?store=${store}`);
        await expect(page.getByTestId("customers-table")).toBeVisible();
        await expect(page.locator("html")).toHaveAttribute("lang", locale);
        await expect(page.getByRole("heading", { level: 1 })).toHaveText(c.title);
        await expect(page.getByTestId("billing-banner")).toBeVisible();
        await shot(page, "customers", locale, viewport);
        await page.goto(`/${locale}/customers/${main}?store=${store}`);
        await expect(page.getByTestId("customer-detail")).toBeVisible();
        await expect(page.getByTestId("customer-privacy")).toBeVisible();
        await shot(page, "customer-detail", locale, viewport);
        await page.goto(`/${locale}/finance?store=${store}`);
        await expect(page.getByTestId("finance-page")).toBeVisible();
        await expect(page.getByRole("heading", { level: 1 })).toHaveText(c.financeTitle);
        await shot(page, "finance", locale, viewport);
        await page.goto(`/${locale}/billing?store=${store}`);
        await expect(page.getByTestId("billing-standing")).toHaveAttribute("data-standing", "RESTRICTED");
        await shot(page, "billing", locale, viewport);
        await page.goto(`/${locale}/studio/claims?store=${store}&scene=${scene}`);
        await expect(page.getByTestId("merchant-claims")).toBeVisible();
        await page.getByRole("button", { name: claimsCopy[locale].openWindow }).click();
        await expect(page.getByTestId("claims-billing-link")).toBeVisible();
        await expect(page.getByTestId("claims-billing-link")).toHaveAttribute("href", `/${locale}/billing?store=${store}`);
        await shot(page, "studio-refusal", locale, viewport);
        // every dialog and control keeps a touch-sized hit area on mobile (44 px)
        if (viewport === "mobile") {
          await page.goto(`/${locale}/customers/${main}?store=${store}`);
          await expect(page.getByTestId("customer-erase")).toBeVisible();
          const box = await page.getByTestId("customer-erase").boundingBox();
          expect(box?.height ?? 0).toBeGreaterThanOrEqual(40);
        }
      } finally {
        await context.close();
      }
    });
  }
}

// a cookie-swapped member without the permissions must see refusals, never data (BFF fence)
test("CB11 permission fence: a member without customers:read / billing:manage sees refusals in the UI", async ({ browser }) => {
  const restrictedToken = required("LC_BROWSER_RESTRICTED_TOKEN");
  const context = await browser.newContext();
  const page = await context.newPage();
  try {
    const csrf = randomBytes(32).toString("base64url");
    const url = origin.replace(/^http:/, "https:");
    await context.addCookies([
      { name: "__Host-commerce_session", value: restrictedToken, url, secure: true, httpOnly: true, sameSite: "Lax" },
      { name: "__Host-commerce_csrf", value: csrf, url, secure: true, httpOnly: false, sameSite: "Lax" },
    ]);
    await page.goto(`/en/customers?store=${store}`);
    await expect(page.getByText(en.forbidden)).toBeVisible();
    await expect(page.getByTestId("customers-table")).toHaveCount(0);
    await page.goto(`/en/billing?store=${store}`);
    await expect(page.getByText(bc.forbidden)).toBeVisible();
    await expect(page.getByTestId("billing-standing")).toHaveCount(0);
    // finance reads under orders:read (contract 6, read_finance_summary): this member may see it, the store:read-only one may not
    await page.goto(`/en/finance?store=${store}`);
    await expect(page.getByTestId("finance-page")).toBeVisible();
    await expect(page.getByText(en.financeForbidden)).toHaveCount(0);
    const bare = await browser.newContext();
    try {
      await bare.addCookies([
        { name: "__Host-commerce_session", value: required("LC_BROWSER_NOFIN_TOKEN"), url, secure: true, httpOnly: true, sameSite: "Lax" },
        { name: "__Host-commerce_csrf", value: csrf, url, secure: true, httpOnly: false, sameSite: "Lax" },
      ]);
      const bp = await bare.newPage();
      await bp.goto(new URL(`/en/finance?store=${store}`, origin).toString());
      await expect(bp.getByText(en.financeForbidden)).toBeVisible();
      await expect(bp.getByTestId("finance-table")).toHaveCount(0);
    } finally {
      await bare.close();
    }
    // the banner reads store:read only: a member with just store:read still sees it when the store is restricted
    await ctl("standing/restricted");
    await page.goto(`/en/orders?store=${store}`);
    await expect(page.getByTestId("billing-banner")).toBeVisible();
    await ctl("standing/good");
  } finally {
    await context.close();
  }
});
