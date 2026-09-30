import { csrfCookie, sessionBoundary } from "./settings-client";
import { validStudioInputToken } from "./studio-request";
import {
  parseDraft,
  parseStudioDetail,
  parseStudioInput,
  parseStudioInputPrepared,
  parseStudioPage,
  parseStudioReceipt,
  type AspectRatio,
} from "./studio-model";

export type StudioErrorCode = "signed-out" | "forbidden" | "not-found" | "conflict" | "invalid" | "unavailable" | "uncertain";
export class StudioError extends Error {
  // `api` is the backend's own bounded error code (e.g. "binding_missing") of a definite
  // 4xx answer, for callers that word specific refusals; never set for an unknown result.
  constructor(readonly code: StudioErrorCode, readonly api = "") { super(code); }
}
export type DraftInput = { title: string; scheduled_at: string | null; aspect_ratio: AspectRatio };

function classify(status: number): StudioErrorCode {
  if (status === 401) return "signed-out";
  if (status === 403) return "forbidden";
  if (status === 404) return "not-found";
  if (status === 409) return "conflict";
  if (status === 400 || status === 422) return "invalid";
  return "unavailable";
}
async function json(response: Response, uncertain: boolean): Promise<unknown> {
  if (response.headers.get("content-type")?.split(";", 1)[0] !== "application/json" ||
    response.headers.get("cache-control") !== "private, no-store")
    throw new StudioError(uncertain ? "uncertain" : "unavailable");
  try { return await response.json(); }
  catch { throw new StudioError(uncertain ? "uncertain" : "unavailable"); }
}
// Studio › Claims (claims-client.ts) reuses this private read/write boundary unchanged.
export async function read(path: string, signal: AbortSignal): Promise<unknown> {
  let response: Response;
  try {
    response = await fetch(path, { method: "GET", cache: "no-store", credentials: "same-origin", signal });
  } catch { throw new StudioError("unavailable"); }
  if (!response.ok) throw new StudioError(classify(response.status));
  return json(response, false);
}
export async function readStudioPage(store: string, cursor: string, signal: AbortSignal) {
  const path = `/api/stores/${store}/live-sessions?limit=20${cursor ? `&cursor=${cursor}` : ""}`;
  try { return parseStudioPage(await read(path, signal)); }
  catch (error) { throw error instanceof StudioError ? error : new StudioError("unavailable"); }
}
export async function readStudioDetail(store: string, sessionID: string, signal: AbortSignal) {
  try { return parseStudioDetail(await read(`/api/stores/${store}/live-sessions/${sessionID}`, signal), sessionID); }
  catch (error) { throw error instanceof StudioError ? error : new StudioError("unavailable"); }
}
export async function readStudioInput(store: string, sessionID: string, signal: AbortSignal) {
  try { return parseStudioInput(await read(`/api/stores/${store}/live-sessions/${sessionID}/input`, signal)); }
  catch (error) { throw error instanceof StudioError ? error : new StudioError("unavailable"); }
}
export async function readStudioInputPrepared(store: string, sessionID: string, signal: AbortSignal) {
  try { return parseStudioInputPrepared(await read(`/api/stores/${store}/live-sessions/${sessionID}/input/prepared`, signal)); }
  catch (error) { throw error instanceof StudioError ? error : new StudioError("unavailable"); }
}

export async function write(path: string, method: "POST" | "PATCH" | "PUT", body: unknown, key: string, boundary: string): Promise<unknown> {
  const csrf = csrfCookie();
  if (!csrf) throw new StudioError("signed-out");
  try {
    if ((await sessionBoundary(csrf)) !== boundary || csrfCookie() !== csrf) throw new StudioError("signed-out");
  } catch { throw new StudioError("signed-out"); }
  let response: Response;
  try {
    response = await fetch(path, {
      method, credentials: "same-origin", cache: "no-store", signal: AbortSignal.timeout(12000),
      headers: { "Content-Type": "application/json", "X-CSRF-Token": csrf, "Idempotency-Key": key },
      body: JSON.stringify(body),
    });
  } catch { throw new StudioError("uncertain"); }
  if (!response.ok) {
    if (response.status >= 500) throw new StudioError("uncertain");
    const body: unknown = await response.json().catch(() => null);
    const api = body && typeof body === "object" && typeof (body as { code?: unknown }).code === "string" ? (body as { code: string }).code : "";
    throw new StudioError(classify(response.status), /^[a-z0-9_]{1,64}$/.test(api) ? api : "");
  }
  const result = await json(response, true);
  try {
    if ((await sessionBoundary()) !== boundary) throw new StudioError("signed-out");
  } catch { throw new StudioError("signed-out"); }
  return result;
}
export async function createStudioDraft(store: string, input: DraftInput, key: string, boundary: string) {
  try { return parseDraft(await write(`/api/stores/${store}/live-sessions`, "POST", input, key, boundary)); }
  catch (error) { throw error instanceof StudioError ? error : new StudioError("uncertain"); }
}
export async function editStudioDraft(store: string, sessionID: string, input: DraftInput, version: number, key: string, boundary: string) {
  try { return parseDraft(await write(`/api/stores/${store}/live-sessions/${sessionID}`, "PATCH", { ...input, expected_version: version }, key, boundary)); }
  catch (error) { throw error instanceof StudioError ? error : new StudioError("uncertain"); }
}
export async function startStudioRehearsal(store: string, sessionID: string, authorizationID: string, version: number, key: string, boundary: string) {
  try {
    return parseStudioReceipt(await write(`/api/stores/${store}/live-sessions/${sessionID}/rehearsal/start`, "POST",
      { authorization_id: authorizationID, expected_session_version: version }, key, boundary), sessionID);
  } catch (error) { throw error instanceof StudioError ? error : new StudioError("uncertain"); }
}
export async function startStudioInput(store: string, sessionID: string, authorizationID: string, version: number, key: string, boundary: string) {
  try {
    return parseStudioReceipt(await write(`/api/stores/${store}/live-sessions/${sessionID}/input/start`, "POST",
      { authorization_id: authorizationID, expected_session_version: version }, key, boundary), sessionID);
  } catch (error) { throw error instanceof StudioError ? error : new StudioError("uncertain"); }
}
export async function requestStudioInputToken(store: string, sessionID: string, attemptID: string, version: number, key: string, boundary: string) {
  // Reuse the authenticated write boundary, but never journal its secret result.
  // A failed/ambiguous delivery must not mint a new command key or auto-retry.
  try {
    const result = await write(`/api/stores/${store}/live-sessions/${sessionID}/input/token`, "POST",
      { attempt_id: attemptID, expected_session_version: version }, key, boundary);
    if (!validStudioInputToken(result) || result.attempt_id !== attemptID ||
        result.expires_at <= Math.floor(Date.now() / 1000)) throw new StudioError("uncertain");
    return result;
  } catch (error) { throw error instanceof StudioError ? error : new StudioError("uncertain"); }
}
export async function stopStudioRehearsal(store: string, sessionID: string, attemptID: string, key: string, boundary: string) {
  try {
    return parseStudioReceipt(await write(`/api/stores/${store}/live-sessions/${sessionID}/rehearsal/stop`, "POST",
      { attempt_id: attemptID }, key, boundary), sessionID);
  } catch (error) { throw error instanceof StudioError ? error : new StudioError("uncertain"); }
}
