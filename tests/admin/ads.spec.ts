// MA09a admin half (contracts/meta-ads-v1.md §9 MA09, §2 flow, §5.1 statuses, §5.3 X7, §7, §12; ads-ui U1-U8; rulings O4, AD9).
// BFF routes exercised through the UI: POST /api/ads/meta/connect, GET /api/ads/meta/callback (the FLfB redirect URI),
// GET|POST /api/stores/{store}/ads/{settings,meta/states/*,meta/bindings,drafts,drafts/*,report,capi} -> Go
// /v1/admin/stores/{store_id}/ads/*. Started only by tests/foundation/browser_meta_ads_test.go (build tag browser), which owns the
// isolated PG, the real Go API + ads worker (dispatcher, sweepers), the fake Graph, the production admin Next build, the signed mock
// IdP and the runner-only control listener. Labels: BROWSER; Meta is MOCK: https://www.facebook.com/... is answered by page.route
// with a 302 to the app's own callback, the code exchange happens in the API process against the fake Graph.
// Locators use the UI unit's data-testid vocabulary and its copy file; every assertion restates the contract or a frozen UI default.
import { expect, test, type Page } from "@playwright/test";
import { createHash } from "node:crypto";
import { readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { adsCopy } from "../../apps/admin/lib/ads-copy";

const required = (name: string) => {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required`);
  return value;
};
const origin = required("LC_BROWSER_PUBLIC_ORIGIN");
const evidence = required("LC_BROWSER_EVIDENCE");
const store = required("LC_BROWSER_STORE");
const account = required("LC_BROWSER_ACCOUNT");
const pageAsset = required("LC_BROWSER_PAGE_ASSET");
const business = required("LC_BROWSER_CLIENT_BUSINESS");
const control = required("LC_BROWSER_CONTROL");
const controlKey = required("LC_BROWSER_CONTROL_KEY");
const appId = required("LC_BROWSER_APP_ID");
const configId = required("LC_BROWSER_CONFIG_ID");
const redirect = required("LC_BROWSER_REDIRECT");
const graphVersion = required("LC_BROWSER_GRAPH_VERSION");
const secrets: string[] = JSON.parse(required("LC_BROWSER_SECRETS"));

test.use({ baseURL: origin, trace: "retain-on-failure", screenshot: "only-on-failure" });

const en = adsCopy.en;
const uuidRe = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
let firstDraft = "";
let copyDraft = "";

async function signedLogin(page: Page) {
  await page.goto(new URL("/en/", origin).toString());
  await page.getByRole("button", { name: "Sign in with identity service" }).click();
  await expect(page.getByTestId("nav-orders")).toBeVisible();
}
async function ctl(resource: string) {
  const response = await fetch(`${control}/${resource}`, { method: "POST", headers: { "X-Gate-Key": controlKey } });
  expect([200, 204], `control ${resource}`).toContain(response.status);
  return response;
}
async function graph() {
  const response = await fetch(`${control}/graph`, { method: "POST", headers: { "X-Gate-Key": controlKey } });
  expect(response.status).toBe(200);
  return (await response.json()) as {
    violations: string[]; oauth_exchanges: number; status_posts: number; token_sources: string[]; adset_bodies: string[];
    campaign: { ID: string; Name: string; Status: string; Effective: string }[];
    adset: { ID: string; Status: string; Effective: string }[];
    creative: { ID: string }[]; ad: { ID: string; Status: string; Effective: string }[];
  };
}
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
  const stored = await page.evaluate(() => JSON.stringify({ local: { ...localStorage }, session: { ...sessionStorage }, cookie: document.cookie }));
  for (const secret of secrets) {
    expect(html).not.toContain(secret);
    expect(stored).not.toContain(secret);
  }
}
const localInput = (offsetMs: number) => {
  const d = new Date(Date.now() + offsetMs);
  const p = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
};
async function openAds(page: Page, locale = "en") {
  await page.goto(`/${locale}/ads?store=${store}`);
  await expect(page.getByTestId("merchant-ads")).toBeVisible();
  await expect(page.getByTestId("ads-connection")).toBeVisible();
}
async function driveUntil(page: Page, status: string) {
  await expect(async () => {
    await ctl("drive");
    await page.getByTestId("ads-refresh").click();
    await expect(page.getByTestId("ads-detail")).toHaveAttribute("data-status", status, { timeout: 2500 });
  }).toPass({ timeout: 90_000, intervals: [500, 1000, 2000] });
}
const bodies: { url: string; status: number; text: string }[] = [];
function recordBodies(page: Page) {
  page.on("response", async (response) => {
    try {
      const url = new URL(response.url());
      if (url.origin !== origin) return;
      const type = response.headers()["content-type"] ?? "";
      if (!/json|text|html/.test(type)) return;
      bodies.push({ url: url.pathname, status: response.status(), text: (await response.text()).slice(0, 200_000) });
    } catch {
      /* redirects and aborted responses have no body */
    }
  });
}

test.describe("MA09a flow", () => {
test.describe.configure({ mode: "serial" });

test("MA09a connect through the fake FLfB dialog: dialog params, state cookie, 303 without code/state, pick list, bind, sealed token never in the browser", async ({ page }) => {
  recordBodies(page);
  await signedLogin(page);
  await openAds(page);
  await expect(page.getByRole("navigation").getByRole("button", { name: en.title, exact: true })).toBeVisible();
  await expect(page.getByTestId("ads-sandbox")).toHaveText(en.sandboxBanner); // AD9: the store is SANDBOX
  await expect(page.getByTestId("ads-conn-empty")).toHaveText(en.connEmpty);
  await expect(page.getByTestId("ads-budget-note")).toHaveText(en.budgetNote); // §12: never a real-time hard stop
  await expect(page.getByTestId("ads-identities")).toContainText(pageAsset); // the Facebook Page identity of the store
  await noSecrets(page);

  let dialog: URL | null = null;
  let connectSetCookie: string[] = [];
  const callbackResponses: { status: number; headers: Record<string, string> }[] = [];
  page.on("response", async (response) => {
    const url = new URL(response.url());
    if (url.pathname === "/api/ads/meta/connect" && response.request().method() === "POST") connectSetCookie = (await response.headersArray()).filter((h) => h.name.toLowerCase() === "set-cookie").map((h) => h.value);
    if (url.pathname === "/api/ads/meta/callback") callbackResponses.push({ status: response.status(), headers: response.headers() });
  });
  await page.route("https://www.facebook.com/**", async (route) => {
    const url = new URL(route.request().url());
    dialog = url;
    const { code } = (await (await ctl("oauth/code")).json()) as { code: string };
    await route.fulfill({ status: 302, headers: { Location: `${origin}/api/ads/meta/callback?code=${encodeURIComponent(code)}&state=${encodeURIComponent(url.searchParams.get("state") ?? "")}` } });
  });
  await page.getByTestId("ads-connect").click();
  await expect(page.getByTestId("ads-pick")).toBeVisible({ timeout: 30_000 });

  // §2 step 1: FLfB dialog on app 大梦 with config_id, response_type=code, override_default_response_type, the fixed redirect, a random state
  expect(dialog).not.toBeNull();
  const d = dialog as unknown as URL;
  expect(d.origin).toBe("https://www.facebook.com");
  expect(d.pathname).toBe(`/${graphVersion}/dialog/oauth`);
  expect(d.searchParams.get("client_id")).toBe(appId);
  expect(d.searchParams.get("config_id")).toBe(configId);
  expect(d.searchParams.get("response_type")).toBe("code");
  expect(d.searchParams.get("override_default_response_type")).toBe("true");
  expect(d.searchParams.get("redirect_uri")).toBe(redirect);
  expect(d.searchParams.get("state") ?? "").toMatch(/^[A-Za-z0-9_-]{43}$/);
  expect(d.searchParams.has("scope")).toBe(false); // FLfB: config_id replaces scope
  expect(d.searchParams.has("client_secret")).toBe(false);
  // U1: the binding cookie is httpOnly, Lax, scoped to the callback path, short-lived, and holds the store only
  const cookieLine = connectSetCookie.find((c) => c.startsWith("lc_ads_connect="));
  expect(cookieLine, "lc_ads_connect Set-Cookie on the connect response").toBeTruthy();
  expect(cookieLine!).toContain(`lc_ads_connect=${store}`);
  expect(cookieLine!).toMatch(/HttpOnly/i);
  expect(cookieLine!).toMatch(/SameSite=Lax/i);
  expect(cookieLine!).toMatch(/Path=\/api\/ads\/meta\/callback/i);
  expect(cookieLine!).toMatch(/Max-Age=600/i);
  // U2: the callback answers 303, no-store, no-referrer, clears the cookie, and never echoes code/state into the target
  const cb = callbackResponses.find((r) => r.status === 303);
  expect(cb, "callback 303").toBeTruthy();
  expect(cb!.headers["location"]).toMatch(new RegExp(`^/en/ads\\?store=${store}&connect=[0-9a-f-]{36}$|^/[a-zA-Z-]+/ads\\?store=${store}&connect=[0-9a-f-]{36}$`));
  expect(cb!.headers["location"]).not.toMatch(/code=|state=/);
  expect(cb!.headers["cache-control"]).toMatch(/no-store/);
  expect(cb!.headers["referrer-policy"]).toBe("no-referrer");
  expect(await page.context().cookies()).not.toEqual(expect.arrayContaining([expect.objectContaining({ name: "lc_ads_connect", value: store })]));
  const url = new URL(page.url());
  expect(url.searchParams.get("connect") ?? "").toMatch(uuidRe);
  expect(url.search).not.toMatch(/code=|state=SYNTH|&state=/);

  // U3: only accounts of THIS login are offered; a POST that names an account outside the pick list is refused (nothing is bound)
  await expect(page.locator('[data-testid^="ads-pick-"][type="radio"]')).toHaveCount(1);
  await expect(page.getByTestId(`ads-pick-${account}`)).toBeVisible();
  await page.getByTestId(`ads-pick-${account}`).check();
  const foreign = "1" + "9".repeat(11);
  let refused = 0;
  await page.route("**/api/stores/*/ads/meta/bindings", async (route) => {
    const body = JSON.parse(route.request().postData() ?? "{}");
    await route.continue({ postData: JSON.stringify({ ...body, ad_account_id: foreign }) });
  });
  page.once("response", (r) => { if (/\/ads\/meta\/bindings$/.test(new URL(r.url()).pathname)) refused = r.status(); });
  await page.getByTestId("ads-pick-submit").click();
  await expect(page.getByTestId("ads-banner")).toBeVisible();
  expect(refused).toBe(422);
  await page.unroute("**/api/stores/*/ads/meta/bindings");
  // the real pick: exactly {state_id, ad_account_id}, one Idempotency-Key
  const binds: { key: string | null; body: string | null; status: number }[] = [];
  page.on("response", (r) => {
    if (r.request().method() === "POST" && /\/ads\/meta\/bindings$/.test(new URL(r.url()).pathname))
      binds.push({ key: r.request().headers()["idempotency-key"] ?? null, body: r.request().postData(), status: r.status() });
  });
  await page.getByTestId("ads-pick-submit").click();
  await expect(page.getByTestId("ads-banner")).toHaveText(en.connected);
  expect(binds).toHaveLength(1);
  expect(binds[0].status).toBe(201);
  expect(binds[0].key).toMatch(/^[A-Za-z0-9_.:-]{8,128}$/);
  expect(Object.keys(JSON.parse(binds[0].body ?? "{}")).sort()).toEqual(["ad_account_id", "state_id"]);
  await expect(page.getByTestId("ads-connections")).toContainText(account);
  await expect(page.getByTestId("ads-connections")).toContainText(business);
  await expect(page.getByTestId("ads-connect")).toBeEnabled();
  expect(new URL(page.url()).searchParams.has("connect")).toBe(false);
  await noSecrets(page);
  for (const b of bodies) for (const s of secrets) expect(b.text, `${b.url} carries a Meta secret`).not.toContain(s);
  const g = await graph();
  expect(g.oauth_exchanges).toBe(1);
  expect(g.violations).toEqual([]);
});

test("MA09a state_mismatch, state_expired, meta_connect_failed and Start again: fixed codes, never a guess, the code is not exchanged on a mismatch", async ({ page }) => {
  await signedLogin(page);
  await openAds(page);
  const before = (await graph()).oauth_exchanges;
  // (a) a return that did not start in this browser: no binding cookie -> state_mismatch, no store guessed
  await page.goto(`/api/ads/meta/callback?code=SYNTH-CODE-x&state=${"A".repeat(43)}`);
  await expect(page).toHaveURL(/\/(en|zh-TW|zh-CN)\/ads\?[^#]*connect_error=state_mismatch/);
  await expect(page.getByTestId("ads-connect-error")).toContainText(adsCopy[(new URL(page.url()).pathname.split("/")[1] as "en" | "zh-TW" | "zh-CN")].connectErrors.state_mismatch);
  expect((await graph()).oauth_exchanges).toBe(before);

  // (b) the return carries a tampered state: the state row does not match -> state_mismatch, still no exchange
  await openAds(page);
  await page.route("https://www.facebook.com/**", async (route) => {
    const u = new URL(route.request().url());
    const state = u.searchParams.get("state") ?? "";
    const tampered = state.slice(0, -1) + (state.endsWith("A") ? "B" : "A");
    const { code } = (await (await ctl("oauth/code")).json()) as { code: string };
    await route.fulfill({ status: 302, headers: { Location: `${origin}/api/ads/meta/callback?code=${encodeURIComponent(code)}&state=${encodeURIComponent(tampered)}` } });
  });
  await page.getByTestId("ads-connect").click();
  await expect(page.getByTestId("ads-connect-error")).toContainText(en.connectErrors.state_mismatch, { timeout: 30_000 });
  expect((await graph()).oauth_exchanges).toBe(before);
  await page.unroute("https://www.facebook.com/**");

  // (c) the state expired while the merchant was on Meta's page -> state_expired
  await openAds(page);
  await page.route("https://www.facebook.com/**", async (route) => {
    const u = new URL(route.request().url());
    await ctl("oauth/expire");
    const { code } = (await (await ctl("oauth/code")).json()) as { code: string };
    await route.fulfill({ status: 302, headers: { Location: `${origin}/api/ads/meta/callback?code=${encodeURIComponent(code)}&state=${encodeURIComponent(u.searchParams.get("state") ?? "")}` } });
  });
  await page.getByTestId("ads-connect").click();
  await expect(page.getByTestId("ads-connect-error")).toContainText(en.connectErrors.state_expired, { timeout: 30_000 });
  await page.unroute("https://www.facebook.com/**");

  // (d) Meta refuses the code (never issued) -> meta_connect_failed (502), and Start again opens a fresh dialog
  await openAds(page);
  await page.route("https://www.facebook.com/**", async (route) => {
    const u = new URL(route.request().url());
    await route.fulfill({ status: 302, headers: { Location: `${origin}/api/ads/meta/callback?code=SYNTH-CODE-never-issued&state=${encodeURIComponent(u.searchParams.get("state") ?? "")}` } });
  });
  await page.getByTestId("ads-connect").click();
  await expect(page.getByTestId("ads-connect-error")).toContainText(en.connectErrors.meta_connect_failed, { timeout: 30_000 });
  let dialogs = 0;
  await page.unroute("https://www.facebook.com/**");
  await page.route("https://www.facebook.com/**", async (route) => {
    dialogs++;
    await route.abort();
  });
  await page.getByTestId("ads-connect-error").getByRole("button", { name: en.startAgain }).click();
  await expect.poll(() => dialogs).toBeGreaterThan(0);
  await page.unroute("https://www.facebook.com/**");
  const g = await graph();
  expect(g.violations).toEqual([]);
  expect(g.oauth_exchanges).toBe(before + 1); // only (d) reached Meta: the never-issued code was presented once and refused; (a) (b) (c) never exchanged
});

test("MA09a draft -> approve -> publish -> pause -> copy: statuses, one keyed request each, the campaign is only ever switched by activate and pause, X7 no resume", async ({ page }) => {
  await signedLogin(page);
  await openAds(page);
  const requests: { method: string; url: string; key: string | null; ifMatch: string | null; body: string | null; status: number }[] = [];
  page.on("response", (r) => {
    const u = new URL(r.url());
    if (r.request().method() !== "GET" && /\/api\/stores\/[^/]+\/ads\//.test(u.pathname))
      requests.push({ method: r.request().method(), url: u.pathname, key: r.request().headers()["idempotency-key"] ?? null, ifMatch: r.request().headers()["if-match"] ?? null, body: r.request().postData(), status: r.status() });
  });
  // new draft
  await page.getByTestId("ads-new-draft").click();
  await expect(page.getByTestId("ads-form")).toBeVisible();
  await expect(page.getByTestId("ads-template-BOOST_POST")).toBeChecked();
  await expect(page.getByTestId("ads-f-countries")).toHaveValue(/TW/);
  await expect(page.getByTestId("ads-f-agemin")).toHaveValue("18");
  await expect(page.getByTestId("ads-f-agemax")).toHaveValue("65");
  await expect(page.getByTestId("ads-f-account")).toHaveValue(/[0-9a-f-]{36}/); // the only connected account is preselected
  await page.getByTestId("ads-f-source").fill(`${pageAsset}_1234567890`);
  await page.getByTestId("ads-f-budget").fill("3000");
  await page.getByTestId("ads-f-starts").fill(localInput(3 * 3600_000));
  await page.getByTestId("ads-f-ends").fill(localInput(27 * 3600_000));
  await page.getByTestId("ads-f-save").click();
  await expect(page.getByTestId("ads-banner")).toHaveText(en.saved);
  await expect(page.getByTestId("ads-detail")).toHaveAttribute("data-status", "DRAFT");
  const created = requests.find((r) => r.method === "POST" && /\/ads\/drafts$/.test(r.url));
  expect(created?.status).toBe(201);
  expect(created?.key).toMatch(/^[A-Za-z0-9_.:-]{8,128}$/);
  const input = JSON.parse(created?.body ?? "{}");
  expect(input.lifetime_budget_minor).toBe(300000); // NT$3000 in minor units, integer math only
  expect(input.currency).toBe("TWD");
  expect(Object.keys(input).sort()).toEqual(["ad_binding_id", "age_max", "age_min", "countries", "currency", "ends_at", "identity_binding_id", "lifetime_budget_minor", "source_ref", "starts_at", "template"]);
  firstDraft = (await page.getByTestId(/^ads-draft-/).first().getAttribute("data-testid"))!.replace("ads-draft-", "");
  expect(firstDraft).toMatch(uuidRe);
  await expect(page.getByTestId("ads-detail")).toContainText("3,000");

  // approve
  await page.getByTestId("ads-approve").click();
  await expect(page.getByTestId("ads-banner")).toHaveText(en.approved);
  await expect(page.getByTestId("ads-detail")).toHaveAttribute("data-status", "APPROVED");
  const approval = requests.find((r) => /\/approve$/.test(r.url));
  expect(JSON.parse(approval?.body ?? "{}")).toEqual({ revision: 1 });
  // an approved draft is frozen: no Edit
  await expect(page.getByTestId("ads-edit")).toHaveCount(0);

  // publish, then the sweeper + dispatcher (control) walk the chain
  await page.getByTestId("ads-publish").click();
  await expect(page.getByTestId("ads-banner")).toHaveText(en.published);
  const publish = requests.find((r) => /\/publish$/.test(r.url));
  expect(publish?.status).toBe(200);
  expect(publish?.key).toMatch(/^[A-Za-z0-9_.:-]{8,128}$/);
  expect(JSON.parse(publish?.body ?? "{}")).toEqual({ publish_attempt: 0 });
  await driveUntil(page, "ACTIVE");
  await expect(page.getByTestId("ads-ops")).toContainText("activate");
  for (const kind of ["campaign", "adset", "creative", "ad", "preflight", "activate"]) await expect(page.getByTestId("ads-ops")).toContainText(kind);
  await expect(page.getByTestId("ads-ops").getByText(en.opStates.SUCCEEDED).first()).toBeVisible();
  const link = await page.getByTestId("ads-manager-link").getAttribute("href");
  expect(new URL(link!).protocol).toBe("https:");
  expect(link!).toContain(account);
  expect(await page.getByTestId("ads-manager-link").getAttribute("rel")).toContain("noopener");
  let g = await graph();
  expect(g.campaign).toHaveLength(1);
  expect(g.campaign[0].Status).toBe("ACTIVE");
  expect(g.adset[0].Effective).toBe("ACTIVE");
  expect(g.adset_bodies[0]).toContain('"lifetime_budget":3000'); // AD4 + F19: TWD offset 1
  expect(g.adset_bodies[0]).not.toContain("daily_budget");
  expect(g.status_posts).toBe(1); // AD3: the only spend switch
  await expect(page.getByTestId("ads-remote")).toContainText(g.campaign[0].ID);
  await expect(page.getByTestId("ads-remote")).toContainText(g.ad[0].ID);
  await expect(page.getByTestId("ads-pause")).toBeVisible();
  await expect(page.getByTestId("ads-publish")).toHaveCount(0);
  await noSecrets(page);

  // insights are read while the draft is ACTIVE (it counts toward the allowance, so reads are planned hourly, D9)
  await ctl("seed/insights");
  await ctl("sweep/insights");

  // pause: always available; afterwards only "copy to a new draft", never a resume (X7)
  await page.getByTestId("ads-pause").click();
  await expect(page.getByTestId("ads-banner")).toHaveText(en.paused);
  await driveUntil(page, "PAUSED");
  g = await graph();
  expect(g.campaign[0].Status).toBe("PAUSED");
  expect(g.status_posts).toBe(2);
  // U5: Pause stays available while any remote id exists (a pause is always allowed, §5.3); what must appear is the copy action
  await expect(page.getByTestId("ads-copy")).toBeVisible();
  await expect(page.getByTestId("ads-actions").getByRole("button", { name: /resume|reactivate|re-activate|activate|重新啟用|恢復|恢复/i })).toHaveCount(0);
  await expect(page.getByTestId(`ads-draft-${firstDraft}`).locator("[data-state]")).toHaveAttribute("data-state", "PAUSED");

  // copy: a NEW draft (new id, fresh approval needed); the paused one stays paused
  await page.getByTestId("ads-copy").click();
  await expect(page.getByTestId("ads-form")).toBeVisible();
  await expect(page.getByTestId("ads-f-budget")).toHaveValue("3000");
  await expect(page.getByTestId("ads-f-source")).toHaveValue(new RegExp(`^${pageAsset}_`));
  await page.getByTestId("ads-f-starts").fill(localInput(4 * 3600_000));
  await page.getByTestId("ads-f-ends").fill(localInput(28 * 3600_000));
  await page.getByTestId("ads-f-save").click();
  await expect(page.getByTestId("ads-banner")).toHaveText(en.saved);
  await expect(page.getByTestId("ads-detail")).toHaveAttribute("data-status", "DRAFT");
  const ids = await page.getByTestId(/^ads-draft-/).evaluateAll((els) => els.map((e) => e.getAttribute("data-testid")!.replace("ads-draft-", "")));
  expect(ids).toHaveLength(2);
  copyDraft = ids.find((id) => id !== firstDraft)!;
  expect(copyDraft).toMatch(uuidRe);
  await expect(page.getByTestId(`ads-draft-${firstDraft}`).locator("[data-state]")).toHaveAttribute("data-state", "PAUSED");
  for (const r of requests) expect(r.status, `${r.method} ${r.url}`).toBeLessThan(300);
  expect((await graph()).violations).toEqual([]);
});

test("MA09a allowance off (O4) disables approve with the copy; SANDBOX banner follows the environment (AD9)", async ({ page }) => {
  await signedLogin(page);
  await ctl("allowance/off");
  await openAds(page);
  await expect(page.getByTestId("ads-allowance-off")).toHaveText(en.allowanceOff);
  await expect(page.getByTestId("ads-allowance")).toHaveCount(0);
  await page.getByTestId(`ads-open-${copyDraft}`).click();
  await expect(page.getByTestId("ads-detail")).toHaveAttribute("data-status", "DRAFT");
  await expect(page.getByTestId("ads-approve")).toBeDisabled();
  await expect(page.getByText(en.approveOffHint)).toBeVisible();
  await ctl("allowance/on");
  await page.getByTestId("ads-refresh").click();
  await expect(page.getByTestId("ads-allowance-off")).toHaveCount(0);
  await expect(page.getByTestId("ads-allowance")).toBeVisible();
  await expect(page.getByTestId("ads-sandbox")).toBeVisible();
  await ctl("env/live");
  await page.getByTestId("ads-refresh").click();
  await expect(page.getByTestId("ads-sandbox")).toHaveCount(0);
  await ctl("env/sandbox");
  await page.getByTestId("ads-refresh").click();
  await expect(page.getByTestId("ads-sandbox")).toBeVisible();
});

test("MA09a report: three separate blocks with window, timezone and fetched_at, no combined figure, budget is not a hard stop", async ({ page }) => {
  await signedLogin(page);
  await openAds(page);
  await page.getByTestId("ads-report-load").click();
  await expect(page.getByTestId("ads-block-orders")).toBeVisible();
  await expect(page.getByTestId("ads-block-delivery")).toBeVisible();
  await expect(page.getByTestId("ads-block-reported")).toBeVisible();
  await expect(page.getByTestId("ads-report-separate")).toHaveText(en.reportSeparate);
  for (const block of ["ads-block-orders", "ads-block-delivery", "ads-block-reported"]) {
    await expect(page.getByTestId(block).getByRole("heading")).toHaveCount(1);
  }
  await expect(page.getByTestId("ads-block-delivery")).toContainText(en.reportTimezone);
  await expect(page.getByTestId("ads-block-delivery")).toContainText(en.reportFetched);
  await expect(page.getByTestId("ads-block-reported")).toContainText(en.reportFetched);
  await expect(page.getByTestId("ads-block-delivery")).toContainText(/3,000/); // impressions 3 x 1000
  await expect(page.getByTestId("ads-block-reported")).toContainText(/6/); // purchases 3 x 2
  await expect(page.getByTestId("ads-block-delivery")).toContainText(/36[.,]9|37/); // spend 3 x 12.30
  expect(await page.getByText(/ROAS|return on ad spend/i).count()).toBe(0);
  await expect(page.getByTestId("ads-budget-note")).toHaveText(en.budgetNote);
  await noSecrets(page);
});

test("MA09a three locales, desktop 1586x992 and 390 px: no horizontal scroll, screenshots hashed", async ({ page }) => {
  await signedLogin(page);
  for (const locale of ["zh-TW", "zh-CN", "en"] as const) {
    for (const viewport of [{ name: "desktop", width: 1586, height: 992 }, { name: "mobile", width: 390, height: 844 }] as const) {
      await page.setViewportSize({ width: viewport.width, height: viewport.height });
      await page.goto(`/${locale}/ads?store=${store}&draft=${firstDraft}`);
      await expect(page.getByRole("heading", { level: 1 })).toHaveText(adsCopy[locale].title);
      await expect(page.getByTestId("ads-connection")).toBeVisible();
      await shot(page, "ads-connection", locale, viewport.name);
      await page.getByTestId("ads-detail").scrollIntoViewIfNeeded();
      await expect(page.getByTestId("ads-detail")).toHaveAttribute("data-status", "PAUSED");
      await expect(page.getByTestId(`ads-draft-${firstDraft}`).locator("[data-state]")).toHaveText(adsCopy[locale].statuses.PAUSED);
      await shot(page, "ads-detail", locale, viewport.name);
      await page.getByTestId("ads-report").scrollIntoViewIfNeeded();
      await page.getByTestId("ads-report-load").click();
      await expect(page.getByTestId("ads-block-delivery")).toBeVisible();
      await shot(page, "ads-report", locale, viewport.name);
    }
  }
});

});

// The two gates below are independent of the serial flow (a failure here must not hide the other): each proves one frozen
// behaviour that the merchant can only see through the UI.
test.describe("MA09a frozen responses reach the merchant", () => {
  test.describe.configure({ mode: "default" });

  test("MA09a a refused command reaches the merchant with its frozen code (over_allowance at approve)", async ({ page }) => {
  await signedLogin(page);
  await openAds(page);
  await page.getByTestId("ads-new-draft").click();
  await page.getByTestId("ads-f-source").fill(`${pageAsset}_9876543210`);
  await page.getByTestId("ads-f-budget").fill("20000"); // NT$20,000 > the NT$10,000 allowance
  await page.getByTestId("ads-f-starts").fill(localInput(5 * 3600_000));
  await page.getByTestId("ads-f-ends").fill(localInput(29 * 3600_000));
  await page.getByTestId("ads-f-save").click();
  await expect(page.getByTestId("ads-banner")).toHaveText(en.saved);
  let status = 0;
  page.on("response", (r) => { if (/\/approve$/.test(new URL(r.url()).pathname)) status = r.status(); });
  await page.getByTestId("ads-approve").click();
  await expect(page.getByTestId("ads-banner")).toBeVisible();
  expect(status).toBe(409);
  // §7: 409 over_allowance. The UI keys its copy on the code; a code rewritten to a generic one shows the retry copy instead.
  await expect(page.getByTestId("ads-banner")).toHaveText(en.errors.over_allowance);
});

  test("MA09a a report window without any insights still renders the three blocks (zeros, not fetched), not an error", async ({ page }) => {
    await signedLogin(page);
    await openAds(page);
    await page.getByTestId("ads-report-from").fill("2026-01-01");
    await page.getByTestId("ads-report-to").fill("2026-01-07");
    await page.getByTestId("ads-report-load").click();
    // §7 / §15.5: three separate blocks with window, timezone and fetched_at, even before Meta delivered anything
    await expect(page.getByTestId("ads-block-orders")).toBeVisible();
    await expect(page.getByTestId("ads-block-delivery")).toBeVisible();
    await expect(page.getByTestId("ads-block-reported")).toBeVisible();
    await expect(page.getByTestId("ads-report")).not.toContainText(en.unavailable);
  });
});
