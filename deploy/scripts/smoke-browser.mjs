// File: deploy/scripts/smoke-browser.mjs
// Purpose: smoke case S34 (optional): drive real Chromium THROUGH THE CADDY EDGE of a running
//   smoke stack (not the dev servers): the storefront neutral return page must render and the
//   merchant admin "/" must follow its locale redirect on the public host to a 200 page without
//   console errors. Screenshots go to the evidence dir.
//   TLS validity is covered by S17 (curl + Caddy local root), so ignoreHTTPSErrors is used here.
// Runs as/in: developer host, invoked by deploy/scripts/smoke.sh full (after S17-S24).
// Reads env: SMOKE_HTTPS_PORT (edge port, default 443), SMOKE_EVIDENCE_DIR (screenshots/output).
// Reads secrets: none. Uses only *.localhost names (Chromium resolves them to loopback).
// Used by: smoke.sh S34. Depends on: the repo's pinned @playwright/test (package.json devDependency
//   1.63.0) and its Chromium build (`pnpm exec playwright install chromium`); no new dependency.
// Exit: 0 pass, 1 fail, 3 NOT_RUN (Chromium not installed).
// Status: DESIGN; NOT_RUN until the full smoke can run (blocked by cmd/migrate, I1). With the I1
//   proposal applied it FAILED on the admin redirect (F2, fixed in compose.yml/admin.Dockerfile).
// Change rules: this is deployment proof only; business browser gates stay in
//   scripts/dev/test-local.sh --browser-* (app proof).
import { mkdir } from "node:fs/promises";
import path from "node:path";

let chromium;
try {
  ({ chromium } = await import("@playwright/test"));
} catch {
  console.error("S34: @playwright/test not importable");
  process.exit(3);
}
const port = process.env.SMOKE_HTTPS_PORT ?? "443";
if (!/^\d{1,5}$/.test(port)) {
  console.error("S34: invalid SMOKE_HTTPS_PORT");
  process.exit(1);
}
const suffix = port === "443" ? "" : `:${port}`;
const outDir = path.join(process.env.SMOKE_EVIDENCE_DIR ?? ".", "browser");
await mkdir(outDir, { recursive: true });

let browser;
try {
  browser = await chromium.launch();
} catch {
  console.error("S34: Chromium is not installed for the pinned Playwright");
  process.exit(3);
}
const failures = [];
try {
  const context = await browser.newContext({ ignoreHTTPSErrors: true });
  const page = await context.newPage();
  const consoleErrors = [];
  page.on("console", (msg) => {
    if (msg.type() === "error") consoleErrors.push(msg.text().slice(0, 200));
  });
  page.on("pageerror", (err) => consoleErrors.push(String(err).slice(0, 200)));

  const ret = await page.goto(`https://shop.localhost${suffix}/payment/return`);
  if (ret?.status() !== 200) failures.push(`storefront status ${ret?.status()}`);
  if (!(await page.getByTestId("payment-return").isVisible())) failures.push("payment-return not visible");
  await page.screenshot({ path: path.join(outDir, "storefront-payment-return.png"), fullPage: true });

  // Admin "/" answers a locale redirect. The browser must follow it on the PUBLIC host and land on
  // a 200 page: a redirect to the internal listener (https://localhost:3100/..., VERIFIED_LOCAL
  // F2) used to throw ERR_CONNECTION_REFUSED here, and "status < 500" alone never caught it.
  consoleErrors.length = 0;
  const adminHost = `admin.localhost${suffix}`;
  let admin = null;
  try {
    admin = await page.goto(`https://${adminHost}/`);
  } catch (err) {
    failures.push(`admin navigation failed: ${String(err).split("\n")[0].slice(0, 200)}`);
  }
  if (admin) {
    const landed = new URL(page.url());
    if (admin.status() !== 200) failures.push(`admin status ${admin.status()}`);
    if (landed.host !== adminHost) failures.push(`admin redirect left the public host: ${landed.host}`);
    await page.waitForLoadState("networkidle");
    await page.screenshot({ path: path.join(outDir, "admin-root.png"), fullPage: true });
    if (consoleErrors.length) failures.push(`admin console errors: ${consoleErrors.length}`);
  }
} finally {
  await browser.close();
}
if (failures.length) {
  console.error(`S34 FAIL: ${failures.join("; ")}`);
  process.exit(1);
}
console.log("S34 PASS: storefront return page and admin root rendered through the edge");
