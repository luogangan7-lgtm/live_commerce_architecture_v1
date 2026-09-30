// PA10 (contracts/merchant-password-auth-v1.md §7.2, §5 "Admin BFF", §9 PA10; Node, no browser, no Next).
// Written from the contract and the auth-ui FROZEN TS interface (docs/delivery/units/auth-ui.md), not from
// the routes. It imports only the frozen exports:
//   apps/admin/lib/password-request.ts  CHALLENGE_COOKIE, clientIPFromHeaders, parseStep1, parseVerify,
//                                        challengeCookie, parseChallengeCookie, clearsChallenge
//   apps/admin/lib/auth.ts              readAuthConfig (+ the existing requireOrigin, hasNoQuery,
//                                        privateIdentity, setSessionCookies, safeError the routes are built from)
// and states the expectations from the contract: strict keys per purpose (§7.1), Origin rule (§7.2), the
// challenge cookie value/flags with Max-Age <= 600 (§7.2), cookie cleared on 200/409 only (§7.2), XFF
// missing => 503 / multiple or invalid => 400 (U2/PD14), the BFF-key call forwarding X-Commerce-Client-IP
// once and never retrying (§7.1), readAuthConfig valid without OIDC when the password flag is on and invalid
// without it (§5), session + CSRF cookies through the existing helper.
// auth.ts starts with `import "server-only"`, which throws in plain Node; the test registers a resolve hook
// (node:module registerHooks, no package added) that turns that one specifier into an empty module. The
// module reads process.env once at import, so the environment is set before the dynamic import.
// Route-level checks that need the Next runtime (the routes themselves, "password never in a response body or
// server log") are asserted by PA11's harness (tests/admin/password-auth.spec.ts + browser_password_auth_test.go).
// Run: node --test --experimental-strip-types tests/admin/password-bff.test.ts
import assert from "node:assert/strict";
import { randomBytes } from "node:crypto";
import { createServer } from "node:http";
import { registerHooks } from "node:module";
import type { AddressInfo } from "node:net";
import { test } from "node:test";

registerHooks({
  resolve(specifier, context, next) {
    if (specifier === "server-only")
      return { url: "data:text/javascript,", shortCircuit: true };
    return next(specifier, context);
  },
});

type Seen = { headers: Record<string, string | string[] | undefined>; body: string };
// phrase builds sample passphrases (14 chars) at runtime so no password-shaped literal sits in the source
// (secret scanners; PROCESS.md §6).
const phrase = (c: string): string => Array(5).fill(c + c).join("-");

const seen: Seen[] = [];
let hang = false;
const upstream = createServer((req, res) => {
  const chunks: Buffer[] = [];
  req.on("data", (c: Buffer) => chunks.push(c));
  req.on("end", () => {
    seen.push({ headers: { ...req.headers }, body: Buffer.concat(chunks).toString() });
    if (hang) return; // never answer: the BFF's own timeout must end the call
    res.writeHead(202, { "content-type": "application/json" });
    res.end('{"binding":"x","expires_at":"2099-01-01T00:00:00Z"}');
  });
});
await new Promise<void>((ok) => upstream.listen(0, "127.0.0.1", ok));
const apiOrigin = `http://127.0.0.1:${(upstream.address() as AddressInfo).port}`;

const bffKey = randomBytes(32).toString("base64url");
const publicOrigin = "https://admin.example.test";
const baseEnv: Record<string, string> = {
  COMMERCE_IDENTITY_ENABLED: "1",
  COMMERCE_PASSWORD_LOGIN_ENABLED: "1",
  COMMERCE_BFF_KEY: bffKey,
  COMMERCE_PUBLIC_ORIGIN: publicOrigin,
  COMMERCE_API_ORIGIN: apiOrigin,
};
// The module-level authConfig is computed from process.env at first import: password login on, NO OIDC.
Object.assign(process.env, baseEnv);
const auth = await import("../../apps/admin/lib/auth.ts");
const pr = await import("../../apps/admin/lib/password-request.ts");

test.after(() => upstream.close());

const binding = randomBytes(32).toString("base64url"); // 43 chars, like Go's A1 binding
const attrs = (setCookie: string) => setCookie.split(";").map((s) => s.trim());

test("PA10 client IP: missing => 503, multiple or invalid => 400, exactly one literal accepted", () => {
  assert.deepEqual(pr.clientIPFromHeaders([]), { status: 503 });
  assert.deepEqual(pr.clientIPFromHeaders(["203.0.113.9"]), { ip: "203.0.113.9" });
  assert.deepEqual(pr.clientIPFromHeaders(["2001:db8::1"]), { ip: "2001:db8::1" });
  assert.deepEqual(pr.clientIPFromHeaders(["::ffff:203.0.113.9"]), { ip: "::ffff:203.0.113.9" });
  for (const bad of [
    ["203.0.113.9", "203.0.113.10"], // two header values
    ["203.0.113.9, 203.0.113.10"], // a proxy list (Fetch joins duplicate headers with ", ")
    ["203.0.113.9:443"],
    ["[2001:db8::1]"],
    ["[2001:db8::1]:443"],
    ["fe80::1%eth0"],
    ["example.test"],
    [""],
    [" "],
    ["203.0.113"],
    ["203.0.113.256"],
    ["0x7f.1"],
  ])
    assert.deepEqual(pr.clientIPFromHeaders(bad), { status: 400 }, JSON.stringify(bad));
});

test("PA10 strict step-1 keys per purpose (§7.1)", () => {
  const ok = { email: "a@b.test", password: phrase("p"), locale: "en" as const };
  assert.deepEqual(pr.parseStep1("signup", ok), ok);
  assert.deepEqual(pr.parseStep1("login", ok), ok);
  assert.deepEqual(pr.parseStep1("reset", { email: "a@b.test", locale: "zh-TW" }), { email: "a@b.test", locale: "zh-TW" });
  for (const locale of ["zh-CN", "zh-TW", "en"] as const)
    assert.ok(pr.parseStep1("login", { ...ok, locale }));
  const rejects: [string, unknown][] = [
    ["signup", { ...ok, tenant_id: "t" }], // unknown key
    ["signup", { email: ok.email, locale: "en" }], // missing password
    ["signup", { email: ok.email, password: ok.password }], // missing locale
    ["signup", { password: ok.password, locale: "en" }], // missing email
    ["login", { ...ok, locale: "fr" }],
    ["login", { ...ok, locale: "EN" }],
    ["login", { ...ok, locale: "" }],
    ["reset", { ...ok }], // reset takes no password
    ["reset", { email: ok.email }],
    ["reset", { email: ok.email, locale: "en", extra: 1 }],
    ["signup", { ...ok, email: 5 }],
    ["signup", { ...ok, password: ["x"] }],
    ["signup", { ...ok, email: "" }],
    ["signup", { ...ok, password: "" }],
    ["signup", null],
    ["signup", []],
    ["signup", "string"],
    ["signup", 7],
    ["signup", undefined],
    ["other", ok], // unknown purpose
  ];
  for (const [purpose, body] of rejects)
    assert.equal(pr.parseStep1(purpose as never, body), null, `${purpose} ${JSON.stringify(body)}`);
  // Password and email are handed back untouched (Go normalizes; the BFF never trims or logs them).
  const padded = pr.parseStep1("login", { email: " A@B.test ", password: "  spaced  ", locale: "en" });
  assert.equal(padded?.password, "  spaced  ");
});

test("PA10 strict verify keys per purpose; the purpose is an argument, never the body", () => {
  assert.deepEqual(pr.parseVerify({ code: "123456" }, "signup"), { code: "123456" });
  assert.deepEqual(pr.parseVerify({ code: "123456" }, "login"), { code: "123456" });
  assert.deepEqual(pr.parseVerify({ code: "123456", new_password: phrase("n") }, "reset"), { code: "123456", new_password: phrase("n") });
  const rejects: [string, unknown][] = [
    ["reset", { code: "123456" }], // new_password is required for reset
    ["signup", { code: "123456", new_password: phrase("x") }], // and only for reset
    ["login", { code: "123456", new_password: phrase("x") }],
    ["signup", { code: "123456", purpose: "reset" }], // purpose is not a body field
    ["signup", { code: "123456", purpose: "signup" }],
    ["signup", {}],
    ["signup", { code: 123456 }],
    ["signup", { code: "" }],
    ["reset", { code: "123456", new_password: "" }],
    ["reset", { code: "123456", new_password: 5 }],
    ["signup", null],
    ["signup", []],
    ["nope", { code: "123456" }],
  ];
  for (const [purpose, body] of rejects)
    assert.equal(pr.parseVerify(body, purpose as never), null, `${purpose} ${JSON.stringify(body)}`);
});

test("PA10 challenge cookie: name, value, flags, Max-Age <= 600", () => {
  assert.equal(pr.CHALLENGE_COOKIE, "__Host-commerce_challenge");
  const now = new Date("2026-09-30T00:00:00Z");
  const inMs = (ms: number) => new Date(now.getTime() + ms).toISOString();
  const c = pr.challengeCookie(binding, "login", "zh-TW", inMs(600_000), now);
  const parts = attrs(c);
  assert.equal(parts[0], `__Host-commerce_challenge=${binding}.login.zh-TW`);
  for (const flag of ["Secure", "HttpOnly", "SameSite=Lax", "Path=/"])
    assert.ok(parts.includes(flag), `${flag} in ${c}`);
  assert.ok(!/domain=/i.test(c), "a __Host- cookie has no Domain");
  const maxAge = (s: string) => Number(/Max-Age=(\d+)/.exec(s)?.[1]);
  assert.equal(maxAge(c), 600);
  assert.equal(maxAge(pr.challengeCookie(binding, "signup", "en", inMs(3_600_000), now)), 600, "capped at 600 even for a longer expiry");
  assert.equal(maxAge(pr.challengeCookie(binding, "reset", "en", inMs(45_000), now)), 45, "never outlives the challenge");
  assert.equal(maxAge(pr.challengeCookie(binding, "reset", "en", inMs(1_500), now)), 1);
  for (const purpose of ["signup", "login", "reset"] as const)
    assert.ok(maxAge(pr.challengeCookie(binding, purpose, "en", inMs(9_999_999), now)) <= 600);
  // Unusable inputs never produce a dead cookie the route could set.
  assert.equal(pr.challengeCookie(binding, "login", "en", inMs(-1_000), now), "");
  assert.equal(pr.challengeCookie(binding, "login", "en", inMs(0), now), "");
  assert.equal(pr.challengeCookie(binding, "login", "en", "not-a-date", now), "");
  assert.equal(pr.challengeCookie("short", "login", "en", inMs(60_000), now), "");
  assert.equal(pr.challengeCookie(binding + "x", "login", "en", inMs(60_000), now), "");
  assert.equal(pr.challengeCookie(binding, "other" as never, "en", inMs(60_000), now), "");
  assert.equal(pr.challengeCookie(binding, "login", "fr", inMs(60_000), now), "");
});

test("PA10 challenge cookie parsing: exactly one well-formed cookie", () => {
  const value = `${binding}.reset.zh-CN`;
  assert.deepEqual(pr.parseChallengeCookie(`__Host-commerce_challenge=${value}`), { binding, purpose: "reset", locale: "zh-CN" });
  assert.deepEqual(pr.parseChallengeCookie(`a=b; __Host-commerce_challenge=${value}; c=d`), { binding, purpose: "reset", locale: "zh-CN" });
  assert.equal(pr.parseChallengeCookie(null), null);
  assert.equal(pr.parseChallengeCookie(""), null);
  assert.equal(pr.parseChallengeCookie("a=b"), null);
  assert.equal(pr.parseChallengeCookie(`__Host-commerce_challenge=${value}; __Host-commerce_challenge=${value}`), null, "two cookies of the name");
  assert.equal(pr.parseChallengeCookie(`__Host-commerce_challenge=${value}; __Host-commerce_challenge=${binding}.login.en`), null);
  assert.equal(pr.parseChallengeCookie(`__Host-commerce_challenge=${binding}.other.en`), null);
  assert.equal(pr.parseChallengeCookie(`__Host-commerce_challenge=${binding}.login.fr`), null);
  assert.equal(pr.parseChallengeCookie(`__Host-commerce_challenge=${binding.slice(1)}.login.en`), null);
  assert.equal(pr.parseChallengeCookie(`__Host-commerce_challenge=${binding}.login`), null);
  assert.equal(pr.parseChallengeCookie(`commerce_challenge=${value}`), null, "no __Host- prefix");
  assert.equal(pr.parseChallengeCookie(`x__Host-commerce_challenge=${value}`), null);
});

test("PA10 the challenge cookie is cleared on 200 and 409 only; every 401 keeps it", () => {
  const cleared = [200, 409];
  for (let status = 100; status < 600; status++)
    assert.equal(pr.clearsChallenge(status), cleared.includes(status), String(status));
});

test("PA10 readAuthConfig: valid without OIDC when the password flag is on, invalid without it", () => {
  const good = { ...baseEnv };
  const cfg = auth.readAuthConfig(good);
  assert.ok(cfg);
  assert.equal(cfg.issuer, null);
  assert.equal(cfg.passwordLogin, true);
  assert.equal(cfg.publicOrigin, publicOrigin);
  // an unset or empty issuer with the flag on is fine
  assert.equal(auth.readAuthConfig({ ...good, COMMERCE_OIDC_ISSUER: "" })?.issuer, null);
  // a configured issuer is kept
  assert.equal(auth.readAuthConfig({ ...good, COMMERCE_OIDC_ISSUER: "https://idp.example.test" })?.issuer, "https://idp.example.test");
  // a set-but-invalid issuer still fails startup (password flag on)
  assert.throws(() => auth.readAuthConfig({ ...good, COMMERCE_OIDC_ISSUER: "http://idp.example.test" }));
  // without the flag the issuer is required, as before
  for (const flag of [undefined, "", "0"]) {
    const env: Record<string, string | undefined> = { ...good, COMMERCE_PASSWORD_LOGIN_ENABLED: flag };
    assert.throws(() => auth.readAuthConfig(env), /COMMERCE_OIDC_ISSUER/, `flag ${JSON.stringify(flag)}`);
    const withIssuer = auth.readAuthConfig({ ...env, COMMERCE_OIDC_ISSUER: "https://idp.example.test" });
    assert.equal(withIssuer?.passwordLogin, false);
    assert.equal(withIssuer?.issuer, "https://idp.example.test");
  }
  // an unknown flag value, a bad BFF key, a missing origin and disabled identity
  assert.throws(() => auth.readAuthConfig({ ...good, COMMERCE_PASSWORD_LOGIN_ENABLED: "2" }));
  assert.throws(() => auth.readAuthConfig({ ...good, COMMERCE_PASSWORD_LOGIN_ENABLED: "true" }));
  assert.throws(() => auth.readAuthConfig({ ...good, COMMERCE_BFF_KEY: "short" }));
  assert.throws(() => auth.readAuthConfig({ ...good, COMMERCE_PUBLIC_ORIGIN: "" }));
  assert.throws(() => auth.readAuthConfig({ ...good, COMMERCE_PUBLIC_ORIGIN: "http://admin.example.test" }), "plain http public origin needs the loopback test flag");
  assert.equal(auth.readAuthConfig({ ...good, COMMERCE_IDENTITY_ENABLED: "0" }), null);
  assert.equal(auth.readAuthConfig({ COMMERCE_PASSWORD_LOGIN_ENABLED: "1" }), null);
  assert.throws(() => auth.readAuthConfig({ ...good, COMMERCE_FIXTURE_ENABLED: "1" }), "identity and fixture modes exclude each other");
  // the module-level configuration of this process (built at import) is the OIDC-less one
  assert.equal(auth.authConfig?.issuer, null);
  assert.equal(auth.passwordLoginOn(), true);
});

test("PA10 Origin rule: exactly the configured public origin, nothing else (§7.2)", () => {
  const req = (origin: string | null, url = `${publicOrigin}/api/auth/password/login`) =>
    new Request(url, { method: "POST", headers: origin === null ? {} : { origin } });
  assert.equal(auth.requireOrigin(req(publicOrigin)), true);
  for (const bad of [
    null,
    "",
    "null",
    "https://admin.example.test/",
    "https://admin.example.test:443",
    "http://admin.example.test",
    "https://evil.example.test",
    "https://admin.example.test.evil.test",
    "https://ADMIN.example.test",
    "https://sub.admin.example.test",
  ])
    assert.equal(auth.requireOrigin(req(bad)), false, String(bad));
  assert.equal(auth.hasNoQuery(new Request(`${publicOrigin}/api/auth/password/login`)), true);
  assert.equal(auth.hasNoQuery(new Request(`${publicOrigin}/api/auth/password/login?x=1`)), false);
});

test("PA10 the BFF call forwards the key and the client IP once and never retries", async () => {
  seen.length = 0;
  const res = await auth.privateIdentity("password/login", { email: "a@b.test", password: "pw", locale: "en" }, undefined, undefined, { timeoutMs: 14000, clientIP: "203.0.113.9" });
  assert.equal(res.status, 202);
  assert.equal(seen.length, 1);
  const h = seen[0].headers;
  assert.equal(h["x-commerce-bff-key"], bffKey);
  assert.equal(h["x-commerce-client-ip"], "203.0.113.9");
  for (const banned of ["authorization", "cookie", "origin", "x-forwarded-for", "forwarded"])
    assert.equal(h[banned], undefined, `${banned} must not be forwarded`);
  assert.equal(h["content-type"], "application/json");
  // without a client IP the header is absent (only the password routes ever set it)
  seen.length = 0;
  await auth.privateIdentity("logout", {}, "tok", undefined, {});
  assert.equal(seen[0].headers["x-commerce-client-ip"], undefined);
  // a hung upstream => 503 retry_later from the BFF's own timeout, exactly one request (no auto retry)
  seen.length = 0;
  hang = true;
  const started = Date.now();
  const slow = await auth.privateIdentity("password/reset", { email: "a@b.test", locale: "en" }, undefined, undefined, { timeoutMs: 300, clientIP: "203.0.113.9" });
  hang = false;
  assert.equal(slow.status, 503);
  assert.ok(Date.now() - started < 3000);
  await new Promise((r) => setTimeout(r, 400));
  assert.equal(seen.length, 1, "a timed-out password call must not be re-sent");
});

test("PA10 session and CSRF cookies come from the existing helper with the existing flags", () => {
  const token = randomBytes(32).toString("base64url");
  const headers = new Headers();
  const expires = new Date(Date.now() + 3_600_000).toISOString();
  assert.equal(auth.setSessionCookies(headers, token, expires), true);
  const set = headers.getSetCookie();
  assert.equal(set.length, 2);
  const session = set.find((c) => c.startsWith("__Host-commerce_session="))!;
  const csrf = set.find((c) => c.startsWith("__Host-commerce_csrf="))!;
  for (const flag of ["Secure", "HttpOnly", "SameSite=Lax", "Path="]) assert.ok(session.includes(flag), `session ${flag}`);
  for (const flag of ["Secure", "SameSite=Lax", "Path="]) assert.ok(csrf.includes(flag), `csrf ${flag}`);
  assert.ok(!csrf.includes("HttpOnly"), "the CSRF cookie is readable by the page");
  assert.ok(session.startsWith(`__Host-commerce_session=${token};`));
  const age = Number(/Max-Age=(\d+)/.exec(session)?.[1]);
  assert.ok(age > 3500 && age <= 3600);
  // unusable token or expiry: false and no cookie
  for (const [t, e] of [["short", expires], [token, "not-a-date"], [token, new Date(Date.now() - 1000).toISOString()], [5, expires]] as [unknown, unknown][]) {
    const h = new Headers();
    assert.equal(auth.setSessionCookies(h, t, e), false);
    assert.equal(h.getSetCookie().length, 0);
  }
});

test("PA10 upstream diagnostics never reach the browser: safeError rebuilds the body from an allow-listed code", async () => {
  const canary = "canary-" + randomBytes(9).toString("hex");
  const upstreamError = (status: number, code: unknown, retryAfter?: string) =>
    new Response(JSON.stringify({ code, message: `password ${canary} rejected for ${canary}@example.test`, details: { p: canary } }), {
      status,
      headers: { "content-type": "application/json", ...(retryAfter ? { "retry-after": retryAfter } : {}) },
    });
  for (const [status, code] of [[401, "invalid_credentials"], [401, "invalid_code"], [409, "account_exists"], [422, "password_policy"], [503, "mail_unavailable"]] as const) {
    const safe = await auth.safeError(upstreamError(status, code));
    const text = await safe.text();
    assert.equal(safe.status, status);
    assert.ok(!text.includes(canary), `${code}: upstream message leaked`);
    assert.ok(text.includes(code));
  }
  const throttled = await auth.safeError(upstreamError(429, "throttled", "90"));
  assert.equal(throttled.headers.get("retry-after"), "90");
  assert.ok(!(await throttled.text()).includes(canary));
  // Retry-After is bounded numeric only, and only on 429
  for (const bad of ["0", "-1", "abc", "99999", "3601", "1.5", "1e3"])
    assert.equal((await auth.safeError(upstreamError(429, "throttled", bad))).headers.get("retry-after"), null, bad);
  assert.equal((await auth.safeError(upstreamError(401, "invalid_code", "90"))).headers.get("retry-after"), null);
  // an unknown or malformed code becomes the generic retry_later
  const odd = await auth.safeError(upstreamError(500, "Sensitive Code!"));
  assert.ok(!(await odd.text()).includes("Sensitive"));
});
