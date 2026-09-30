// Admin customers/finance/billing client: browser -> BFF `/api/stores/{store}/{customers*,finance/*,billing*}`
// -> Go `internal/httpapi/{customers,finance,billing}.go` (contract customers-billing-v1 §5). Reads parse the frozen
// DTOs (customers-model / billing-model); writes carry the cookie CSRF + session fence of settings-client and never
// trust their own response body for state (the caller re-GETs). Also owns `useGuardedRead`, the one page-level read
// lifecycle shared by the four pages: session-boundary fence, PII cleared when the tab is hidden or the session ends.
// Bearer links (checkout/portal URLs) and export bodies are handed straight to the browser and never stored or logged.
import { useCallback, useEffect, useRef, useState } from "react";
import { csrfCookie, safeError, sessionBoundary } from "./settings-client";
import {
  parseCustomerDetail,
  parseCustomerList,
  parseErasureSummary,
  parseFinanceSummary,
  type CustomerDetail,
  type CustomerList,
  type ErasureSummary,
  type FinanceSummary,
} from "./customers-model";

export type ReadCode = "signed-out" | "forbidden" | "not-found" | "unavailable";
export class ReadError extends Error {
  constructor(readonly code: ReadCode) {
    super(code);
  }
}

// Body is read only after status/type checks; `no-store` must be present so PII is never cacheable.
export async function readJSON(path: string, signal: AbortSignal): Promise<unknown> {
  let response: Response;
  try {
    response = await fetch(path, { method: "GET", cache: "no-store", credentials: "same-origin", signal });
  } catch {
    throw new ReadError("unavailable");
  }
  if (response.status === 401) throw new ReadError("signed-out");
  if (response.status === 403) throw new ReadError("forbidden");
  if (response.status === 404) throw new ReadError("not-found");
  if (!response.ok || response.headers.get("content-type")?.split(";", 1)[0] !== "application/json" ||
    !response.headers.get("cache-control")?.split(",").some((part) => part.trim() === "no-store"))
    throw new ReadError("unavailable");
  try {
    return await response.json();
  } catch {
    throw new ReadError("unavailable");
  }
}
export async function get<T>(path: string, parse: (value: unknown) => T, signal: AbortSignal): Promise<T> {
  try {
    return parse(await readJSON(path, signal));
  } catch (error) {
    if (error instanceof ReadError) throw error;
    throw new ReadError("unavailable");
  }
}

const base = (store: string) => `/api/stores/${store}`;
// Go GET customers (customers:read): q is trimmed server-side; the cursor is opaque.
export const readCustomers = (store: string, q: string, after: string, signal: AbortSignal): Promise<CustomerList> =>
  get(`${base(store)}/customers?limit=25${after ? `&after=${after}` : ""}${q ? `&q=${encodeURIComponent(q)}` : ""}`, parseCustomerList, signal);
// Go GET customers/{id} (customers:read).
export const readCustomer = (store: string, id: string, signal: AbortSignal): Promise<CustomerDetail> =>
  get(`${base(store)}/customers/${id}`, (value) => parseCustomerDetail(value, id), signal);
// Go GET finance/summary (orders:read); day range is UTC+8 finance days (Q11).
export const readFinance = (store: string, from: string, to: string, signal: AbortSignal): Promise<FinanceSummary> =>
  get(`${base(store)}/finance/summary?from=${from}&to=${to}`, parseFinanceSummary, signal);
// Go GET finance/summary.csv (orders:read + orders:export): plain GET download streamed by the BFF, never fetched into JS.
export const financeCSVHref = (store: string, from: string, to: string) =>
  `${base(store)}/finance/summary.csv?from=${from}&to=${to}`;

export type WriteOutcome<T> = { ok: true; value: T } | { ok: false; code: string; uncertain: boolean };
// One fenced POST. `key` only on the three customer POSTs (contract §6); a 5xx or lost answer is `uncertain`.
export async function post(
  store: string,
  resource: string,
  options: { key?: string; body?: string; boundary: string; timeoutMs?: number },
): Promise<{ response: Response } | { ok: false; code: string; uncertain: boolean }> {
  const csrf = csrfCookie();
  try {
    // Recheck directly before the network write, like writeSettings: a changed session sends nothing.
    if (!csrf || (await sessionBoundary(csrf)) !== options.boundary || csrfCookie() !== csrf)
      return { ok: false, code: "unauthorized", uncertain: false };
  } catch {
    return { ok: false, code: "unauthorized", uncertain: false };
  }
  const headers: Record<string, string> = { "Content-Type": "application/json", "X-CSRF-Token": csrf };
  if (options.key) headers["Idempotency-Key"] = options.key;
  try {
    const response = await fetch(`${base(store)}/${resource}`, {
      method: "POST",
      credentials: "same-origin",
      cache: "no-store",
      headers,
      ...(options.body === undefined ? {} : { body: options.body }),
      signal: AbortSignal.timeout(options.timeoutMs ?? 12000),
    });
    return { response };
  } catch {
    return { ok: false, code: "retry_later", uncertain: true };
  }
}
async function failure(response: Response): Promise<{ ok: false; code: string; uncertain: boolean }> {
  const value: unknown = await response.json().catch(() => null);
  return { ok: false, code: safeError(value).code, uncertain: response.status >= 500 };
}
async function postJSON<T>(
  store: string,
  resource: string,
  options: { key?: string; body?: string; boundary: string; timeoutMs?: number },
  parse: (value: unknown) => T,
): Promise<WriteOutcome<T>> {
  const sent = await post(store, resource, options);
  if (!("response" in sent)) return sent;
  if (!sent.response.ok) return failure(sent.response);
  try {
    return { ok: true, value: parse(await sent.response.json()) };
  } catch {
    // Committed on the server but unreadable here: same key replays the stored answer, so retrying is safe.
    return { ok: false, code: "retry_later", uncertain: true };
  }
}

// Go POST customers/{id}/consent-withdrawals (customers:privacy; body exactly purpose/channel; 201).
export const postWithdrawal = (store: string, id: string, key: string, body: string, boundary: string) =>
  postJSON(store, `customers/${id}/consent-withdrawals`, { key, body, boundary }, () => true);
// Go POST customers/{id}/erasure (customers:privacy; body exactly {"confirm":"ERASE"}; 200 counts, 409 erasure_blocked).
export const postErasure = (store: string, id: string, key: string, boundary: string): Promise<WriteOutcome<ErasureSummary>> =>
  postJSON(store, `customers/${id}/erasure`, { key, body: JSON.stringify({ confirm: "ERASE" }), boundary }, parseErasureSummary);

// Go POST customers/{id}/exports (customers:privacy; 200 attachment lc.customer-export.v1 <= 1 MiB, 409 export_too_large).
// The body is PII: it becomes a Blob download and the object URL is revoked right after the click. Same key on retry
// replays the stored export, so an uncertain result is retried with the same key by the caller.
export async function postExport(store: string, id: string, key: string, boundary: string): Promise<WriteOutcome<true>> {
  const sent = await post(store, `customers/${id}/exports`, { key, boundary, timeoutMs: 30000 });
  if (!("response" in sent)) return sent;
  const { response } = sent;
  if (!response.ok) return failure(response);
  if (response.headers.get("content-type")?.split(";", 1)[0] !== "application/json") return { ok: false, code: "retry_later", uncertain: true };
  try {
    const blob = await response.blob();
    const href = URL.createObjectURL(blob);
    const link = document.createElement("a");
    link.href = href;
    link.download = `customer-${id.slice(0, 8)}.json`;
    link.rel = "noopener";
    document.body.append(link);
    link.click();
    link.remove();
    setTimeout(() => URL.revokeObjectURL(href), 1000);
    return { ok: true, value: true };
  } catch {
    return { ok: false, code: "retry_later", uncertain: true };
  }
}

// useGuardedRead: one read per (scope, tick). The result is dropped, and the loader re-run on return, whenever the tab
// is hidden (bfcache must not keep PII), the session cookie changes, or another tab signs out. `boundary` is the session
// fence of the last accepted read; writes pass it to `post` so they refuse to run under a different session.
export type ReadStatus = "loading" | "ready" | "hidden" | ReadCode;
export function useGuardedRead<T>(
  scope: string,
  run: ((signal: AbortSignal) => Promise<T>) | null,
  initialError: ReadCode | null,
) {
  const [tick, setTick] = useState(0);
  const key = `${scope}|${initialError ?? ""}|${tick}`;
  const [view, setView] = useState<{ key: string; status: ReadStatus; data: T | null }>({ key: "", status: "loading", data: null });
  const latest = useRef(run);
  latest.current = run;
  const generation = useRef(0);
  const controller = useRef<AbortController | null>(null);
  const hidden = useRef(false);
  const blocked = useRef(false);
  const boundary = useRef("");

  const load = useCallback(async () => {
    if (hidden.current || blocked.current) return;
    if (initialError || !latest.current) {
      setView({ key, status: initialError ?? "not-found", data: null });
      return;
    }
    const epoch = ++generation.current;
    controller.current?.abort();
    const active = new AbortController();
    controller.current = active;
    setView({ key, status: "loading", data: null });
    const live = () => generation.current === epoch && !active.signal.aborted && !hidden.current;
    try {
      const before = await sessionBoundary();
      if (!live()) return;
      const data = await latest.current!(active.signal);
      // The cookie is a change fence, not authority: the BFF just authorized this read.
      if (!live() || (await sessionBoundary()) !== before) throw new ReadError("signed-out");
      if (!live()) return;
      boundary.current = before;
      setView({ key, status: "ready", data });
    } catch (error) {
      if (!live()) return;
      const code: ReadCode = error instanceof ReadError ? error.code
        : error instanceof Error && error.message === "session_changed" ? "signed-out" : "unavailable";
      if (code === "signed-out") blocked.current = true;
      boundary.current = "";
      setView({ key, status: code, data: null });
    }
  }, [key, initialError]);

  useEffect(() => {
    void load();
    return () => {
      generation.current++;
      controller.current?.abort();
    };
  }, [load]);

  useEffect(() => {
    const clear = (status: ReadStatus, block = false) => {
      generation.current++;
      controller.current?.abort();
      boundary.current = "";
      if (block) blocked.current = true;
      setView({ key, status, data: null });
    };
    const conceal = () => {
      hidden.current = true;
      clear("hidden");
    };
    const reveal = () => {
      if (!hidden.current) return;
      hidden.current = false;
      if (!blocked.current) void load();
    };
    const visibility = () => (document.visibilityState === "hidden" ? conceal() : reveal());
    const onMessage = (event: MessageEvent) => {
      if (event.data?.type === "logout") clear("signed-out", true);
    };
    const onStorage = (event: StorageEvent) => {
      if (event.key === "commerce-session-logout") clear("signed-out", true);
    };
    const onLogout = () => clear("signed-out", true);
    const onFocus = () => {
      if (hidden.current) return reveal();
      if (!boundary.current) return;
      void sessionBoundary()
        .then((value) => {
          if (boundary.current && value !== boundary.current) clear("signed-out", true);
        })
        .catch(() => clear("signed-out", true));
    };
    let channel: BroadcastChannel | null = null;
    try {
      channel = new BroadcastChannel("commerce-session");
      channel.addEventListener("message", onMessage);
    } catch {
      /* storage event still works */
    }
    document.addEventListener("visibilitychange", visibility);
    window.addEventListener("pagehide", conceal);
    window.addEventListener("pageshow", reveal);
    window.addEventListener("storage", onStorage);
    window.addEventListener("commerce-session-logout", onLogout);
    window.addEventListener("focus", onFocus);
    if (document.visibilityState === "hidden") conceal();
    return () => {
      document.removeEventListener("visibilitychange", visibility);
      window.removeEventListener("pagehide", conceal);
      window.removeEventListener("pageshow", reveal);
      window.removeEventListener("storage", onStorage);
      window.removeEventListener("commerce-session-logout", onLogout);
      window.removeEventListener("focus", onFocus);
      channel?.removeEventListener("message", onMessage);
      channel?.close();
    };
  }, [key, load]);

  const current = view.key === key ? view : { key, status: "loading" as ReadStatus, data: null as T | null };
  const reload = useCallback(() => {
    blocked.current = false;
    setTick((value) => value + 1);
  }, []);
  // After a write: re-GET in place (no loading flash, so an open dialog/result message survives). Server state only.
  const refresh = useCallback(async () => {
    if (hidden.current || blocked.current || !latest.current || !boundary.current) return false;
    const epoch = generation.current;
    const before = boundary.current;
    const signal = controller.current?.signal ?? new AbortController().signal;
    try {
      const data = await latest.current(signal);
      if (generation.current !== epoch || hidden.current || signal.aborted || (await sessionBoundary()) !== before) return false;
      setView((previous) => (previous.key === key ? { key, status: "ready", data } : previous));
      return true;
    } catch {
      return false;
    }
  }, [key]);
  return { status: current.status, data: current.data, boundary: boundary.current, reload, refresh };
}
