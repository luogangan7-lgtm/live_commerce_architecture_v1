// Admin ads client: browser -> BFF `/api/stores/{store}/ads/*` -> Go `/v1/admin/stores/{store_id}/ads/*`
// (internal/httpapi/ads.go), and browser -> BFF POST `/api/ads/meta/connect` -> Go POST ads/meta/connect.
// Reads parse the frozen DTOs (ads-model.ts); writes carry cookie CSRF + session fence + Idempotency-Key (+ If-Match
// for PUT drafts/{id}) and never trust their own response body for state: the caller re-GETs (no optimistic state).
// Non-goals: no token ever reaches this file (the Meta token stays in Go/PG, HPKE-sealed); no retry policy.
import { csrfCookie, sessionBoundary } from "./settings-client";
import {
  adsErrorCode, parseConnectState, parseDraft, parseDraftList, parseReport, parseSettings,
  type AdsCode, type Draft,
} from "./ads-model";
import { safeDialogURL } from "./ads-request";

export type AdsReadCode = "signed-out" | "forbidden" | "not-found" | "unavailable" | AdsCode;
export class AdsReadError extends Error {
  constructor(readonly code: AdsReadCode) {
    super(code);
  }
}

async function read<T>(path: string, parse: (value: unknown) => T, signal: AbortSignal): Promise<T> {
  let response: Response;
  try {
    response = await fetch(path, { method: "GET", cache: "no-store", credentials: "same-origin", signal });
  } catch {
    throw new AdsReadError("unavailable");
  }
  if (response.status === 401) throw new AdsReadError("signed-out");
  if (response.status === 403) throw new AdsReadError("forbidden");
  if (response.status === 404) throw new AdsReadError("not-found");
  let body: unknown = null;
  if (response.headers.get("content-type")?.split(";", 1)[0] === "application/json") body = await response.json().catch(() => null);
  if (!response.ok) {
    // e.g. 410 state_expired on the pick list; 429/5xx stay "unavailable".
    const code = adsErrorCode(body);
    throw new AdsReadError(code === "retry_later" ? "unavailable" : code);
  }
  try {
    return parse(body);
  } catch {
    // A shape or enum we do not know is an error state, never a guess.
    throw new AdsReadError("unavailable");
  }
}

const base = (store: string) => `/api/stores/${store}/ads`;
// Go GET ads/settings (ads:read)
export const readSettings = (store: string, signal: AbortSignal) => read(`${base(store)}/settings`, parseSettings, signal);
// Go GET ads/drafts (ads:read)
export const readDrafts = (store: string, signal: AbortSignal) => read(`${base(store)}/drafts`, parseDraftList, signal);
// Go GET ads/drafts/{id} (ads:read); the revision it returns is the next If-Match.
export const readDraft = (store: string, id: string, signal: AbortSignal) =>
  read(`${base(store)}/drafts/${id}`, (value) => {
    const d = parseDraft(value);
    if (d.id !== id) throw new Error("id");
    return d;
  }, signal);
// Go GET ads/meta/states/{state_id} (ads:manage + integration:manage; principal + store match, unexpired)
export const readConnectState = (store: string, stateID: string, signal: AbortSignal) =>
  read(`${base(store)}/meta/states/${stateID}`, (value) => parseConnectState(value, stateID), signal);
// Go GET ads/report?from&to (ads:read)
export const readReport = (store: string, from: string, to: string, signal: AbortSignal) =>
  read(`${base(store)}/report?from=${from}&to=${to}`, parseReport, signal);

export type WriteResult<T = null> = { ok: true; value: T } | { ok: false; code: AdsCode; uncertain: boolean };

async function write<T>(
  method: "POST" | "PUT", url: string, key: string, body: string, boundary: string,
  parse: (value: unknown) => T, ifMatch?: number,
): Promise<WriteResult<T>> {
  const csrf = csrfCookie();
  try {
    if (!csrf || (await sessionBoundary(csrf)) !== boundary || csrfCookie() !== csrf) throw new Error("session_changed");
  } catch {
    return { ok: false, code: "unauthorized", uncertain: false }; // nothing was sent
  }
  let response: Response;
  try {
    response = await fetch(url, {
      method,
      credentials: "same-origin",
      headers: {
        "Content-Type": "application/json", "Idempotency-Key": key, "X-CSRF-Token": csrf,
        ...(ifMatch === undefined ? {} : { "If-Match": String(ifMatch) }),
      },
      body,
      signal: AbortSignal.timeout(8000),
    });
  } catch {
    // Outcome unknown: the caller keeps the same key + bytes for a retry (the command may have committed).
    return { ok: false, code: "retry_later", uncertain: true };
  }
  const value: unknown = await response.json().catch(() => null);
  if (response.status >= 500) return { ok: false, code: adsErrorCode(value), uncertain: true };
  if (!response.ok) return { ok: false, code: adsErrorCode(value), uncertain: false };
  try {
    return { ok: true, value: parse(value) };
  } catch {
    // Accepted by the server but the body is not the shape we know: state is re-read anyway.
    return { ok: true, value: null as T };
  }
}
const none = () => null;
const draftOrNull = (value: unknown): Draft | null => {
  try {
    return parseDraft(value);
  } catch {
    return null;
  }
};

// Go POST ads/meta/bindings (Idempotency-Key): {state_id, ad_account_id, dataset_id?}
export const postBindings = (store: string, key: string, body: string, boundary: string) =>
  write("POST", `${base(store)}/meta/bindings`, key, body, boundary, none);
// Go POST ads/drafts (Idempotency-Key, ads:manage) -> 201 Draft
export const postDraft = (store: string, key: string, body: string, boundary: string) =>
  write("POST", `${base(store)}/drafts`, key, body, boundary, draftOrNull);
// Go PUT ads/drafts/{id} (Idempotency-Key + If-Match: <revision>) -> 200 Draft
export const putDraft = (store: string, id: string, key: string, body: string, boundary: string, revision: number) =>
  write("PUT", `${base(store)}/drafts/${id}`, key, body, boundary, draftOrNull, revision);
// Go POST ads/drafts/{id}/{approve|publish|pause|end}. approve body {revision}, publish body {publish_attempt}, pause/end {}.
export const postDraftAction = (
  store: string, id: string, action: "approve" | "publish" | "pause" | "end", key: string, body: string, boundary: string,
) => write("POST", `${base(store)}/drafts/${id}/${action}`, key, body, boundary, none);
// Go PUT ads/capi (Idempotency-Key, ads:manage) -> settings.capi
export const putCapi = (store: string, key: string, body: string, boundary: string) =>
  write("PUT", `${base(store)}/capi`, key, body, boundary, none);

// BFF POST /api/ads/meta/connect -> Go POST ads/meta/connect; answers {dialog_url} (checked to be exactly www.facebook.com).
export function postConnect(store: string, key: string, boundary: string): Promise<WriteResult<string>> {
  return write("POST", "/api/ads/meta/connect", key, JSON.stringify({ store }), boundary, (value) => {
    const url = safeDialogURL(value && typeof value === "object" ? (value as Record<string, unknown>).dialog_url : null);
    if (!url) throw new Error("dialog_url");
    return url;
  }).then((result) => (result.ok && typeof result.value !== "string" ? { ok: false as const, code: "retry_later" as const, uncertain: false } : result));
}

export const newKey = (prefix: string) => `${prefix}-${crypto.randomUUID()}`;
