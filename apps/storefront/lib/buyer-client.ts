// Browser-only buyer session coordination. The bearer stays in the HttpOnly
// cookie; neither this module nor its journal ever reads it.
import { CLAIM_TOKEN } from "./claim-contract.ts";

const LOCK = "commerce-buyer-session-v1";
const PENDING = "commerce-buyer-pending-v1";
const CONTEXT = /^[A-Za-z0-9_-]{43}$/;
const KEY = /^[A-Za-z0-9_.:-]{8,128}$/;
const OPERATION =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
// The only no-key mutations: handoff (a committed one-shot form release, never a replay) and
// the Stripe refresh/cancel signals (stripe-buyer-ui-v1 §2; Go dedupes them by attempt).
// Keep this set exact; payment UI owns GET-only recovery after uncertainty.
const HANDOFF =
  /^orders\/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\/payment\/(?:handoff|refresh|cancel)$/;
// taiwan-cvs-logistics-v1 §5.2: verify re-reads a map selection; keyless, no body, safe to repeat.
const CVS_VERIFY =
  /^cvs-selections\/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\/verify$/;

export type SessionStatus = {
  state: "absent" | "expired" | "inactive" | "active";
  context: string | null;
  expires_at: string | null;
};

type Journal = {
  v: 1;
  id: string;
  baseline: string | null;
  phase: "prepare" | "reset";
};

export class BuyerClientError extends Error {
  readonly code:
    | "context_changed"
    | "uncertain"
    | "unavailable"
    | "invalid_response"
    | "requires_reset"
    | "request_failed";
  readonly status?: number;
  // The server's own refusal code (e.g. cvs_recipient_rejected) when a definite 4xx carried one; UI text only.
  readonly detail?: string;
  constructor(code: BuyerClientError["code"], status?: number, detail?: string) {
    super(code);
    this.name = "BuyerClientError";
    this.code = code;
    this.status = status;
    this.detail = detail;
  }
}

function storage(): Storage {
  try {
    const result = window.localStorage;
    // Availability must be established before the first cookie-changing fetch.
    result.getItem(PENDING);
    return result;
  } catch {
    throw new BuyerClientError("unavailable");
  }
}

function journal(store: Storage): Journal | null {
  let raw: string | null;
  try {
    raw = store.getItem(PENDING);
  } catch {
    throw new BuyerClientError("unavailable");
  }
  if (raw === null) return null;
  try {
    const parsed: unknown = JSON.parse(raw);
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed))
      throw new Error("journal");
    const value = parsed as Record<string, unknown>;
    if (
      Object.keys(value).sort().join(",") !== "baseline,id,phase,v" ||
      value.v !== 1 ||
      typeof value.id !== "string" ||
      !OPERATION.test(value.id) ||
      (value.baseline !== null &&
        (typeof value.baseline !== "string" ||
          !CONTEXT.test(value.baseline))) ||
      (value.phase !== "prepare" && value.phase !== "reset")
    )
      throw new Error("journal");
    return value as Journal;
  } catch {
    throw new BuyerClientError("uncertain");
  }
}

function persist(store: Storage, value: Journal) {
  try {
    store.setItem(PENDING, JSON.stringify(value));
  } catch {
    throw new BuyerClientError("unavailable");
  }
  if (journal(store)?.id !== value.id)
    throw new BuyerClientError("unavailable");
}

function clear(store: Storage, id: string) {
  if (journal(store)?.id !== id) throw new BuyerClientError("uncertain");
  try {
    store.removeItem(PENDING);
  } catch {
    throw new BuyerClientError("unavailable");
  }
  if (journal(store) !== null) throw new BuyerClientError("unavailable");
}

async function locked<T>(work: (store: Storage) => Promise<T>): Promise<T> {
  if (
    typeof window === "undefined" ||
    typeof navigator === "undefined" ||
    !navigator.locks?.request
  )
    throw new BuyerClientError("unavailable");
  const store = storage();
  try {
    return await navigator.locks.request(LOCK, { mode: "exclusive" }, () =>
      work(store),
    );
  } catch (error) {
    if (error instanceof BuyerClientError) throw error;
    throw new BuyerClientError("unavailable");
  }
}

async function request(
  method: string,
  suffix: string,
  context?: string,
  body?: unknown,
  idempotencyKey?: string,
  claimToken?: string,
): Promise<Response> {
  const headers = new Headers();
  if (context !== undefined) headers.set("X-Buyer-Context", context);
  if (body !== undefined) headers.set("Content-Type", "application/json");
  if (idempotencyKey !== undefined)
    headers.set("Idempotency-Key", idempotencyKey);
  if (claimToken !== undefined) headers.set("X-Commerce-Claim-Token", claimToken);
  try {
    return await fetch(`/api/buyer/${suffix}`, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      cache: "no-store",
      credentials: "same-origin",
      redirect: "error",
    });
  } catch {
    throw new BuyerClientError("uncertain");
  }
}

async function parsedStatus(response: Response): Promise<SessionStatus> {
  if (
    !response.ok ||
    response.headers.get("content-type")?.split(";", 1)[0] !==
      "application/json"
  )
    throw new BuyerClientError("invalid_response", response.status);
  let data: unknown;
  try {
    data = await response.json();
  } catch {
    throw new BuyerClientError("invalid_response");
  }
  if (!data || typeof data !== "object" || Array.isArray(data))
    throw new BuyerClientError("invalid_response");
  const value = data as Record<string, unknown>;
  if (
    Object.keys(value).sort().join(",") !== "context,expires_at,state" ||
    !["absent", "expired", "inactive", "active"].includes(
      String(value.state),
    ) ||
    (value.context !== null &&
      (typeof value.context !== "string" || !CONTEXT.test(value.context))) ||
    (value.expires_at !== null &&
      (typeof value.expires_at !== "string" ||
        !Number.isFinite(Date.parse(value.expires_at)))) ||
    (value.state === "absent" &&
      (value.context !== null || value.expires_at !== null)) ||
    (value.state !== "absent" &&
      (value.context === null || value.expires_at === null))
  )
    throw new BuyerClientError("invalid_response");
  return value as SessionStatus;
}

async function status(): Promise<SessionStatus> {
  const response = await request("GET", "session");
  if (!response.ok) {
    if (response.status === 401)
      throw new BuyerClientError("requires_reset", 401);
    throw new BuyerClientError("request_failed", response.status);
  }
  return parsedStatus(response);
}

async function recover(
  store: Storage,
  current: SessionStatus,
): Promise<boolean> {
  const pending = journal(store);
  if (!pending) return false;
  if (current.context !== null && current.context !== pending.baseline) {
    clear(store, pending.id);
    return false;
  }
  return true;
}

// A received, parsed local error is conclusive because the server never sets a
// cookie on errors. HTML, malformed JSON, aborts and lost responses are not.
export async function definiteError(response: Response): Promise<boolean> {
  if (
    response.ok ||
    response.headers.get("content-type")?.split(";", 1)[0] !==
      "application/json" ||
    !response.headers
      .get("cache-control")
      ?.split(",")
      .some((value) => value.trim() === "no-store") ||
    response.headers.get("x-content-type-options") !== "nosniff"
  )
    return false;
  try {
    const body: unknown = await response.json();
    if (!body || typeof body !== "object" || Array.isArray(body)) return false;
    const value = body as Record<string, unknown>;
    return (
      Object.keys(value).sort().join(",") ===
        "code,details,message,request_id,retryable" &&
      typeof value.code === "string" &&
      /^[a-z_]{1,64}$/.test(value.code) &&
      typeof value.message === "string" &&
      typeof value.request_id === "string" &&
      /^[0-9a-f]{32}$/.test(value.request_id) &&
      typeof value.retryable === "boolean" &&
      !!value.details &&
      typeof value.details === "object" &&
      !Array.isArray(value.details) &&
      Object.keys(value.details).length === 0
    );
  } catch {
    return false;
  }
}

async function activate(current: SessionStatus): Promise<SessionStatus> {
  if (current.state === "active") return current;
  if (current.state !== "inactive" || !current.context)
    throw new BuyerClientError("requires_reset");
  const response = await request(
    "POST",
    "session/activate",
    current.context,
    {},
  );
  if (response.status === 409)
    throw new BuyerClientError("context_changed", 409);
  if (!response.ok)
    throw new BuyerClientError("request_failed", response.status);
  const next = await parsedStatus(response);
  if (next.state !== "active" || next.context !== current.context)
    throw new BuyerClientError("invalid_response");
  return next;
}

async function prepare(
  store: Storage,
  phase: Journal["phase"],
  baseline: string | null,
): Promise<SessionStatus> {
  const marker: Journal = { v: 1, id: crypto.randomUUID(), baseline, phase };
  persist(store, marker);
  let response: Response;
  try {
    response = await request(
      "POST",
      `session/${phase}`,
      baseline ?? undefined,
      {},
    );
  } catch {
    throw new BuyerClientError("uncertain");
  }
  if (!response.ok) {
    if (await definiteError(response)) {
      clear(store, marker.id);
      throw new BuyerClientError(
        response.status === 409 ? "context_changed" : "request_failed",
        response.status,
      );
    }
    throw new BuyerClientError("uncertain");
  }
  // A success response alone is not cookie delivery proof.
  let next: SessionStatus;
  try {
    next = await status();
  } catch {
    throw new BuyerClientError("uncertain");
  }
  if (!next.context || next.context === baseline)
    throw new BuyerClientError("uncertain");
  clear(store, marker.id);
  return next;
}

export function readBuyerSession(): Promise<SessionStatus> {
  return locked(async (store) => {
    const current = await status();
    await recover(store, current);
    return current;
  });
}

export function initializeBuyerSession(): Promise<SessionStatus> {
  return locked(async (store) => {
    let current = await status();
    if (await recover(store, current)) throw new BuyerClientError("uncertain");
    if (current.state === "absent")
      current = await prepare(store, "prepare", null);
    if (current.state === "expired")
      throw new BuyerClientError("requires_reset");
    return activate(current);
  });
}

export function resetBuyerSession(
  expectedContext: string,
): Promise<SessionStatus> {
  return locked(async (store) => {
    const current = await status();
    if (await recover(store, current)) throw new BuyerClientError("uncertain");
    if (!CONTEXT.test(expectedContext) || current.context !== expectedContext)
      throw new BuyerClientError("context_changed");
    const next = await prepare(store, "reset", expectedContext);
    return activate(next);
  });
}

export function logoutBuyerSession(expectedContext: string): Promise<void> {
  return locked(async (store) => {
    const current = await status();
    if (await recover(store, current)) throw new BuyerClientError("uncertain");
    if (!CONTEXT.test(expectedContext) || current.context !== expectedContext)
      throw new BuyerClientError("context_changed");
    const response = await request(
      "POST",
      "session/logout",
      expectedContext,
      {},
    );
    if (response.status === 409)
      throw new BuyerClientError("context_changed", 409);
    if (response.status !== 204)
      throw new BuyerClientError("request_failed", response.status);
  });
}

// claimToken is the in-memory claim-link bearer: required on exactly the two claim
// routes (GET claim-link, POST claim-link/redeem) and refused everywhere else, so it can
// only ever leave the page in its own header (live-keyword-claims-v1 §7.2, §11.1).
export async function buyerRequest(
  method: string,
  suffix: string,
  context: string,
  body?: unknown,
  idempotencyKey?: string,
  claimToken?: string,
): Promise<Response> {
  if (!CONTEXT.test(context)) throw new BuyerClientError("context_changed");
  const claimRoute =
    (method === "GET" && suffix === "claim-link") ||
    (method === "POST" && suffix === "claim-link/redeem");
  if (claimRoute !== (claimToken !== undefined) || (claimToken !== undefined && !CLAIM_TOKEN.test(claimToken)))
    throw new BuyerClientError("request_failed");
  if (
    !/^(?:GET|PUT|POST)$/.test(method) ||
    !/^[A-Za-z0-9_/?=&.%-]+$/.test(suffix) ||
    suffix.includes("..") ||
    suffix.startsWith("/")
  )
    throw new BuyerClientError("request_failed");
  const handoff =
    method === "POST" && (HANDOFF.test(suffix) || CVS_VERIFY.test(suffix));
  if (method !== "GET") {
    if (journal(storage())) throw new BuyerClientError("uncertain");
    if (
      handoff
        ? body !== undefined || idempotencyKey !== undefined
        : !idempotencyKey || !KEY.test(idempotencyKey) || body === undefined
    )
      throw new BuyerClientError("request_failed");
  } else if (body !== undefined || idempotencyKey !== undefined)
    throw new BuyerClientError("request_failed");
  const response = await request(
    method,
    suffix,
    context,
    body,
    idempotencyKey,
    claimToken,
  );
  if (response.status === 409) {
    try {
      const clone = response.clone();
      const parsed: unknown = await clone.json();
      if (
        parsed &&
        typeof parsed === "object" &&
        (parsed as { code?: unknown }).code === "context_changed"
      )
        throw new BuyerClientError("context_changed", 409);
    } catch (error) {
      if (error instanceof BuyerClientError) throw error;
    }
  }
  return response;
}
