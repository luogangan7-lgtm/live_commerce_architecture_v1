// BFF request grammar for customers, finance and billing (customers-billing-v1 §5):
// BFF `/api/stores/{store}/{customers*,finance/*,billing*}` -> Go `internal/httpapi/{customers,finance,billing}.go`
// (customers-core / billing-core units). No generic proxying: every resource is listed here and nowhere else.
// The integrator's route.ts calls `customersRoute` to admit a resource and `validCustomersRequest` /
// `validCustomersBody` before forwarding; Go re-validates everything (this is a fence, not the authority).
const uuid = "[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}";

export type CustomersRouteKind =
  | "list"
  | "detail"
  | "withdraw"
  | "export"
  | "erase"
  | "finance"
  | "finance-csv"
  | "billing"
  | "standing"
  | "checkout"
  | "portal";

const routes: [string, RegExp, CustomersRouteKind][] = [
  ["GET", /^customers$/, "list"],
  ["GET", new RegExp(`^customers/${uuid}$`), "detail"],
  ["POST", new RegExp(`^customers/${uuid}/consent-withdrawals$`), "withdraw"],
  ["POST", new RegExp(`^customers/${uuid}/exports$`), "export"],
  ["POST", new RegExp(`^customers/${uuid}/erasure$`), "erase"],
  ["GET", /^finance\/summary$/, "finance"],
  ["GET", /^finance\/summary\.csv$/, "finance-csv"],
  ["GET", /^billing$/, "billing"],
  ["GET", /^billing\/standing$/, "standing"],
  ["POST", /^billing\/checkout$/, "checkout"],
  ["POST", /^billing\/portal$/, "portal"],
];
export function customersRoute(method: string, path: string): CustomersRouteKind | null {
  return routes.find(([m, re]) => m === method && re.test(path))?.[2] ?? null;
}

// Idempotency-Key is required on the three customer POSTs and forbidden on every other route
// (billing POSTs are keyless: Stripe params carry a wall-clock expiry, contract §5 step 3 / §6).
const keyed: readonly CustomersRouteKind[] = ["withdraw", "export", "erase"];
// A JSON body is required only where Go reads one; export and portal carry none.
const withBody: readonly CustomersRouteKind[] = ["withdraw", "erase", "checkout"];
const keyPattern = /^[A-Za-z0-9_.:-]{8,128}$/;

const day = /^(\d{4})-(\d{2})-(\d{2})$/;
// Calendar-valid YYYY-MM-DD as UTC days since the epoch, or null.
function dayNumber(value: string): number | null {
  const match = day.exec(value);
  if (!match) return null;
  const parsed = Date.UTC(+match[1], +match[2] - 1, +match[3]);
  const back = new Date(parsed);
  return back.getUTCFullYear() === +match[1] && back.getUTCMonth() === +match[2] - 1 && back.getUTCDate() === +match[3]
    ? parsed / 86_400_000
    : null;
}

// Inspect the raw URL before Next.js drops empty search strings like a bare '?'.
// list: limit 1..100, after (opaque cursor), q 1..40 code points; finance: from/to required, D13 range 0..91 days;
// every other resource is exact (no query at all).
export function validCustomersQuery(kind: CustomersRouteKind, rawURL: string): boolean {
  const at = rawURL.indexOf("?");
  if (at < 0) return kind !== "finance" && kind !== "finance-csv";
  if (kind !== "list" && kind !== "finance" && kind !== "finance-csv") return false;
  const seen = new Map<string, string>();
  for (const segment of rawURL.slice(at + 1).split("&")) {
    const match = /^(limit|after|q|from|to)=([^=#+]*)$/.exec(segment);
    if (!match || seen.has(match[1])) return false;
    seen.set(match[1], match[2]);
  }
  if (kind === "list") {
    const limit = seen.get("limit");
    const after = seen.get("after");
    const q = seen.get("q");
    if (seen.has("from") || seen.has("to")) return false;
    if (limit !== undefined && !/^(?:[1-9]|[1-9][0-9]|100)$/.test(limit)) return false;
    if (after !== undefined && !/^[A-Za-z0-9_-]{1,1024}$/.test(after)) return false;
    if (q !== undefined) {
      let text: string;
      try {
        text = decodeURIComponent(q);
      } catch {
        return false;
      }
      // Server trims, so blank is refused here too; control characters and lone surrogates never reach Go.
      if (text.trim() === "" || Array.from(text).length > 40 || /[\p{C}\p{Zl}\p{Zp}]/u.test(text)) return false;
    }
    return true;
  }
  if (seen.size !== 2 || !seen.has("from") || !seen.has("to")) return false;
  const from = dayNumber(seen.get("from")!);
  const to = dayNumber(seen.get("to")!);
  return from !== null && to !== null && to - from >= 0 && to - from <= 91;
}

// Headers/body presence fence. Body *content* is checked by validCustomersBody after the BFF reads it.
export function validCustomersRequest(kind: CustomersRouteKind, request: Request): boolean {
  if (!validCustomersQuery(kind, request.url)) return false;
  const key = request.headers.get("idempotency-key");
  if (keyed.includes(kind) ? key === null || !keyPattern.test(key) : key !== null) return false;
  const length = request.headers.get("content-length");
  if (request.headers.has("transfer-encoding")) return false;
  if (withBody.includes(kind)) {
    return request.headers.get("content-type")?.split(";", 1)[0].trim().toLowerCase() === "application/json";
  }
  // GET has no body by construction; keyless POSTs (export, portal) must declare an empty one.
  return (request.method === "GET" ? request.body === null : true) && (length === null || length === "0");
}

export const consentPairs = [
  ["marketing_messages", "meta_dm"],
  ["ads_personalization", "meta_ads"],
] as const;
export const checkoutPrice = /^price_[A-Za-z0-9]{1,64}$/;

// Exact bodies (D12/D8/checkout frozen shapes): unknown or missing key = invalid; export/portal must be empty.
export function validCustomersBody(kind: CustomersRouteKind, text: string): boolean {
  if (!withBody.includes(kind)) return text === "";
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch {
    return false;
  }
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const body = value as Record<string, unknown>;
  const keys = Object.keys(body).sort().join(",");
  if (kind === "erase") return keys === "confirm" && body.confirm === "ERASE";
  if (kind === "checkout") return keys === "price_id" && typeof body.price_id === "string" && checkoutPrice.test(body.price_id);
  return (
    keys === "channel,purpose" &&
    consentPairs.some(([purpose, channel]) => body.purpose === purpose && body.channel === channel)
  );
}
