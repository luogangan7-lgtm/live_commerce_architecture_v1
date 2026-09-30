// Admin ads model: strict parsers for the frozen ads DTOs plus the pure money/date/draft-input rules.
// Read by components/Ads.tsx and lib/ads-client.ts; BFF `/api/stores/{store}/ads/*`
// -> Go `internal/httpapi/ads.go` (contracts/meta-ads-v1.md §5.1, §5.2, §7; docs/delivery/units/ads-core.md "Frozen HTTP").
// Non-goals: no fetching, no auth, no domain authority. Client validation is only a hint that saves a round
// trip; Go re-runs every rule (§5.2) and its code wins. An unknown enum is a parse error, never guessed.
// Depends on nothing so `node --test --experimental-strip-types` can import it (tests/admin/ads-model.test.ts).

export class AdsParseError extends Error {
  constructor(what: string) {
    super(`ads_parse: ${what}`);
  }
}

// §5.1 derived status; display-only (no job selects by it, but the UI mirrors the same names).
export const draftStatuses = [
  "DRAFT", "APPROVED", "SUBMITTING", "REMOTE_PAUSED", "ACTIVE", "PAUSED", "UNKNOWN", "FAILED", "ENDED", "REJECTED",
] as const;
export type DraftStatus = (typeof draftStatuses)[number];
export const templates = ["BOOST_POST", "PRODUCT_TRAFFIC"] as const;
export type Template = (typeof templates)[number];
export const environments = ["SANDBOX", "LIVE"] as const;
export type Environment = (typeof environments)[number];
// integration.operations.state CHECK (migrations/0008:45); shown raw-by-enum in the op list.
export const opStates = [
  "READY", "DISPATCHING", "UNKNOWN", "ACKNOWLEDGED", "SUCCEEDED", "FAILED_FINAL", "CANCELLED", "BLOCKED_POLICY", "STALE_BINDING",
] as const;
export type OpState = (typeof opStates)[number];

// The Frozen HTTP error codes (ads-core.md) plus the codes the BFF/session layer adds. Copy must cover all of them.
export const adsServerCodes = [
  "state_mismatch", "state_expired", "meta_connect_failed", "not_in_pick_list", "client_business_changed",
  "revision_changed", "over_allowance", "billing_restricted", "attempt_changed", "prior_attempt_not_paused",
  "draft_approved", "budget_below_minimum", "not_whole_unit", "currency_mismatch", "starts_too_soon",
  "binding_disabled", "source_not_owned", "product_not_published", "forbidden", "not_found", "invalid_request",
] as const;
export const adsSessionCodes = [
  "unauthorized", "retry_later", "rate_limited", "invalid_json", "json_required", "method_not_allowed", "conflict",
] as const;
// Client-side hint codes (never sent to the server as a code; they name a local rule that failed).
export const adsLocalCodes = [
  "budget_invalid", "unsupported_currency", "ends_before_start", "duration_too_long", "age_invalid",
  "countries_invalid", "source_invalid", "bindings_missing", "dates_invalid",
] as const;
export const adsCodes = [...adsServerCodes, ...adsSessionCodes, ...adsLocalCodes] as const;
export type AdsCode = (typeof adsCodes)[number];
// Fixed `connect_error` values the callback BFF may put in the URL (U2); `denied` = the merchant cancelled on Meta.
export const connectErrors = [
  "state_mismatch", "state_expired", "meta_connect_failed", "denied", "invalid_request", "forbidden", "unavailable",
] as const;
export type ConnectError = (typeof connectErrors)[number];

export const canonicalUUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const timestamp = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})$/;
const dateOnly = /^\d{4}-\d{2}-\d{2}$/;
const code = /^[a-z0-9_]{1,64}$/;
const maxMinor = 1_000_000_000_000;

// ---------- small strict readers ----------
type Rec = Record<string, unknown>;
function rec(value: unknown, what: string): Rec {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new AdsParseError(what);
  return value as Rec;
}
function str(o: Rec, key: string, max = 256, min = 1): string {
  const v = o[key];
  if (typeof v !== "string" || v.length < min || v.length > max || /[\u0000-\u001f\u007f]/.test(v)) throw new AdsParseError(key);
  return v;
}
function optStr(o: Rec, key: string, max = 256): string | null {
  const v = o[key];
  if (v === undefined || v === null || v === "") return null;
  return str(o, key, max);
}
function int(o: Rec, key: string, min = 0, max = maxMinor): number {
  const v = o[key];
  if (typeof v !== "number" || !Number.isSafeInteger(v) || v < min || v > max) throw new AdsParseError(key);
  return v;
}
function bool(o: Rec, key: string): boolean {
  if (typeof o[key] !== "boolean") throw new AdsParseError(key);
  return o[key] as boolean;
}
function ts(o: Rec, key: string): string {
  const v = str(o, key, 40);
  if (!timestamp.test(v) || !Number.isFinite(Date.parse(v))) throw new AdsParseError(key);
  return v;
}
function optTs(o: Rec, key: string): string | null {
  const v = o[key];
  return v === undefined || v === null || v === "" ? null : ts(o, key);
}
function uuid(o: Rec, key: string): string {
  const v = str(o, key, 36);
  if (!canonicalUUID.test(v)) throw new AdsParseError(key);
  return v;
}
function oneOf<T extends string>(o: Rec, key: string, allowed: readonly T[]): T {
  const v = o[key];
  if (typeof v !== "string" || !allowed.includes(v as T)) throw new AdsParseError(`${key}:enum`);
  return v as T;
}
function list<T>(value: unknown, what: string, max: number, each: (item: unknown) => T): T[] {
  if (!Array.isArray(value) || value.length > max) throw new AdsParseError(what);
  return value.map(each);
}
function currencyCode(o: Rec, key: string): string {
  const v = str(o, key, 3, 3);
  if (!/^[A-Z]{3}$/.test(v)) throw new AdsParseError(key);
  return v;
}

// ---------- error body ----------
// Go answers `{"code":"<code>",...}` (httperror.Write; the brief's `{"error":...}` was wrong); the BFF passes it through.
// Both shapes are read so a hook gap degrades to `retry_later`, never to a guessed code.
export function adsErrorCode(body: unknown): AdsCode {
  const o = body && typeof body === "object" ? (body as Rec) : {};
  const raw = typeof o.code === "string" ? o.code : typeof o.error === "string" ? o.error : "";
  return code.test(raw) && (adsCodes as readonly string[]).includes(raw) ? (raw as AdsCode) : "retry_later";
}

// ---------- settings (D5 GET ads/settings) ----------
export type Connection = {
  binding_id: string; provider: string; asset_id: string; client_business_id: string; enabled: boolean; connected_at: string;
};
export type Identity = { binding_id: string; provider: string; asset_id: string };
export type Settings = {
  environment: Environment;
  allowance_currency: string;
  max_active_budget_minor: number;
  sandbox_ad_account: string | null;
  capi: { enabled: boolean; dataset_binding_id: string | null; test_event_code: string | null };
  connections: Connection[];
  identities: Identity[];
};
const providerName = /^[a-z][a-z0-9_]{0,39}$/;
export function parseSettings(value: unknown): Settings {
  const o = rec(value, "settings");
  const capi = rec(o.capi, "capi");
  const allowanceCurrency = o.allowance_currency === null || o.allowance_currency === "" || o.allowance_currency === undefined
    ? "" : currencyCode(o, "allowance_currency");
  return {
    environment: oneOf(o, "environment", environments),
    allowance_currency: allowanceCurrency,
    max_active_budget_minor: int(o, "max_active_budget_minor"),
    sandbox_ad_account: optStr(o, "sandbox_ad_account", 64),
    capi: {
      enabled: bool(capi, "enabled"),
      dataset_binding_id: capi.dataset_binding_id === undefined || capi.dataset_binding_id === null ? null : uuid(capi, "dataset_binding_id"),
      test_event_code: optStr(capi, "test_event_code", 64),
    },
    connections: list(o.connections, "connections", 100, (item) => {
      const c = rec(item, "connection");
      const provider = str(c, "provider", 40);
      if (!providerName.test(provider)) throw new AdsParseError("provider");
      return {
        binding_id: uuid(c, "binding_id"), provider, asset_id: str(c, "asset_id", 64),
        client_business_id: str(c, "client_business_id", 64), enabled: bool(c, "enabled"), connected_at: ts(c, "connected_at"),
      };
    }),
    identities: list(o.identities, "identities", 100, (item) => {
      const c = rec(item, "identity");
      const provider = str(c, "provider", 40);
      if (!providerName.test(provider)) throw new AdsParseError("provider");
      return { binding_id: uuid(c, "binding_id"), provider, asset_id: str(c, "asset_id", 64) };
    }),
  };
}

// ---------- connect state + pick list (D5 GET ads/meta/states/{id}) ----------
export type PickItem = {
  kind: "ad_account" | "dataset"; id: string; name: string; currency: string; timezone: string; account_status: number;
};
export type ConnectState = { state_id: string; expires_at: string; client_business_id: string; picks: PickItem[] };
export function parseConnectState(value: unknown, expectedID: string): ConnectState {
  const o = rec(value, "state");
  const state_id = uuid(o, "state_id");
  if (state_id !== expectedID) throw new AdsParseError("state_id:mismatch");
  return {
    state_id,
    expires_at: ts(o, "expires_at"),
    client_business_id: str(o, "client_business_id", 64),
    picks: list(o.picks, "picks", 500, (item) => {
      const p = rec(item, "pick");
      return {
        kind: oneOf(p, "kind", ["ad_account", "dataset"] as const),
        id: str(p, "id", 64),
        name: str(p, "name", 200, 0),
        currency: p.currency === undefined || p.currency === "" ? "" : currencyCode(p, "currency"),
        timezone: optStr(p, "timezone", 64) ?? "",
        account_status: p.account_status === undefined ? 0 : int(p, "account_status", 0, 1000),
      };
    }),
  };
}

// ---------- drafts ----------
export type Remote = { campaign_id: string | null; adset_id: string | null; creative_id: string | null; ad_id: string | null };
export type Op = { kind: string; seq: number; attempt: number; state: OpState; code: string | null; updated_at: string };
export type DraftInput = {
  ad_binding_id: string; identity_binding_id: string; template: Template; source_ref: string; currency: string;
  lifetime_budget_minor: number; starts_at: string; ends_at: string; countries: string[]; age_min: number; age_max: number;
};
export type Draft = DraftInput & {
  id: string; revision: number; publish_attempt: number; status: DraftStatus; approved_revision: number | null;
  ended_at: string | null; created_at: string; remote: Remote; ops: Op[]; ads_manager_url: string | null;
};
const remoteID = /^[A-Za-z0-9_-]{1,64}$/;
function remoteField(o: Rec, key: string): string | null {
  const v = o[key];
  if (v === undefined || v === null || v === "") return null;
  if (typeof v !== "string" || !remoteID.test(v)) throw new AdsParseError(key);
  return v;
}
export function parseDraft(value: unknown): Draft {
  const o = rec(value, "draft");
  const remote = rec(o.remote ?? {}, "remote");
  const countries = list(o.countries, "countries", 50, (item) => {
    if (typeof item !== "string" || !/^[A-Z]{2}$/.test(item)) throw new AdsParseError("countries");
    return item;
  });
  const ageMin = int(o, "age_min", 13, 65);
  const ageMax = int(o, "age_max", 13, 65);
  if (countries.length === 0 || ageMin > ageMax) throw new AdsParseError("targeting");
  const url = optStr(o, "ads_manager_url", 2048);
  return {
    id: uuid(o, "id"),
    revision: int(o, "revision", 0, 1_000_000_000),
    publish_attempt: int(o, "publish_attempt", 0, 1000),
    status: oneOf(o, "status", draftStatuses),
    approved_revision: o.approved_revision === undefined || o.approved_revision === null ? null : int(o, "approved_revision", 0, 1_000_000_000),
    ended_at: optTs(o, "ended_at"),
    created_at: ts(o, "created_at"),
    ad_binding_id: uuid(o, "ad_binding_id"),
    identity_binding_id: uuid(o, "identity_binding_id"),
    template: oneOf(o, "template", templates),
    source_ref: str(o, "source_ref", 128),
    currency: currencyCode(o, "currency"),
    lifetime_budget_minor: int(o, "lifetime_budget_minor"),
    starts_at: ts(o, "starts_at"),
    ends_at: ts(o, "ends_at"),
    countries, age_min: ageMin, age_max: ageMax,
    remote: {
      campaign_id: remoteField(remote, "campaign_id"), adset_id: remoteField(remote, "adset_id"),
      creative_id: remoteField(remote, "creative_id"), ad_id: remoteField(remote, "ad_id"),
    },
    ops: list(o.ops ?? [], "ops", 200, (item) => {
      const p = rec(item, "op");
      const c = optStr(p, "code", 64);
      if (c !== null && !code.test(c)) throw new AdsParseError("op.code");
      const kind = str(p, "kind", 64);
      if (!/^[a-z][a-z0-9_.]{0,63}$/.test(kind)) throw new AdsParseError("op.kind");
      return {
        kind, seq: int(p, "seq", 0, 1000), attempt: int(p, "attempt", 0, 1000),
        state: oneOf(p, "state", opStates), code: c, updated_at: ts(p, "updated_at"),
      };
    }),
    ads_manager_url: url !== null && adsManagerHref(url) !== null ? url : null,
  };
}
export function parseDraftList(value: unknown): Draft[] {
  const o = rec(value, "drafts");
  return list(o.items, "items", 200, parseDraft);
}

// The Ads Manager deep link is server-provided; only an https link on a Meta host is ever rendered as a link.
export function adsManagerHref(value: string): string | null {
  try {
    const url = new URL(value);
    if (url.protocol !== "https:" || url.username || url.password || url.port ||
      !(url.hostname === "facebook.com" || url.hostname.endsWith(".facebook.com"))) return null;
    return url.toString();
  } catch {
    return null;
  }
}
export function hasRemote(d: Pick<Draft, "remote">): boolean {
  const r = d.remote;
  return !!(r.campaign_id || r.adset_id || r.creative_id || r.ad_id);
}
// UI hints only (the server re-authorizes and re-validates every action).
export const canEdit = (d: Draft) => d.status === "DRAFT";
export const canApprove = (d: Draft) => d.status === "DRAFT";
export const canPublish = (d: Draft) => d.status === "APPROVED" || d.status === "FAILED";
export const canPause = (d: Draft) => hasRemote(d) && d.status !== "ENDED";
export const canEnd = (d: Draft) => d.status !== "DRAFT" && d.status !== "ENDED";
// X7: a paused draft never re-activates; the only way forward is a copy into a brand-new draft.
export const canCopy = (d: Draft) => (["PAUSED", "REMOTE_PAUSED", "ENDED", "FAILED", "REJECTED"] as DraftStatus[]).includes(d.status);

// ---------- report (GET ads/report) ----------
export type Report = {
  window: { from: string; to: string };
  timezone: string;
  orders: null | {
    captured_minor: number; refunded_minor: number; net_minor: number; currency: string; note: string | null; fetched_at: string | null;
  };
  meta_delivery: {
    spend_minor: number; impressions: number; clicks: number; currency: string; account_timezone: string;
    fetched_at: string | null; final_through: string | null;
  };
  meta_reported: { purchases: number; purchase_value_minor: number; currency: string; fetched_at: string | null };
};
export function parseReport(value: unknown): Report {
  const o = rec(value, "report");
  const w = rec(o.window, "window");
  const from = str(w, "from", 10, 10);
  const to = str(w, "to", 10, 10);
  if (!validDate(from) || !validDate(to)) throw new AdsParseError("window");
  const orders = o.orders === null || o.orders === undefined ? null : (() => {
    const p = rec(o.orders, "orders");
    const note = optStr(p, "note", 64);
    if (note !== null && !code.test(note)) throw new AdsParseError("orders.note");
    return {
      captured_minor: int(p, "captured_minor"), refunded_minor: int(p, "refunded_minor"),
      // net = captured - refunded and may only go negative through server bugs; refuse rather than display it.
      net_minor: int(p, "net_minor", -maxMinor), currency: currencyCode(p, "currency"), note, fetched_at: optTs(p, "fetched_at"),
    };
  })();
  const d = rec(o.meta_delivery, "meta_delivery");
  const r = rec(o.meta_reported, "meta_reported");
  const through = optStr(d, "final_through", 10);
  if (through !== null && !validDate(through)) throw new AdsParseError("final_through");
  return {
    window: { from, to },
    timezone: str(o, "timezone", 64),
    orders,
    meta_delivery: {
      spend_minor: int(d, "spend_minor"), impressions: int(d, "impressions"), clicks: int(d, "clicks"),
      currency: currencyCode(d, "currency"), account_timezone: str(d, "account_timezone", 64), fetched_at: optTs(d, "fetched_at"),
      final_through: through,
    },
    meta_reported: {
      purchases: int(r, "purchases"), purchase_value_minor: int(r, "purchase_value_minor"), currency: currencyCode(r, "currency"),
      fetched_at: optTs(r, "fetched_at"),
    },
  };
}

// ---------- money (I05: integer math only, never a float on the way to the server) ----------
export const budgetCurrencies = ["TWD", "USD", "HKD"] as const;
export type MoneyResult = { ok: true; minor: number } | { ok: false; code: "budget_invalid" | "not_whole_unit" | "budget_below_minimum" | "unsupported_currency" };
/**
 * Whole-currency-unit text -> minor units (x100). TWD must be a whole NT$ (Meta offset 1, §3: "% 100 == 0");
 * "300.00" is whole, "300.5" is not. USD/HKD accept up to two decimals. No separators, signs, exponents or
 * non-ASCII digits. Zero is below any minimum.
 */
export function wholeToMinor(currency: string, text: string): MoneyResult {
  if (!(budgetCurrencies as readonly string[]).includes(currency)) return { ok: false, code: "unsupported_currency" };
  const m = /^([0-9]{1,10})(?:\.([0-9]+))?$/.exec(text.trim());
  if (!m) return { ok: false, code: "budget_invalid" };
  const whole = Number(m[1]);
  const frac = m[2] ?? "";
  let cents = 0;
  if (currency === "TWD") {
    if (/[1-9]/.test(frac)) return { ok: false, code: "not_whole_unit" };
  } else {
    if (frac.length > 2) return { ok: false, code: "budget_invalid" };
    cents = Number(frac.padEnd(2, "0"));
  }
  const minor = whole * 100 + cents;
  return minor <= 0 ? { ok: false, code: "budget_below_minimum" } : { ok: true, minor };
}
export function minorToWhole(minor: number): string {
  const whole = Math.floor(minor / 100);
  const rest = minor % 100;
  return rest === 0 ? String(whole) : `${whole}.${String(rest).padStart(2, "0")}`;
}
// Display only. Every ads currency here (TWD/USD/HKD) is x100 minor (contract §3), unlike lib/client.ts money().
export function formatMinor(locale: string, currency: string, minor: number): string {
  try {
    const digits = minor % 100 === 0 ? 0 : 2; // whole amounts read "NT$300", fractional ones "US$12.50" (never "12.5")
    return new Intl.NumberFormat(locale, { style: "currency", currency, minimumFractionDigits: digits, maximumFractionDigits: digits }).format(minor / 100);
  } catch {
    return `${minorToWhole(minor)} ${currency}`;
  }
}

// ---------- dates ----------
export function validDate(value: string): boolean {
  if (!dateOnly.test(value)) return false;
  const [y, m, d] = value.split("-").map(Number);
  const t = new Date(Date.UTC(y, m - 1, d));
  return t.getUTCFullYear() === y && t.getUTCMonth() === m - 1 && t.getUTCDate() === d;
}
export const dayMs = 86_400_000;
export function daysBetween(from: string, to: string): number {
  return Math.round((Date.parse(`${to}T00:00:00Z`) - Date.parse(`${from}T00:00:00Z`)) / dayMs);
}
// §7 report window: YYYY-MM-DD, from <= to, at most 92 days (inclusive count).
export const maxReportDays = 92;
export function validReportWindow(from: string, to: string): boolean {
  if (!validDate(from) || !validDate(to)) return false;
  const span = daysBetween(from, to);
  return span >= 0 && span + 1 <= maxReportDays;
}
export function localDate(ms: number): string {
  const d = new Date(ms);
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
}
/** `<input type="datetime-local">` value (local wall time, minute precision) -> epoch ms, or null. */
export function localToEpoch(value: string): number | null {
  const m = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})$/.exec(value);
  if (!m) return null;
  const [y, mo, d, h, mi] = m.slice(1).map(Number);
  const t = new Date(y, mo - 1, d, h, mi, 0, 0);
  const ok = t.getFullYear() === y && t.getMonth() === mo - 1 && t.getDate() === d && t.getHours() === h && t.getMinutes() === mi;
  return ok ? t.getTime() : null;
}
export const isoSeconds = (ms: number) => new Date(ms).toISOString().replace(/\.\d{3}Z$/, "Z");
export function epochToLocal(ms: number): string {
  const d = new Date(ms);
  const p = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
}

// ---------- draft form (U4) ----------
export const minLeadMs = 10 * 60_000; // §5.2 starts_at >= now + 10 min
export const maxRunDays = 30; // §2 step 4 end <= start + 30 d
export type DraftForm = {
  ad_binding_id: string; identity_binding_id: string; template: Template; source_ref: string; currency: string;
  budget: string; starts_local: string; ends_local: string; countries: string; age_min: string; age_max: string;
};
export const emptyForm = (currency: string): DraftForm => ({
  ad_binding_id: "", identity_binding_id: "", template: "BOOST_POST", source_ref: "", currency,
  budget: "", starts_local: "", ends_local: "", countries: "TW", age_min: "18", age_max: "65",
});
export function formFromDraft(d: Draft): DraftForm {
  return {
    ad_binding_id: d.ad_binding_id, identity_binding_id: d.identity_binding_id, template: d.template, source_ref: d.source_ref,
    currency: d.currency, budget: minorToWhole(d.lifetime_budget_minor), starts_local: epochToLocal(Date.parse(d.starts_at)),
    ends_local: epochToLocal(Date.parse(d.ends_at)), countries: d.countries.join(","), age_min: String(d.age_min), age_max: String(d.age_max),
  };
}
/** X7 copy: same content, blank schedule (the old dates are past or stale), never the old id or revision. */
export function copyForm(d: Draft): DraftForm {
  return { ...formFromDraft(d), starts_local: "", ends_local: "" };
}
// BOOST_POST source: `<page_id>_<post_id>` (FB) or an Instagram media id; PRODUCT_TRAFFIC: a product id. Pattern only (§5.2 server decides).
const boostSource = /^(?:[0-9]{5,30}_[0-9]{5,30}|[0-9]{5,30})$/;
const productSource = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$/;
export function validSource(template: Template, value: string): boolean {
  return (template === "BOOST_POST" ? boostSource : productSource).test(value.trim());
}
export function parseCountries(text: string): string[] | null {
  const parts = text.split(",").map((s) => s.trim().toUpperCase()).filter(Boolean);
  if (parts.length === 0 || parts.length > 25 || new Set(parts).size !== parts.length || parts.some((p) => !/^[A-Z]{2}$/.test(p))) return null;
  return parts;
}
export type FormResult = { ok: true; input: DraftInput } | { ok: false; codes: AdsCode[] };
/**
 * Turn the form into the exact DraftInput (11 keys) or list every local rule that failed. `allowanceCurrency`
 * = "" means not yet set (server decides). `nowMs` is injected so the date rules are testable.
 */
export function buildDraftInput(form: DraftForm, nowMs: number, allowanceCurrency: string): FormResult {
  const codes: AdsCode[] = [];
  if (!canonicalUUID.test(form.ad_binding_id) || !canonicalUUID.test(form.identity_binding_id)) codes.push("bindings_missing");
  if (!validSource(form.template, form.source_ref)) codes.push("source_invalid");
  if (allowanceCurrency && form.currency !== allowanceCurrency) codes.push("currency_mismatch");
  const money = wholeToMinor(form.currency, form.budget);
  if (!money.ok) codes.push(money.code);
  const start = localToEpoch(form.starts_local);
  const end = localToEpoch(form.ends_local);
  if (start === null || end === null) codes.push("dates_invalid");
  else {
    if (start < nowMs + minLeadMs) codes.push("starts_too_soon");
    if (end <= start) codes.push("ends_before_start");
    else if (end > start + maxRunDays * dayMs) codes.push("duration_too_long");
  }
  const countries = parseCountries(form.countries);
  if (!countries) codes.push("countries_invalid");
  const ageMin = /^[0-9]{2}$/.test(form.age_min) ? Number(form.age_min) : NaN;
  const ageMax = /^[0-9]{2}$/.test(form.age_max) ? Number(form.age_max) : NaN;
  if (!(ageMin >= 18 && ageMax <= 65 && ageMin <= ageMax)) codes.push("age_invalid");
  if (codes.length > 0 || !money.ok || start === null || end === null || !countries) return { ok: false, codes };
  return {
    ok: true,
    input: {
      ad_binding_id: form.ad_binding_id, identity_binding_id: form.identity_binding_id, template: form.template,
      source_ref: form.source_ref.trim(), currency: form.currency, lifetime_budget_minor: money.minor,
      starts_at: isoSeconds(start), ends_at: isoSeconds(end), countries, age_min: ageMin, age_max: ageMax,
    },
  };
}

// ---------- capi (PUT ads/capi) ----------
export const testEventCode = /^[A-Za-z0-9_-]{0,64}$/;
export function capiBody(enabled: boolean, datasetBindingID: string, testCode: string) {
  // Empty optional fields go as null, never "" (an empty string is not a uuid for the Go decoder).
  return { enabled, dataset_binding_id: datasetBindingID || null, test_event_code: testCode.trim() || null };
}
export function validCapi(enabled: boolean, datasetBindingID: string, testCode: string): boolean {
  return testEventCode.test(testCode.trim()) && (!enabled || canonicalUUID.test(datasetBindingID)) &&
    (datasetBindingID === "" || canonicalUUID.test(datasetBindingID));
}

// Meta account_status (Marketing API AdAccount): only 1 lets an ad account run ads.
export const accountReady = (status: number) => status === 1;
