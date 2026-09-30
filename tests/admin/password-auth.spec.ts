// PA11 (contracts/merchant-password-auth-v1.md §7.2, §7.3, §9 PA11) — Chromium against the packaged Next admin
// BFF, an in-process Go api and real PG, started by tests/foundation/browser_password_auth_test.go
// (`bash scripts/dev/test-local.sh --browser-password-auth`). Password login is on, OIDC is NOT configured.
// The mailbox is the loopback SMTP fake; this spec reads emailed codes from the test binary's inspection
// endpoint (LC_BROWSER_INSPECT_ORIGIN, /mail?to=...), which exists only in that binary on loopback.
// Written from the contract, not from PasswordAuth.tsx: fields are found by the attributes §7.3 prescribes
// (type=email / type=password / inputmode=numeric autocomplete=one-time-code maxlength=6), buttons by form
// position, and copy is never matched literally except the pre-existing onboarding labels (zh-TW) that
// tests/admin/auth-real.spec.ts already relies on.
// Evidence written to LC_BROWSER_EVIDENCE_DIR: screens/*.png, screenshots.jsonl (name, locale, viewport, sha256),
// canaries.txt (every password, emailed code and address used; the Go side scans server logs for them).
import { createHash, randomBytes } from "node:crypto";
import { appendFileSync, mkdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import {
  expect,
  request as playwrightRequest,
  test,
  type APIRequestContext,
  type BrowserContext,
  type Page,
} from "@playwright/test";

function requiredOrigin(name: string) {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required`);
  const url = new URL(value);
  if (url.origin !== value || (url.protocol !== "http:" && url.protocol !== "https:"))
    throw new Error(`${name} must be an exact HTTP(S) origin`);
  return url.origin;
}
const publicOrigin = requiredOrigin("LC_BROWSER_PUBLIC_ORIGIN");
const inspectOrigin = requiredOrigin("LC_BROWSER_INSPECT_ORIGIN");
const evidenceDir = process.env.LC_BROWSER_EVIDENCE_DIR;
if (!evidenceDir) throw new Error("LC_BROWSER_EVIDENCE_DIR is required");
mkdirSync(join(evidenceDir, "screens"), { recursive: true });

test.use({ baseURL: publicOrigin, trace: "off", screenshot: "only-on-failure" });
test.describe.configure({ mode: "serial", timeout: 150_000 });

const locales = ["zh-CN", "zh-TW", "en"] as const;
type Locale = (typeof locales)[number];
const viewports = [
  { name: "desktop", width: 1586, height: 992 },
  { name: "mobile", width: 390, height: 844 },
] as const;
const challengeName = "__Host-commerce_challenge";
const sessionName = "__Host-commerce_session";
const csrfName = "__Host-commerce_csrf";

// ---- sentinels ---------------------------------------------------------------------------------
function remember(value: string) {
  appendFileSync(join(evidenceDir!, "canaries.txt"), value + "\n");
  return value;
}
const letters = (n: number) =>
  Array.from(randomBytes(n), (b: number) => String.fromCharCode(97 + (b % 26))).join("");
const newEmail = () => remember(`pwa.${letters(14)}@example.test`);
const newPassword = () => remember(`Pwa-${letters(22)}`);
// One source address per test flow, like Caddy's X-Forwarded-For; unique so per-source throttles never collide.
const source = () => `10.${randomBytes(1)[0]}.${randomBytes(1)[0]}.${1 + (randomBytes(1)[0] % 250)}`;

// ---- mailbox (inspection endpoint) ----------------------------------------------------------------
type Mail = { subject: string; text: string; html: string };
async function mails(to: string): Promise<Mail[]> {
  const res = await fetch(`${inspectOrigin}/mail?to=${encodeURIComponent(to.toLowerCase())}`);
  return (await res.json()) as Mail[];
}
async function nthMail(to: string, n: number): Promise<Mail> {
  const deadline = Date.now() + 20_000;
  for (;;) {
    const all = await mails(to);
    if (all.length >= n) return all[n - 1];
    if (Date.now() > deadline) throw new Error(`mail ${n} did not arrive`);
    await new Promise((r) => setTimeout(r, 100));
  }
}
const codeOf = (mail: Mail) => {
  const m = /(?:^|[^0-9A-Za-z])(\d{6})(?:[^0-9A-Za-z]|$)/.exec(mail.text);
  if (!m) throw new Error("no 6-digit code in the mail");
  return remember(m[1]);
};
const nthCode = async (to: string, n: number) => codeOf(await nthMail(to, n));
const wrongCodeFor = (code: string) => (code === "000000" ? "000001" : "000000");

// ---- page helpers --------------------------------------------------------------------------------
const emailInput = (page: Page) => page.locator('input[type="email"]');
const passwordInput = (page: Page) => page.locator('input[type="password"]');
const codeInput = (page: Page) => page.locator('input[autocomplete="one-time-code"]');
// Next injects an empty role=alert route announcer into every page; only alerts that carry text are messages.
const alert = (page: Page) => page.getByRole("alert").filter({ hasText: /\S/ });
const submitButton = (page: Page) => page.locator('form button[type="submit"]');

async function useSource(context: BrowserContext, ip: string) {
  // Emulates Caddy: the BFF takes the client address only from a single X-Forwarded-For value.
  await context.setExtraHTTPHeaders({ "x-forwarded-for": ip });
}

async function authCookies(context: BrowserContext) {
  const host = new URL(publicOrigin).hostname;
  return (await context.cookies()).filter((c) => c.domain === host);
}
async function cookieValue(context: BrowserContext, name: string) {
  return (await authCookies(context)).find((c) => c.name === name)?.value;
}

async function browserJSON(page: Page, path: string, options: { method?: string; body?: unknown; csrf?: boolean } = {}) {
  return page.evaluate(
    async ({ path, options, csrfName }) => {
      const headers = new Headers();
      if (options.body !== undefined) headers.set("Content-Type", "application/json");
      if (options.csrf) {
        const values = document.cookie.split(";").map((p) => p.trim()).filter((p) => p.startsWith(`${csrfName}=`)).map((p) => p.slice(csrfName.length + 1));
        if (values.length !== 1) throw new Error("exact CSRF cookie is unavailable");
        headers.set("X-CSRF-Token", values[0]);
      }
      const response = await fetch(path, { method: options.method ?? "GET", headers, body: options.body === undefined ? undefined : JSON.stringify(options.body), credentials: "same-origin" });
      const text = await response.text();
      return { status: response.status, body: text ? JSON.parse(text) : null };
    },
    { path, options, csrfName },
  );
}

async function shot(page: Page, name: string, locale: string, viewport: string) {
  const path = join(evidenceDir!, "screens", `${name}-${locale}-${viewport}.png`);
  const png = await page.screenshot({ path, fullPage: false });
  const sha256 = createHash("sha256").update(png).digest("hex");
  appendFileSync(join(evidenceDir!, "screenshots.jsonl"), JSON.stringify({ name, locale, viewport, sha256 }) + "\n");
}

async function noHorizontalScroll(page: Page) {
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  expect(overflow, "no horizontal page scroll").toBeLessThanOrEqual(0);
}

async function fillCredentials(page: Page, email: string, password?: string) {
  await emailInput(page).fill(email);
  if (password !== undefined) await passwordInput(page).fill(password);
}

// Submits the code step, waits for the BFF's answer and for the page navigation that follows a 200
// (the URL may be the same before and after, so the code input disappearing is the signal).
async function submitCode(page: Page, expectStatus = 200) {
  const [response] = await Promise.all([
    page.waitForResponse((r) => new URL(r.url()).pathname === "/api/auth/password/verify"),
    submitButton(page).click(),
  ]);
  expect(response.status()).toBe(expectStatus);
  if (expectStatus === 200) await expect(codeInput(page)).toHaveCount(0);
  return response;
}

// Sign-up up to the code step.
async function startSignup(page: Page, locale: string, email: string, password: string) {
  await page.goto(`/${locale}/signup`);
  await fillCredentials(page, email, password);
  await submitButton(page).click();
  await expect(codeInput(page)).toBeVisible();
}

// ---------------------------------------------------------------------------------------------------
test("full chain: sign-up -> code -> onboarding -> logout -> sign-in -> code -> workspace; forgot -> reset -> signed in, old session revoked (zh-TW, desktop)", async ({ browser, page, context }) => {
  test.setTimeout(150_000);
  const locale: Locale = "zh-TW";
  await page.setViewportSize({ width: 1586, height: 992 });
  await useSource(context, source());
  const email = newEmail();
  const password = newPassword();

  // sign-up
  await startSignup(page, locale, email, password);
  const challenge = (await authCookies(context)).find((c) => c.name === challengeName);
  expect(challenge, "challenge cookie set after step 1").toBeTruthy();
  expect({ httpOnly: challenge!.httpOnly, secure: challenge!.secure, sameSite: challenge!.sameSite, path: challenge!.path }).toEqual({ httpOnly: true, secure: true, sameSite: "Lax", path: "/" });
  expect(challenge!.expires - Date.now() / 1000, "challenge cookie lives at most 10 minutes").toBeLessThanOrEqual(600 + 5);
  expect(challenge!.value).toMatch(/^[A-Za-z0-9_-]{43}\.signup\.zh-TW$/);
  expect(challenge!.value).not.toContain(email);
  await expect(page.locator("body")).not.toContainText(email); // only a masked address is shown
  await expect(page.locator("body")).not.toContainText(password);
  const code = await nthCode(email, 1);
  // wrong code first: the error shows, the challenge cookie is KEPT (401 never clears it), the step stays
  await codeInput(page).fill(wrongCodeFor(code));
  await submitButton(page).click();
  await expect(alert(page)).toBeVisible();
  await expect(codeInput(page)).toBeVisible();
  expect(await cookieValue(context, challengeName), "a 401 keeps the challenge cookie").toBe(challenge!.value);
  await codeInput(page).fill(code);
  await submitCode(page);
  await expect(page).toHaveURL(/\/zh-TW\/?$/);
  expect(await cookieValue(context, challengeName), "a 200 clears the challenge cookie").toBeUndefined();
  const cookies = await authCookies(context);
  const meta = (name: string) => cookies.filter((c) => c.name === name).map(({ httpOnly, secure, sameSite, path }) => ({ httpOnly, secure, sameSite, path }));
  expect(meta(sessionName)).toEqual([{ httpOnly: true, secure: true, sameSite: "Lax", path: "/" }]);
  expect(meta(csrfName)).toEqual([{ httpOnly: false, secure: true, sameSite: "Lax", path: "/" }]);
  const firstSession = await cookieValue(context, sessionName);

  // onboarding (existing UI, unchanged by password auth)
  expect(await browserJSON(page, "/api/stores")).toEqual({ status: 200, body: { items: [] } });
  await page.getByLabel("商戶名稱").fill("Browser Merchant");
  await page.getByRole("button", { name: "下一步：商店設定" }).click();
  await page.getByLabel("商店名稱").fill("Browser Store");
  await page.getByLabel("交易幣別").selectOption("TWD");
  await page.getByRole("button", { name: "下一步：庫存倉" }).click();
  await page.getByLabel("初始庫存倉名稱").fill("Browser Warehouse");
  const created = page.waitForResponse((r) => new URL(r.url()).pathname === "/api/onboarding/initial-store");
  await page.getByRole("button", { name: "建立內部工作區" }).click();
  expect((await created).status()).toBe(200);
  await page.getByRole("button", { name: "進入工作區" }).click();
  await expect(page.getByRole("heading", { name: "商品與庫存" })).toBeVisible();

  // logout
  expect((await browserJSON(page, "/api/auth/logout", { method: "POST", body: {}, csrf: true })).status).toBe(204);
  await page.goto(`/${locale}/`);
  await expect(emailInput(page)).toBeVisible();
  await expect(passwordInput(page)).toBeVisible();

  // sign-in: wrong password shows the same error for a known and an unknown address (no enumeration in the UI)
  await fillCredentials(page, email, newPassword());
  await submitButton(page).click();
  await expect(alert(page)).toBeVisible();
  const knownError = await alert(page).innerText();
  expect(knownError.trim().length, "the sign-in failure message is not empty").toBeGreaterThan(0);
  await page.goto(`/${locale}/`);
  await fillCredentials(page, newEmail(), newPassword());
  await submitButton(page).click();
  await expect(alert(page)).toBeVisible();
  expect(await alert(page).innerText(), "unknown address and wrong password read the same").toBe(knownError);
  // right password -> code step -> workspace
  await page.goto(`/${locale}/`);
  await fillCredentials(page, email, password);
  await submitButton(page).click();
  await expect(codeInput(page)).toBeVisible();
  const loginCode = await nthCode(email, 2);
  await codeInput(page).fill(loginCode);
  await submitCode(page);
  const stores = await browserJSON(page, "/api/stores");
  expect(stores.status).toBe(200);
  expect((stores.body as { items: { name: string }[] }).items.map((s) => s.name)).toEqual(["Browser Store"]);
  const loginSession = await cookieValue(context, sessionName);
  expect(loginSession).toBeTruthy();
  expect(loginSession).not.toBe(firstSession);

  // forgot -> reset -> signed in, from a second, signed-out browser; the first browser's session is revoked
  const newPw = newPassword();
  const other = await browser.newContext({ ignoreHTTPSErrors: true, baseURL: publicOrigin, viewport: { width: 1586, height: 992 } });
  await useSource(other, source());
  const page2 = await other.newPage();
  await page2.goto(`/${locale}/reset`);
  await emailInput(page2).fill(email);
  await expect(passwordInput(page2)).toHaveCount(0); // step 1 of reset asks for the address only
  await submitButton(page2).click();
  await expect(codeInput(page2)).toBeVisible();
  await expect(page2.locator("body")).not.toContainText(email);
  const resetCode = await nthCode(email, 3);
  await codeInput(page2).fill(resetCode);
  await passwordInput(page2).fill(newPw);
  expect(await passwordInput(page2).getAttribute("autocomplete")).toBe("new-password");
  await submitCode(page2);
  const resetSession = await cookieValue(other, sessionName);
  expect(resetSession).toBeTruthy();
  expect(resetSession).not.toBe(loginSession);
  expect((await browserJSON(page2, "/api/stores")).status).toBe(200);
  // every other merchant session of the principal died with the reset (PD10), including the first browser's
  expect((await browserJSON(page, "/api/stores")).status, "the first browser's session was revoked by the reset").toBe(401);
  for (const [name, token, want] of [["old login session", loginSession, 401], ["first session", firstSession, 401], ["reset session", resetSession, 200]] as const) {
    const probe = await playwrightRequest.newContext({ ignoreHTTPSErrors: true, baseURL: publicOrigin, extraHTTPHeaders: { cookie: `${sessionName}=${token}` } });
    expect((await probe.get("/api/stores")).status(), name).toBe(want);
    await probe.dispose();
  }
  await other.close();
  // the old password no longer works, the new one does (BFF level)
  const anon = await playwrightRequest.newContext({ ignoreHTTPSErrors: true, baseURL: publicOrigin, extraHTTPHeaders: { origin: publicOrigin, "x-forwarded-for": source() } });
  expect((await anon.post("/api/auth/password/login", { data: { email, password, locale: "en" } })).status()).toBe(401);
  // The new password passes the password check: 202 (code mailed) or, when the login mail bucket is still inside its
  // 60 s window from the earlier sign-in, 429 - which is only reachable AFTER a correct password (a wrong one is 401).
  expect([202, 429]).toContain((await anon.post("/api/auth/password/login", { data: { email, password: newPw, locale: "en" } })).status());
  await anon.dispose();
});

// ---------------------------------------------------------------------------------------------------
test("BFF contract: Origin, query, client IP, strict keys, cookie clearing on 200/409 only, sessions survive a failed verify, no secret in any body", async () => {
  const bodies: string[] = [];
  const api = async (ctx: APIRequestContext, path: string, init: { data?: unknown; headers?: Record<string, string>; method?: string } = {}) => {
    const res = await (init.method === "GET" ? ctx.get(path, { headers: init.headers }) : ctx.post(path, { data: init.data, headers: init.headers }));
    const text = await res.text();
    bodies.push(text);
    return { res, text, json: () => (text ? JSON.parse(text) : null), setCookies: res.headersArray().filter((h) => h.name.toLowerCase() === "set-cookie").map((h) => h.value) };
  };
  const ok = (ip = source()) => ({ origin: publicOrigin, "x-forwarded-for": ip });
  const ctx = await playwrightRequest.newContext({ ignoreHTTPSErrors: true, baseURL: publicOrigin });
  const email = newEmail();
  const password = newPassword();
  const step1 = { email, password, locale: "en" };

  // methods, Origin, query
  for (const route of ["signup", "login", "reset", "verify"]) expect((await api(ctx, `/api/auth/password/${route}`, { method: "GET" })).res.status(), `GET ${route}`).toBe(405);
  expect([400, 401, 403]).toContain((await api(ctx, "/api/auth/password/signup", { data: step1, headers: { "x-forwarded-for": source() } })).res.status()); // no Origin
  expect([400, 401, 403]).toContain((await api(ctx, "/api/auth/password/signup", { data: step1, headers: { origin: "https://evil.example.test", "x-forwarded-for": source() } })).res.status());
  expect([400, 401, 403]).toContain((await api(ctx, "/api/auth/password/login", { data: step1, headers: { origin: publicOrigin + "/", "x-forwarded-for": source() } })).res.status());
  expect([400, 401, 403]).toContain((await api(ctx, "/api/auth/password/reset?x=1", { data: { email, locale: "en" }, headers: ok() })).res.status());
  // client IP: missing => 503, list / invalid => 400
  // A request with no X-Forwarded-For at all: Caddy always sets the header in production, and the packaged Next
  // server fills it from the socket when it is absent, so the route sees one value and answers 202 here; the
  // BFF's own 503 for a missing header is proven on the pure function in PA10 (clientIPFromHeaders([]) -> 503).
  expect([202, 503]).toContain((await api(ctx, "/api/auth/password/signup", { data: { ...step1, email: newEmail() }, headers: { origin: publicOrigin } })).res.status());
  expect((await api(ctx, "/api/auth/password/signup", { data: step1, headers: { origin: publicOrigin, "x-forwarded-for": "10.1.1.1, 10.2.2.2" } })).res.status()).toBe(400);
  expect((await api(ctx, "/api/auth/password/signup", { data: step1, headers: { origin: publicOrigin, "x-forwarded-for": "not-an-ip" } })).res.status()).toBe(400);
  // strict keys
  for (const data of [{ ...step1, tenant_id: "t" }, { email, locale: "en" }, { ...step1, locale: "fr" }, { email, password }, []]) {
    const r = await api(ctx, "/api/auth/password/signup", { data, headers: ok() });
    expect([400, 422], JSON.stringify(Object.keys(data as object))).toContain(r.res.status());
  }
  expect([400, 422]).toContain((await api(ctx, "/api/auth/password/reset", { data: { email, locale: "en", password }, headers: ok() })).res.status());
  // no challenge was created by any rejected call above
  expect((await mails(email)).length).toBe(0);

  // sign-up succeeds: 202 {step, expires_at} exactly; the binding is only in the cookie
  const signupIP = source();
  const s1 = await api(ctx, "/api/auth/password/signup", { data: step1, headers: ok(signupIP) });
  expect(s1.res.status()).toBe(202);
  expect(Object.keys(s1.json()).sort()).toEqual(["expires_at", "step"]);
  expect(s1.json().step).toBe("code");
  const challengeSet = s1.setCookies.find((c) => c.startsWith(`${challengeName}=`))!;
  expect(challengeSet).toBeTruthy();
  const parts = challengeSet.split(";").map((p) => p.trim());
  for (const flag of ["Secure", "HttpOnly", "SameSite=Lax", "Path=/"]) expect(parts, flag).toContain(flag);
  expect(challengeSet.toLowerCase()).not.toContain("domain=");
  expect(Number(/Max-Age=(\d+)/.exec(challengeSet)?.[1])).toBeLessThanOrEqual(600);
  const challengeValue = parts[0].slice(challengeName.length + 1);
  expect(challengeValue).toMatch(/^[A-Za-z0-9_-]{43}\.signup\.en$/);
  expect(s1.text).not.toContain(challengeValue.split(".")[0]);
  expect(s1.setCookies.some((c) => c.startsWith(`${sessionName}=`))).toBe(false);
  const code = await nthCode(email, 1);
  const cookieHeader = `${challengeName}=${challengeValue}`;

  // verify: no cookie / duplicated cookie / wrong code all keep everything; a 401 never clears the challenge cookie
  // "no cookie" must mean no cookie on the wire: `ctx`'s jar already holds the Secure challenge cookie from sign-up and sends it over an
  // https origin (WebKit gate, browserFront) though not over http://127.0.0.1, so the no-cookie probe uses its own empty context.
  const bare = await playwrightRequest.newContext({ ignoreHTTPSErrors: true, baseURL: publicOrigin });
  expect([400, 401, 403, 422]).toContain((await api(bare, "/api/auth/password/verify", { data: { code }, headers: ok() })).res.status());
  await bare.dispose();
  const dup = await api(ctx, "/api/auth/password/verify", { data: { code }, headers: { ...ok(), cookie: `${cookieHeader}; ${cookieHeader}` } });
  expect(dup.res.status()).toBeGreaterThanOrEqual(400);
  expect(dup.setCookies.some((c) => c.startsWith(`${sessionName}=`))).toBe(false);
  const wrong = await api(ctx, "/api/auth/password/verify", { data: { code: wrongCodeFor(code) }, headers: { ...ok(), cookie: cookieHeader } });
  expect(wrong.res.status()).toBe(401);
  expect(wrong.json().code).toBe("invalid_code");
  expect(wrong.setCookies.filter((c) => c.startsWith(`${challengeName}=`) && /Max-Age=0|Expires=Thu, 01 Jan 1970/i.test(c)), "401 must not clear the challenge cookie").toEqual([]);
  expect(wrong.text).not.toContain(challengeValue.split(".")[0]);
  // strict verify keys, purpose never taken from the body
  expect([400, 422]).toContain((await api(ctx, "/api/auth/password/verify", { data: { code, purpose: "reset" }, headers: { ...ok(), cookie: cookieHeader } })).res.status());
  expect([400, 422]).toContain((await api(ctx, "/api/auth/password/verify", { data: { code, new_password: newPassword() }, headers: { ...ok(), cookie: cookieHeader } })).res.status());
  // 200: cleared challenge, session + CSRF set with the existing flags, redirect to the locale root
  const good = await api(ctx, "/api/auth/password/verify", { data: { code }, headers: { ...ok(), cookie: cookieHeader } });
  expect(good.res.status()).toBe(200);
  expect(good.json()).toEqual({ redirect: "/en/" });
  expect(good.setCookies.filter((c) => c.startsWith(`${challengeName}=`) && /Max-Age=0/i.test(c)).length, "200 clears the challenge cookie").toBe(1);
  const sessionSet = good.setCookies.find((c) => c.startsWith(`${sessionName}=`))!;
  const csrfSet = good.setCookies.find((c) => c.startsWith(`${csrfName}=`))!;
  expect(sessionSet).toMatch(/Secure/);
  expect(sessionSet).toMatch(/HttpOnly/);
  expect(sessionSet).toMatch(/SameSite=Lax/);
  expect(csrfSet).toMatch(/Secure/);
  expect(csrfSet).not.toMatch(/HttpOnly/);
  const session = sessionSet.split(";")[0].slice(sessionName.length + 1);
  remember(session);

  // a failed verify never destroys a valid existing session
  const loginIP = source();
  const l1 = await api(ctx, "/api/auth/password/login", { data: step1, headers: ok(loginIP) });
  expect(l1.res.status()).toBe(202);
  const lChallenge = l1.setCookies.find((c) => c.startsWith(`${challengeName}=`))!.split(";")[0];
  const lCode = await nthCode(email, 2);
  const withSession = `${sessionName}=${session}; ${lChallenge}`;
  const lWrong = await api(ctx, "/api/auth/password/verify", { data: { code: wrongCodeFor(lCode) }, headers: { ...ok(loginIP), cookie: withSession } });
  expect(lWrong.res.status()).toBe(401);
  expect(lWrong.setCookies.some((c) => c.startsWith(`${sessionName}=`) && /Max-Age=0/i.test(c)), "a failed verify must not clear the session").toBe(false);
  const probe = await playwrightRequest.newContext({ ignoreHTTPSErrors: true, baseURL: publicOrigin, extraHTTPHeaders: { cookie: `${sessionName}=${session}` } });
  expect((await probe.get("/api/stores")).status()).toBe(200);
  await probe.dispose();

  // 409: two sources hold sign-up challenges for one address; the second complete conflicts and clears
  const race = newEmail();
  const racePw = newPassword();
  const a = await api(ctx, "/api/auth/password/signup", { data: { email: race, password: racePw, locale: "en" }, headers: ok() });
  const b = await api(ctx, "/api/auth/password/signup", { data: { email: race, password: racePw, locale: "en" }, headers: ok() });
  expect([a.res.status(), b.res.status()]).toEqual([202, 202]);
  const cA = a.setCookies.find((c) => c.startsWith(`${challengeName}=`))!.split(";")[0];
  const cB = b.setCookies.find((c) => c.startsWith(`${challengeName}=`))!.split(";")[0];
  const codes = [await nthCode(race, 1), await nthCode(race, 2)];
  const first = await api(ctx, "/api/auth/password/verify", { data: { code: codes[0] }, headers: { ...ok(), cookie: cA } });
  const second = await api(ctx, "/api/auth/password/verify", { data: { code: codes[1] }, headers: { ...ok(), cookie: cB } });
  expect(first.res.status()).toBe(200);
  expect(second.res.status()).toBe(409);
  expect(second.json().code).toBe("account_exists");
  expect(second.setCookies.filter((c) => c.startsWith(`${challengeName}=`) && /Max-Age=0/i.test(c)).length, "409 clears the challenge cookie").toBe(1);
  expect(second.setCookies.some((c) => c.startsWith(`${sessionName}=`)), "a 409 issues no session").toBe(false);

  // no response body ever echoed a password, an address or a code
  const all = bodies.join("\n");
  for (const secret of [password, racePw, email, race, code, lCode, codes[0], codes[1]]) expect(all, "a response body carries a secret").not.toContain(secret);
  await ctx.dispose();
});

// ---------------------------------------------------------------------------------------------------
// Before React hydrates, a form without method=post submits as GET and puts the email and password into the
// URL (history, Caddy access logs). Every server-rendered form holding a secret field must POST.
test("SSR: no form with a password field lacks method=post (/, /signup, /reset)", async () => {
  const ctx = await playwrightRequest.newContext({ ignoreHTTPSErrors: true, baseURL: publicOrigin });
  let seen = 0;
  for (const path of ["/zh-CN/", "/zh-CN/signup", "/zh-CN/reset", "/en/signup"]) {
    const res = await ctx.get(path);
    expect(res.status(), path).toBe(200);
    const forms = (await res.text()).match(/<form\b[^>]*>[\s\S]*?<\/form>/gi) ?? [];
    for (const form of forms) {
      if (!/name="(password|new_password)"/.test(form)) continue;
      seen++;
      expect(form, `${path}: <form> with a password field must be method=post`).toMatch(/^<form\b[^>]*\bmethod="post"/i);
    }
  }
  expect(seen, "at least one SSR password form must be found").toBeGreaterThan(0);
  await ctx.dispose();
});

test("throttled UI state and the 60 s resend cooldown", async ({ page, context }) => {
  test.setTimeout(150_000);
  await page.setViewportSize({ width: 1586, height: 992 });
  const ip = source();
  await useSource(context, ip);
  const victim = newEmail();
  // ordinary failure text first (a different source, so its bucket is untouched)
  const other = await context.newPage();
  await other.goto("/en/");
  await fillCredentials(other, newEmail(), newPassword());
  await submitButton(other).click();
  await expect(alert(other)).toBeVisible();
  const invalidText = await alert(other).innerText();
  expect(invalidText.trim().length).toBeGreaterThan(0);
  await other.close();
  // ten wrong checks for one (email, source) through the BFF, then the UI is throttled
  const api = await playwrightRequest.newContext({ ignoreHTTPSErrors: true, baseURL: publicOrigin, extraHTTPHeaders: { origin: publicOrigin, "x-forwarded-for": ip } });
  for (let i = 0; i < 10; i++) expect((await api.post("/api/auth/password/login", { data: { email: victim, password: newPassword(), locale: "en" } })).status()).toBe(401);
  const throttled = await api.post("/api/auth/password/login", { data: { email: victim, password: newPassword(), locale: "en" } });
  expect(throttled.status()).toBe(429);
  expect(Number(throttled.headers()["retry-after"])).toBeGreaterThan(0);
  await api.dispose();
  await page.goto("/en/");
  await fillCredentials(page, victim, newPassword());
  await submitButton(page).click();
  await expect(alert(page)).toBeVisible();
  const throttledText = await alert(page).innerText();
  expect(throttledText).not.toBe(invalidText);
  expect(throttledText, "the throttled message states minutes").toMatch(/\d/);
  await shot(page, "throttled", "en", "desktop");

  // resend cooldown: disabled right after step 1, enabled after 60 s, resend = a new code that supersedes the old one
  const email = newEmail();
  await useSource(context, source());
  await startSignup(page, "en", email, newPassword());
  const resend = page.locator('form button[type="button"]').first();
  await expect(resend).toBeDisabled();
  const first = await nthCode(email, 1);
  await page.waitForTimeout(61_500);
  await expect(resend).toBeEnabled();
  await resend.click();
  const second = await nthCode(email, 2);
  expect(second).not.toBe(first);
  await codeInput(page).fill(first);
  await submitButton(page).click();
  await expect(alert(page)).toBeVisible(); // the superseded code is refused
  await codeInput(page).fill(second);
  await submitCode(page);
  expect(await cookieValue(context, sessionName)).toBeTruthy();
});

// ---------------------------------------------------------------------------------------------------
const mailByLocale = new Map<string, Mail>();
for (const viewport of viewports) {
  for (const locale of locales) {
    test(`matrix ${locale} ${viewport.name}: forms, autocomplete attributes, code step, screenshots, mail language`, async ({ page, context }) => {
      await page.setViewportSize({ width: viewport.width, height: viewport.height });
      await useSource(context, source());
      // sign-in
      await page.goto(`/${locale}/`);
      await expect(emailInput(page)).toBeVisible();
      expect(await emailInput(page).getAttribute("autocomplete")).toBe("email");
      expect(await passwordInput(page).getAttribute("autocomplete")).toBe("current-password");
      const signinHeading = await page.getByRole("heading", { level: 1 }).innerText();
      await noHorizontalScroll(page);
      await shot(page, "signin", locale, viewport.name);
      // sign-up
      await page.goto(`/${locale}/signup`);
      expect(await emailInput(page).getAttribute("autocomplete")).toBe("email");
      expect(await passwordInput(page).getAttribute("autocomplete")).toBe("new-password");
      await noHorizontalScroll(page);
      await shot(page, "signup", locale, viewport.name);
      // reset (address only)
      await page.goto(`/${locale}/reset`);
      expect(await emailInput(page).getAttribute("autocomplete")).toBe("email");
      await expect(passwordInput(page)).toHaveCount(0);
      await noHorizontalScroll(page);
      await shot(page, "reset", locale, viewport.name);
      // the three modes read differently
      const resetHeading = await page.getByRole("heading", { level: 1 }).innerText();
      expect(resetHeading).not.toBe(signinHeading);
      // code step of a sign-up
      const email = newEmail();
      await startSignup(page, locale, email, newPassword());
      expect(await codeInput(page).getAttribute("inputmode")).toBe("numeric");
      expect(await codeInput(page).getAttribute("autocomplete")).toBe("one-time-code");
      expect(await codeInput(page).getAttribute("maxlength")).toBe("6");
      // sign-up wording never confirms existence and shows only a masked address
      await expect(page.locator("body")).not.toContainText(email);
      await noHorizontalScroll(page);
      await shot(page, "code", locale, viewport.name);
      // the code field takes digits only and at most six
      await codeInput(page).fill("12ab34567");
      expect(await codeInput(page).inputValue()).toMatch(/^\d{1,6}$/);
      // wrong code -> alert; the resend button is disabled during the cooldown
      const code = await nthCode(email, 1);
      await codeInput(page).fill(wrongCodeFor(code));
      await submitButton(page).click();
      await expect(alert(page)).toBeVisible();
      await expect(page.locator('form button[type="button"]').first()).toBeDisabled();
      // the emailed message follows the page locale
      const mail = await nthMail(email, 1);
      mailByLocale.set(`${locale}/${viewport.name}`, mail);
      const cjk = /\p{Script=Han}/u.test(mail.text);
      expect(cjk, `mail language for ${locale}`).toBe(locale !== "en");
      expect(mail.subject).not.toContain(code);
      expect(mail.text + mail.html).not.toMatch(/https?:\/\//i);
    });
  }
}

test("matrix: locale copy differs per locale and the reset wording is the same for known and unknown addresses", async ({ page, context }) => {
  // zh-CN, zh-TW and en mails are three different texts
  const texts = locales.map((l) => mailByLocale.get(`${l}/desktop`)?.text);
  expect(texts.every(Boolean)).toBe(true);
  expect(new Set(texts).size).toBe(3);
  // reset: a known and an unknown address produce the same visible code-step text (modulo the masked address)
  await page.setViewportSize({ width: 1586, height: 992 });
  const visible = async (email: string) => {
    await useSource(context, source());
    await page.context().clearCookies({ name: challengeName });
    await page.goto("/en/reset");
    await emailInput(page).fill(email);
    await submitButton(page).click();
    await expect(codeInput(page)).toBeVisible();
    const text = await page.locator("main, body").first().innerText();
    const masked = `${email[0]}***@${email.split("@")[1]}`;
    expect(text).toContain(masked);
    return text.replaceAll(masked, "<MASK>");
  };
  const known = newEmail();
  await (await playwrightRequest.newContext({ ignoreHTTPSErrors: true, baseURL: publicOrigin, extraHTTPHeaders: { origin: publicOrigin, "x-forwarded-for": source() } })).post("/api/auth/password/signup", { data: { email: known, password: newPassword(), locale: "en" } });
  await nthCode(known, 1);
  const a = await visible(known);
  const b = await visible(newEmail());
  expect(a, "reset step-1 wording must not depend on whether the account exists").toBe(b);
  // sanity: the recorded screenshots exist
  const shots = readFileSync(join(evidenceDir!, "screenshots.jsonl"), "utf8").trim().split("\n").map((l: string) => JSON.parse(l));
  expect(shots.length).toBeGreaterThanOrEqual(3 * 2 * 4 + 1);
  expect(new Set(shots.map((s: { sha256: string }) => s.sha256)).size).toBeGreaterThan(10);
});
