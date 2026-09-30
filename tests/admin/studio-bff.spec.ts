import assert from "node:assert/strict";
import { test } from "node:test";
import * as http from "node:http";
import { chromium } from "@playwright/test";

const required = (name: string) => {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required`);
  return value;
};
const origin = required("LC_BROWSER_PUBLIC_ORIGIN");
const api = required("LC_BROWSER_API_ORIGIN");
const store = required("LC_BROWSER_STUDIO_STORE");
const foreignStore = required("LC_BROWSER_STUDIO_FOREIGN_STORE");
const unlistedStore = required("LC_BROWSER_STUDIO_UNLISTED_STORE");
const session = required("LC_BROWSER_STUDIO_SESSION");
const authorization = required("LC_BROWSER_STUDIO_AUTHORIZATION");
const readonlyToken = required("LC_BROWSER_STUDIO_READONLY_TOKEN");
const expiredToken = required("LC_BROWSER_STUDIO_EXPIRED_TOKEN");
const revokedToken = required("LC_BROWSER_STUDIO_REVOKED_TOKEN");
const base = `/api/stores/${store}/live-sessions`;
const detail = `${base}/${session}`;
const sessionCookie = "__Host-commerce_session";
const csrfCookie = "__Host-commerce_csrf";

type Raw = { status: number; headers: http.IncomingHttpHeaders; body: string };
function raw(path: string, method = "GET", headers: Record<string, string> = {}, body?: string | Buffer): Promise<Raw> {
  const url = new URL(origin);
  return new Promise((resolve, reject) => {
    const request = http.request({ hostname: url.hostname, port: url.port, method, path, headers, timeout: 5000 }, (response) => {
      const chunks: Buffer[] = [];
      response.on("data", (chunk: Buffer) => chunks.push(chunk));
      response.on("end", () => resolve({ status: response.statusCode ?? 0, headers: response.headers, body: Buffer.concat(chunks).toString("utf8") }));
    });
    request.on("timeout", () => request.destroy(new Error("Next request timeout")));
    request.on("error", reject);
    request.end(body);
  });
}
async function observation() {
  const response = await fetch(`${api}/__test/studio-observation`);
  assert.equal(response.status, 200);
  return (await response.json()) as { count: number; stripped_failures: number; last_uri: string; last_method: string };
}
function safe(response: Raw, status: number) {
  assert.equal(response.status, status, response.body);
  assert.equal(response.headers["cache-control"], "private, no-store");
  assert.doesNotMatch(response.body, /backend-secret|SQLSTATE|browser-bogus-token|studio-secret/i);
  const body = JSON.parse(response.body) as Record<string, unknown>;
  assert.equal(typeof body.code, "string");
  assert.deepEqual(body.details, {});
}
async function deniedWithoutUpstream(path: string, method: string, headers: Record<string, string>, body: string | Buffer | undefined, status: number) {
  const before = (await observation()).count;
  const response = await raw(path, method, headers, body);
  safe(response, status);
  assert.equal((await observation()).count, before, `${method} ${path} reached Go`);
}

test("Studio BFF signed browser session, exact six routes and fail-closed transport", { timeout: 100_000 }, async () => {
  const browser = await chromium.launch({ headless: true });
  try {
    const context = await browser.newContext({ baseURL: origin });
    const page = await context.newPage();
    await page.goto(`${origin}/en/`);
    await page.getByRole("button", { name: "Sign in with identity service" }).click();
    let cookies = await context.cookies();
    for (let i = 0; i < 100 && !cookies.some((item) => item.name === sessionCookie); i++) {
      await new Promise((resolve) => setTimeout(resolve, 50));
      cookies = await context.cookies();
    }
    const sessionValue = cookies.find((item) => item.name === sessionCookie)?.value;
    const csrfValue = cookies.find((item) => item.name === csrfCookie)?.value;
    assert.ok(sessionValue && csrfValue, "signed session and CSRF cookies required");
    assert.equal(cookies.find((item) => item.name === sessionCookie)?.httpOnly, true);
    const cookie = `${sessionCookie}=${sessionValue}; ${csrfCookie}=${csrfValue}`;
    const authorized = { Cookie: cookie };
    const write = (key: string) => ({ ...authorized, Origin: origin, "X-CSRF-Token": csrfValue, "Content-Type": "application/json", "Idempotency-Key": key });

    const first = await raw(`${base}?limit=1`, "GET", { ...authorized, Authorization: "Bearer browser-bogus-token", "X-Tenant-ID": "bogus", "X-Forwarded-Host": "attacker.invalid", "X-BFF-Test": "bogus" });
    assert.equal(first.status, 200, first.body);
    assert.equal(first.headers["cache-control"], "private, no-store");
    const firstPage = JSON.parse(first.body) as { items: Array<{ session_id: string }>; next_cursor: string };
    assert.equal(firstPage.items.length, 1);
    assert.equal(firstPage.items[0].session_id, session);
    assert.equal((await observation()).last_uri, `/v1/admin/stores/${store}/live-sessions?limit=1`);
    const prepared = await raw(detail, "GET", authorized);
    assert.equal(prepared.status, 200, prepared.body);
    const studio = JSON.parse(prepared.body) as Record<string, any>;
    assert.deepEqual(Object.keys(studio).sort(), ["attempt", "can_manage", "draft", "media_enabled", "prepared"]);
    assert.equal(studio.media_enabled, true); // this harness mounts the MOCK media planner (G2 capability)
    assert.equal(studio.prepared.authorization_id, authorization);
    assert.equal(studio.attempt, null);
    assert.deepEqual(Object.keys(studio.prepared.destinations[0]).sort(), ["ordinal", "provider"]);

    const createBody = `{"title":"Studio via BFF","aspect_ratio":"16:9"}`;
    const createKey = `studio-bff-create-${Date.now()}`;
    const created = await raw(base, "POST", write(createKey), createBody);
    assert.equal(created.status, 200, created.body);
    assert.equal(created.headers["cache-control"], "private, no-store");
    const draft = JSON.parse(created.body) as { session_id: string; version: number; title: string };
    assert.equal(draft.version, 1);
    const replay = await raw(base, "POST", write(createKey), createBody);
    assert.deepEqual(JSON.parse(replay.body), draft);
    const changedReplay = await raw(base, "POST", write(createKey), `{"title":"changed replay","aspect_ratio":"16:9"}`);
    safe(changedReplay, 409);
    const list = await raw(`${base}?limit=1`, "GET", authorized);
    assert.equal(list.status, 200, list.body);
    const pageBody = JSON.parse(list.body) as { items: Array<{ session_id: string }>; next_cursor: string };
    assert.equal(pageBody.items[0].session_id, draft.session_id);
    assert.match(pageBody.next_cursor, /^[A-Za-z0-9_-]{1,1024}$/);
    const next = await raw(`${base}?limit=1&cursor=${pageBody.next_cursor}`, "GET", authorized);
    assert.equal(next.status, 200, next.body);
    assert.equal((JSON.parse(next.body) as { items: Array<{ session_id: string }> }).items[0].session_id, session);
    const blank = await raw(`${base}/${draft.session_id}`, "GET", authorized);
    assert.equal(blank.status, 200, blank.body);
    assert.equal((JSON.parse(blank.body) as any).prepared, null);
    const editKey = `studio-bff-edit-${Date.now()}`;
    const edit = await raw(`${base}/${draft.session_id}`, "PATCH", write(editKey), `{"title":"Edited via BFF","aspect_ratio":"16:9","expected_version":1}`);
    assert.equal(edit.status, 200, edit.body);
    assert.equal((JSON.parse(edit.body) as any).version, 2);
    safe(await raw(`${base}/${draft.session_id}`, "PATCH", write(`studio-bff-stale-${Date.now()}`), `{"title":"stale","aspect_ratio":"16:9","expected_version":1}`), 409);

    const startPath = `${detail}/rehearsal/start`;
    const startBody = `{"authorization_id":"${authorization}","expected_session_version":1}`;
    const startKey = `studio-bff-start-${Date.now()}`;
    const started = await raw(startPath, "POST", write(startKey), startBody);
    assert.equal(started.status, 200, started.body);
    const receipt = JSON.parse(started.body) as Record<string, any>;
    assert.deepEqual(Object.keys(receipt).sort(), ["attempt_id", "session_id", "state"]);
    assert.equal(receipt.session_id, session);
    assert.deepEqual(JSON.parse((await raw(startPath, "POST", write(startKey), startBody)).body), receipt);
    safe(await raw(`${base}/${draft.session_id}/rehearsal/start`, "POST", write(`studio-bff-noauth-${Date.now()}`), `{"authorization_id":"${authorization}","expected_session_version":2}`), 404);
    const stopPath = `${detail}/rehearsal/stop`;
    const stopped = await raw(stopPath, "POST", write(`studio-bff-stop-${Date.now()}`), `{"attempt_id":"${receipt.attempt_id}"}`);
    assert.equal(stopped.status, 200, stopped.body);
    const stopReceipt = JSON.parse(stopped.body) as Record<string, unknown>;
    assert.deepEqual(Object.keys(stopReceipt).sort(), ["attempt_id", "session_id", "state"]);
    assert.equal(stopReceipt.attempt_id, receipt.attempt_id);
    assert.equal(stopReceipt.state, "cancelled_before_start", "no worker may imply provider success");
    const afterStop = await raw(detail, "GET", authorized);
    assert.equal(afterStop.status, 200, afterStop.body);
    const attempt = (JSON.parse(afterStop.body) as any).attempt as Record<string, unknown>;
    assert.deepEqual(Object.keys(attempt).sort(), ["attempt_id", "cleanup_required", "destinations", "environment", "escalated", "operation_state", "resource_state", "stop_requested", "stop_wire_count", "transport_status", "updated_at"]);
    assert.deepEqual(Object.keys((attempt.destinations as any[])[0]).sort(), ["ordinal", "provider"]);
    assert.equal(attempt.operation_state, "CANCELLED");
    assert.equal(attempt.resource_state, "UNOBSERVED");
    assert.equal(attempt.stop_requested, true);
    assert.equal(attempt.stop_wire_count, 0);

    const readonly = { Cookie: `${sessionCookie}=${readonlyToken}` };
    const readonlyRead = await raw(detail, "GET", readonly);
    assert.equal(readonlyRead.status, 200, readonlyRead.body);
    assert.equal((JSON.parse(readonlyRead.body) as any).can_manage, false);
    safe(await raw(base, "POST", { ...write(`studio-bff-readonly-${Date.now()}`), Cookie: `${sessionCookie}=${readonlyToken}; ${csrfCookie}=${csrfValue}` }, createBody), 403);
    safe(await raw(`/api/stores/${foreignStore}/live-sessions/${session}`, "GET", authorized), 404);
    await deniedWithoutUpstream(`/api/stores/${unlistedStore}/live-sessions`, "GET", authorized, undefined, 404);

    for (const [path, method, extra, body] of [
      [`${base}?`, "GET", {}, undefined],
      [`${base}?limit=1&limit=2`, "GET", {}, undefined],
      [`${base}?li%6dit=1`, "GET", {}, undefined],
      [`${base}?limit=%31`, "GET", {}, undefined],
      [`${base}?cursor=A+B`, "GET", {}, undefined],
      [`${base}?unknown=1`, "GET", {}, undefined],
      [`${detail}?`, "GET", {}, undefined],
      [`${startPath}?x=1`, "POST", write(`studio-bff-query-${Date.now()}`), startBody],
      [base, "GET", { "Idempotency-Key": "read-key" }, undefined],
      [base, "GET", { "Content-Length": "1" }, "x"],
    ] as Array<[string, string, Record<string, string>, string | undefined]>) {
      await deniedWithoutUpstream(path, method, { ...authorized, ...extra }, body, 422);
    }
    for (const path of [
      `/api/stores/${store}/%6cive-sessions`,
      `/api/stores/${store}/live-sessions%2F${session}`,
      `/api/stores/${store}/live-sessions-extra`,
      `/api/stores/${store}/live-sessions/${session}/unknown`,
    ]) {
      await deniedWithoutUpstream(path, "GET", authorized, undefined, 404);
    }
    for (const method of ["DELETE", "OPTIONS", "HEAD"]) {
      const before = (await observation()).count;
      const response = await raw(detail, method, authorized);
      assert.equal(response.status, 405);
      assert.equal(response.headers["cache-control"], "private, no-store");
      assert.equal((await observation()).count, before);
    }
    await deniedWithoutUpstream(startPath, "GET", authorized, undefined, 405);
    await deniedWithoutUpstream(base, "POST", { ...authorized, "Content-Type": "application/json", "Idempotency-Key": `studio-bff-nocsrf-${Date.now()}` }, createBody, 403);
    await deniedWithoutUpstream(base, "POST", { ...write(`studio-bff-origin-${Date.now()}`), Origin: "http://attacker.invalid" }, createBody, 403);
    await deniedWithoutUpstream(base, "POST", { ...write(`studio-bff-csrf-${Date.now()}`), "X-CSRF-Token": "invalid" }, createBody, 403);
    await deniedWithoutUpstream(base, "POST", write(`studio-bff-utf8-${Date.now()}`), Buffer.from([0xff]), 400);
    await deniedWithoutUpstream(base, "POST", write(`studio-bff-large-${Date.now()}`), `{"title":"${"x".repeat(65537)}","aspect_ratio":"16:9"}`, 400);
    await deniedWithoutUpstream(`${base}/${draft.session_id}`, "PATCH", write(`studio-bff-large-edit-${Date.now()}`), `{"title":"${"x".repeat(65537)}","aspect_ratio":"16:9","expected_version":2}`, 400);
    safe(await raw(base, "POST", write(`studio-bff-dup-${Date.now()}`), `{"title":"a","title":"b","aspect_ratio":"16:9"}`), 400);
    safe(await raw(base, "POST", write(`studio-bff-bom-${Date.now()}`), Buffer.concat([Buffer.from([0xef, 0xbb, 0xbf]), Buffer.from(createBody)])), 400);
    for (const token of [expiredToken, revokedToken]) {
      safe(await raw(base, "GET", { Cookie: `${sessionCookie}=${token}` }), 401);
    }
    await deniedWithoutUpstream(base, "GET", { Authorization: "Bearer browser-bogus-token" }, undefined, 401);
    for (const cursor of ["NONJSON", "OVERSIZE", "ERROR"]) {
      const response = await raw(`${base}?cursor=${cursor}`, "GET", authorized);
      safe(response, 503);
      assert.equal(response.headers["x-backend-secret"], undefined);
      assert.equal(response.headers["set-cookie"], undefined);
    }
    assert.equal((await observation()).stripped_failures, 0);
  } finally {
    await browser.close();
  }
});
