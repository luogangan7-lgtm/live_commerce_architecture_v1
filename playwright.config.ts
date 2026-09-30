import { defineConfig, devices } from "@playwright/test";

// These suites have deliberately different server/authority fixtures. Never
// silently run signed-identity tests against a ledger's shared dev bearer.
const suite = process.env.LC_BROWSER_SUITE ?? "ledger";
const suites: Record<string, string[]> = {
  ledger: ["ledger.spec.ts", "production.spec.ts", "visual-states.spec.ts"],
  "identity-mock": ["auth.spec.ts"],
  "entry-mock": ["entry.spec.ts"],
  "identity-real": ["auth-real.spec.ts"],
  // PA11 (merchant-password-auth-v1): started by tests/foundation/browser_password_auth_test.go.
  "password-auth": ["password-auth.spec.ts"],
  "settings-real": ["settings-real.spec.ts"],
  "merchant-orders-bff": ["orders-bff.spec.ts"],
  "merchant-orders-ui": ["orders-ui.spec.ts"],
  "studio-ui": ["studio-ui.spec.ts"],
  // KC16: admin + storefront Next, Go and PG are started by browser_live_claims_test.go.
  "live-claims": ["claims-ui.spec.ts"],
};
if (!Object.hasOwn(suites, suite)) throw new Error("Invalid LC_BROWSER_SUITE");

// Engine switch (admin pages): LC_BROWSER_ENGINE=webkit runs the same specs in Playwright WebKit with the Desktop Safari
// profile (scripts/dev/test-local.sh --browser-webkit); default chromium leaves every existing gate unchanged. The Go
// harness forwards the variable (browserEnvironment in tests/foundation/browser_identity_chain_test.go). Under webkit the admin is
// reached over a self-signed https front (browserFront in the same file: WebKit refuses `__Host-` cookies on http), hence ignoreHTTPSErrors.
const safari = process.env.LC_BROWSER_ENGINE === "webkit" ? { ...devices["Desktop Safari"], ignoreHTTPSErrors: true } : {};

export default defineConfig({
  testDir: "./tests/admin",
  testMatch: suites[suite],
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 30000,
  expect: { timeout: 10000 },
  reporter: [
    ["list"],
    ["json", { outputFile: "output/playwright/results.json" }],
  ],
  outputDir: "output/playwright/test-results",
  use: {
    ...safari,
    baseURL: "http://127.0.0.1:3100",
    headless: true,
    viewport: { width: 1586, height: 992 },
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
  },
});
