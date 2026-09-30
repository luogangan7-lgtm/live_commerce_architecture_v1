// BFF for /api/buyer/* -> Go /v1/buyer/* (internal/buyer, internal/checkout). Payment routes:
//   GET orders/{id}/payment; POST .../payment/prepare (keyed, body), .../handoff|refresh|cancel
//   (keyless, nonretryable; Go HostedPaymentStarter Begin/Take/RefreshPayment/CancelPayment).
// Every success body is re-validated with lib/payment-contract.ts before it reaches the browser.
// CVS (taiwan-cvs-logistics-v1 §5.2, §16.1): exactly POST cvs-selections (keyed), GET cvs-selections/{id},
// POST cvs-selections/{id}/verify (keyless, no body) and POST cvs-stores (keyed) -> Go internal/buyerhttp/cvs.go;
// bodies and answers are re-validated with lib/cvs-contract.ts. No generic proxying.
import { createHmac, randomBytes, timingSafeEqual } from "node:crypto";
import { isIP } from "node:net";
import {
  validHostedHandoff,
  validOrderPayment,
  validPaymentPrepared,
  validPaymentSignal,
  type PaymentMethodCode,
} from "./payment-contract.ts";
import {
  CLAIM_TOKEN,
  validClaimPreview,
  validClaimRedeemed,
} from "./claim-contract.ts";
// customers-billing-ui hook: buyer privacy routes (customers-billing-v1 §5) and their closed response validators.
import {
  privacyBodies,
  privacyRoutes,
  validBuyerExport,
  validBuyerPrivacy,
  validConsentResult,
  validErasureSummary,
} from "./privacy-contract.ts";
import {
  CVS_ERROR_CODES,
  DEFINITE_CVS_CODES,
  isPaymentMode,
  validBuyerStore,
  validCvsSelection,
  validCvsSelectionOpen,
  validReturnPath,
} from "./cvs-contract.ts";

const COOKIE = "__Host-commerce_buyer";
const MAX_JSON = 64 * 1024;
const MAX_UPSTREAM = 1024 * 1024;
const INBOUND_DEADLINE = 12_000;
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const KEY = /^[A-Za-z0-9_.:-]{8,128}$/;
const TOKEN = /^[A-Za-z0-9_-]{43}$/;

type Config = { api: string; bff: string; signing: Buffer; ttl: number };
type Envelope = {
  v: 1;
  token: string;
  iat: number;
  exp: number;
  origin: string;
};
type Session = {
  state: "absent" | "expired" | "inactive" | "active";
  context: string | null;
  expires_at: string | null;
};
type Route = {
  method: string;
  privatePath: string;
  body?:
    | "empty"
    | "cart"
    | "quote"
    | "destination"
    | "checkout"
    | "payment"
    | "claim"
    | "consent"
    | "erasure"
    | "cvsSelection"
    | "cvsStore";
  query?: "catalog" | "options" | "orders";
  session?: string;
  payment?: "view" | "prepare" | "handoff" | "refresh" | "cancel";
  orderID?: string;
  // Claim-link preview/redeem (live-keyword-claims-v1 B1/B2): the only routes that take
  // and forward the X-Commerce-Claim-Token header.
  claim?: "preview" | "redeem";
  // CVS map selection / buyer-entered store routes; keyless = POST with neither key nor body (verify).
  cvs?: "open" | "get" | "verify" | "store";
  keyless?: boolean;
  selectionID?: string;
};

const messages: Record<string, string> = {
  unauthorized: "Session unavailable.",
  forbidden: "Request not permitted.",
  not_found: "Resource not found.",
  method_not_allowed: "Method not allowed.",
  invalid_request: "Request validation failed.",
  invalid_json: "Malformed JSON body.",
  json_required: "JSON content type required.",
  context_changed: "Buyer session changed. Reload before continuing.",
  conflict: "Request conflicts with current state.",
  insufficient_inventory: "Insufficient available inventory.",
  rate_limited: "Too many requests.",
  unavailable: "Temporarily unavailable.",
  erased: "This data was erased.",
  erasure_blocked: "Erasure is blocked while a payment may still be open.",
  export_too_large: "The export is too large for one file.",
  idempotency_conflict: "Request conflicts with an earlier one.",
  // taiwan-cvs-logistics-v1 §5.2/§16 refusals; the UI maps the code to text, these are generic fallbacks.
  ...Object.fromEntries(
    CVS_ERROR_CODES.map((code) => [code, "Request refused."]),
  ),
};

function headers(extra?: HeadersInit) {
  const result = new Headers(extra);
  result.set("Cache-Control", "no-store");
  result.set("X-Content-Type-Options", "nosniff");
  return result;
}

function failure(status: number, code: string, nonretryable = false): Response {
  const request_id = randomBytes(16).toString("hex");
  const safeCode = Object.hasOwn(messages, code) ? code : "unavailable";
  const h = headers({ "X-Request-ID": request_id });
  return Response.json(
    {
      code: safeCode,
      message: messages[safeCode],
      request_id,
      // A definite CVS refusal (e.g. pay_at_pickup_limit, 429) committed nothing: never "retryable".
    retryable:
      !nonretryable &&
      !DEFINITE_CVS_CODES.includes(code) &&
      (status === 503 || status === 429),
      details: {},
    },
    { status, headers: h },
  );
}

function success(value: unknown, extra?: HeadersInit): Response {
  return Response.json(value, { headers: headers(extra) });
}

function canonical32(value: string): boolean {
  return (
    TOKEN.test(value) &&
    Buffer.from(value, "base64url").length === 32 &&
    Buffer.from(value, "base64url").toString("base64url") === value
  );
}

function config(): Config | null {
  const enabled = process.env.COMMERCE_BUYER_WEB_ENABLED ?? "";
  if (enabled === "" || enabled === "0") return null;
  if (enabled !== "1") throw new Error("invalid enabled flag");
  const raw = process.env.COMMERCE_BUYER_API_ORIGIN ?? "";
  let url: URL;
  try {
    url = new URL(raw);
  } catch {
    throw new Error("invalid API origin");
  }
  const loopback =
    url.protocol === "http:" &&
    (url.hostname === "127.0.0.1" || url.hostname === "localhost") &&
    !!url.port;
  if (
    raw !== url.origin ||
    (url.protocol !== "https:" && !loopback) ||
    url.username ||
    url.password ||
    url.search ||
    url.hash
  )
    throw new Error("invalid API origin");
  const bff = process.env.COMMERCE_BUYER_BFF_KEY ?? "";
  const cookie = process.env.COMMERCE_BUYER_COOKIE_KEY ?? "";
  if (!canonical32(bff) || !canonical32(cookie) || bff === cookie)
    throw new Error("invalid keys");
  const rawTTL = process.env.COMMERCE_BUYER_SESSION_TTL ?? "";
  if (!/^[1-9][0-9]{1,6}$/.test(rawTTL)) throw new Error("invalid TTL");
  const ttl = Number(rawTTL);
  if (ttl < 60 || ttl > 2592000) throw new Error("invalid TTL");
  return { api: raw, bff, signing: Buffer.from(cookie, "base64url"), ttl };
}

function candidateOrigin(request: Request): string | null {
  const host = request.headers.get("host");
  if (
    !host ||
    host.length > 253 ||
    host !== host.toLowerCase() ||
    host.includes(":") ||
    host.includes(",") ||
    host.includes("%") ||
    host.includes("@") ||
    host.endsWith(".") ||
    isIP(host)
  )
    return null;
  const labels = host.split(".");
  if (
    labels.length < 2 ||
    labels.some(
      (label) =>
        label.length > 63 || !/^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$/.test(label),
    )
  )
    return null;
  return `https://${host}`;
}

function mac(config: Config, purpose: string, text: string): Buffer {
  return createHmac("sha256", config.signing)
    .update(purpose)
    .update("\0")
    .update(text)
    .digest();
}

function envelopeValue(config: Config, envelope: Envelope): string {
  const payload = Buffer.from(JSON.stringify(envelope)).toString("base64url");
  return `${payload}.${mac(config, "buyer-cookie-v1", payload).toString("base64url")}`;
}

function context(config: Config, origin: string, cookie: string): string {
  return mac(config, "buyer-context-v1", `${origin}\0${cookie}`).toString(
    "base64url",
  );
}

function cookieValue(request: Request): {
  kind: "absent" | "invalid" | "present";
  value?: string;
} {
  const source = request.headers.get("cookie") ?? "";
  if (source.length > 8192) return { kind: "invalid" };
  const values: string[] = [];
  for (const part of source.split(";")) {
    const at = part.indexOf("=");
    if (at >= 0 && part.slice(0, at).trim() === COOKIE)
      values.push(part.slice(at + 1).trim());
  }
  return values.length === 0
    ? { kind: "absent" }
    : values.length === 1 && values[0].length <= 1024
      ? { kind: "present", value: values[0] }
      : { kind: "invalid" };
}

function parseEnvelope(
  config: Config,
  origin: string,
  value: string,
): Envelope | null {
  const pieces = value.split(".");
  if (
    pieces.length !== 2 ||
    !/^[A-Za-z0-9_-]+$/.test(pieces[0]) ||
    !canonical32(pieces[1])
  )
    return null;
  const expected = mac(config, "buyer-cookie-v1", pieces[0]);
  if (!timingSafeEqual(expected, Buffer.from(pieces[1], "base64url")))
    return null;
  try {
    const bytes = Buffer.from(pieces[0], "base64url");
    if (bytes.toString("base64url") !== pieces[0]) return null;
    const decoded: unknown = JSON.parse(
      new TextDecoder("utf-8", { fatal: true }).decode(bytes),
    );
    if (!decoded || typeof decoded !== "object" || Array.isArray(decoded))
      return null;
    const e = decoded as Envelope;
    if (
      e.v !== 1 ||
      !canonical32(e.token) ||
      !Number.isSafeInteger(e.iat) ||
      !Number.isSafeInteger(e.exp) ||
      e.origin !== origin ||
      e.exp <= e.iat ||
      e.exp - e.iat > config.ttl ||
      e.iat > Math.floor(Date.now() / 1000) + 60 ||
      JSON.stringify(e) !== bytes.toString("utf8") ||
      Object.keys(e).join(",") !== "v,token,iat,exp,origin"
    )
      return null;
    return e;
  } catch {
    return null;
  }
}

function identify(
  request: Request,
  config: Config,
  origin: string,
): {
  envelope: Envelope | null;
  cookie: string | null;
  context: string | null;
  invalid: boolean;
} {
  const found = cookieValue(request);
  if (found.kind === "absent")
    return { envelope: null, cookie: null, context: null, invalid: false };
  if (found.kind === "invalid")
    return { envelope: null, cookie: null, context: null, invalid: true };
  const e = parseEnvelope(config, origin, found.value!);
  return e
    ? {
        envelope: e,
        cookie: found.value!,
        context: context(config, origin, found.value!),
        invalid: false,
      }
    : { envelope: null, cookie: null, context: null, invalid: true };
}

function route(
  path: string,
  method: string,
): { route?: Route; known: boolean; invalidID?: boolean } {
  const prefix = "/api/buyer/";
  if (!path.startsWith(prefix) || path.includes("%") || path.includes("//"))
    return { known: false };
  const suffix = path.slice(prefix.length);
  const session = /^session(?:\/(prepare|activate|reset|logout))?$/.exec(
    suffix,
  );
  if (session) {
    const allowed = session[1] ? "POST" : "GET";
    return {
      known: true,
      route:
        method === allowed
          ? {
              method,
              privatePath: "session",
              body: session[1] ? "empty" : undefined,
              session: session[1] ?? "status",
            }
          : undefined,
    };
  }
  const exact: Record<string, Record<string, Omit<Route, "method">>> = {
    catalog: { GET: { privatePath: "catalog", query: "catalog" } },
    "checkout-options": {
      GET: { privatePath: "checkout-options", query: "options" },
    },
    cart: {
      GET: { privatePath: "cart" },
      PUT: { privatePath: "cart", body: "cart" },
    },
    quotes: { POST: { privatePath: "quotes", body: "quote" } },
    destination: {
      GET: { privatePath: "destination" },
      PUT: { privatePath: "destination", body: "destination" },
    },
    checkout: { POST: { privatePath: "checkout", body: "checkout" } },
    orders: { GET: { privatePath: "orders", query: "orders" } },
    "cvs-selections": {
      POST: { privatePath: "cvs-selections", body: "cvsSelection", cvs: "open" },
    },
    "cvs-stores": {
      POST: { privatePath: "cvs-stores", body: "cvsStore", cvs: "store" },
    },
    "claim-link": { GET: { privatePath: "claim-link", claim: "preview" } },
    "claim-link/redeem": {
      POST: { privatePath: "claim-link/redeem", body: "claim", claim: "redeem" },
    },
    ...privacyRoutes,
  };
  if (Object.hasOwn(exact, suffix)) {
    const selected = exact[suffix][method];
    return {
      known: true,
      route: selected ? { ...selected, method } : undefined,
    };
  }
  const payment = /^orders\/([^/]+)\/payment(?:\/(prepare|handoff|refresh|cancel))?$/.exec(
    suffix,
  );
  if (payment) {
    if (!UUID.test(payment[1])) return { known: true, invalidID: true };
    const kind = payment[2] ?? "view";
    const allowed = kind === "view" ? "GET" : "POST";
    return {
      known: true,
      route:
        method === allowed
          ? {
              method,
              privatePath: suffix,
              body: kind === "prepare" ? "payment" : undefined,
              payment: kind as Route["payment"],
              orderID: payment[1],
            }
          : undefined,
    };
  }
  const selection = /^cvs-selections\/([^/]+)(?:\/(verify))?$/.exec(suffix);
  if (selection) {
    if (!UUID.test(selection[1])) return { known: true, invalidID: true };
    const verify = selection[2] === "verify";
    return {
      known: true,
      route:
        method === (verify ? "POST" : "GET")
          ? {
              method,
              privatePath: suffix,
              cvs: verify ? "verify" : "get",
              keyless: verify,
              selectionID: selection[1],
            }
          : undefined,
    };
  }
  const item = /^(quotes|destinations|orders)\/([^/]+)$/.exec(suffix);
  if (!item) return { known: false };
  if (!UUID.test(item[2])) return { known: true, invalidID: true };
  return {
    known: true,
    route: method === "GET" ? { method, privatePath: suffix } : undefined,
  };
}

function validQuery(rawURL: string, kind?: Route["query"]): string | null {
  const mark = rawURL.indexOf("?");
  if (mark < 0) return "";
  if (!kind) return null;
  const raw = rawURL.slice(mark + 1).split("#", 1)[0];
  if (
    !raw ||
    raw.length > 2048 ||
    raw.includes(";") ||
    raw.includes("+") ||
    raw.split("&").some((part) => !part || !part.includes("="))
  )
    return null;
  const allowed =
    kind === "catalog"
      ? ["product_id", "limit", "cursor"]
      : kind === "orders"
        ? ["limit", "cursor"]
        : ["market_id", "country", "limit", "cursor"];
  const seen = new Set<string>();
  for (const part of raw.split("&")) {
    const at = part.indexOf("=");
    let name: string, value: string;
    try {
      name = decodeURIComponent(part.slice(0, at));
      value = decodeURIComponent(part.slice(at + 1));
    } catch {
      return null;
    }
    if (!allowed.includes(name) || seen.has(name) || !value) return null;
    seen.add(name);
    if ((name === "product_id" || name === "market_id") && !UUID.test(value))
      return null;
    if (name === "country" && !/^[A-Z]{2}$/.test(value)) return null;
    if (
      name === "limit" &&
      (!/^[0-9]{1,3}$/.test(value) || +value < 1 || +value > 100)
    )
      return null;
    if (
      name === "cursor" &&
      (value.length > 1024 || !/^[A-Za-z0-9_-]+$/.test(value))
    )
      return null;
  }
  return `?${raw}`;
}

async function readBounded(
  stream: ReadableStream<Uint8Array> | null,
  limit: number,
  timeoutMs = 0,
): Promise<Uint8Array | null> {
  if (!stream) return new Uint8Array();
  const reader = stream.getReader();
  const chunks: Uint8Array[] = [];
  let length = 0;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let timedOut = false;
  const deadline =
    timeoutMs > 0
      ? new Promise<never>((_, reject) => {
          timer = setTimeout(() => {
            timedOut = true;
            reject(new Error("body deadline"));
            void reader.cancel().catch(() => {});
          }, timeoutMs);
        })
      : null;
  try {
    while (true) {
      const part = deadline
        ? await Promise.race([reader.read(), deadline])
        : await reader.read();
      if (timedOut) throw new Error("body deadline");
      if (part.done) break;
      length += part.value.length;
      if (length > limit) {
        void reader.cancel().catch(() => {});
        return null;
      }
      chunks.push(part.value);
    }
  } finally {
    if (timer) clearTimeout(timer);
  }
  const result = new Uint8Array(length);
  let offset = 0;
  for (const chunk of chunks) {
    result.set(chunk, offset);
    offset += chunk.length;
  }
  return result;
}

// JSON.parse alone drops duplicate keys. This small scanner rejects them and
// null at any depth before a commerce command reaches the private transport.
function strictJSON(text: string, allowNull = false): unknown {
  let at = 0;
  const space = () => {
    while (/\s/.test(text[at] ?? "") && at < text.length) at++;
  };
  const string = (): string => {
    const start = at++;
    while (at < text.length) {
      if (text[at] === "\\") {
        at += 2;
        continue;
      }
      if (text[at++] === '"')
        return JSON.parse(text.slice(start, at)) as string;
    }
    throw new Error("string");
  };
  const value = (): void => {
    space();
    if (text[at] === "{") {
      at++;
      space();
      const keys = new Set<string>();
      if (text[at] === "}") {
        at++;
        return;
      }
      while (true) {
        if (text[at] !== '"') throw new Error("key");
        const key = string();
        if (keys.has(key)) throw new Error("duplicate");
        keys.add(key);
        space();
        if (text[at++] !== ":") throw new Error("colon");
        value();
        space();
        if (text[at] === "}") {
          at++;
          return;
        }
        if (text[at++] !== ",") throw new Error("comma");
        space();
      }
    }
    if (text[at] === "[") {
      at++;
      space();
      if (text[at] === "]") {
        at++;
        return;
      }
      while (true) {
        value();
        space();
        if (text[at] === "]") {
          at++;
          return;
        }
        if (text[at++] !== ",") throw new Error("comma");
      }
    }
    if (text[at] === '"') {
      string();
      return;
    }
    for (const literal of ["true", "false"])
      if (text.startsWith(literal, at)) {
        at += literal.length;
        return;
      }
    if (text.startsWith("null", at)) {
      if (!allowNull) throw new Error("null");
      at += 4;
      return;
    }
    const number = /^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?/.exec(
      text.slice(at),
    );
    if (!number || !Number.isFinite(Number(number[0])))
      throw new Error("value");
    at += number[0].length;
  };
  value();
  space();
  if (at !== text.length) throw new Error("trailing");
  return JSON.parse(text) as unknown;
}

type Shape = { [key: string]: "string" | "integer" | "boolean" | Shape | [Shape] };
const shapes: Record<Exclude<Route["body"], undefined>, Shape> = {
  empty: {},
  cart: {
    expected_version: "integer",
    items: [{ sku_id: "string", quantity: "integer" }],
  },
  quote: {
    cart_version: "integer",
    market_id: "string",
    country: "string",
    method: "string",
  },
  destination: {
    expected_version: "integer",
    cart_version: "integer",
    kind: "string",
    country: "string",
    recipient_name: "string",
    phone: "string",
    home_address: {
      region: "string",
      city: "string",
      postal_code: "string",
      line1: "string",
      line2: "string",
    },
    pickup_id: "string",
  },
  checkout: {
    quote_id: "string",
    destination_id: "string",
    cart_version: "integer",
    service_version: "integer",
    allocation_version: "integer",
    payment_mode: "string",
  },
  cvsSelection: {
    cart_version: "integer",
    market_id: "string",
    service_code: "string",
    return_path: "string",
  },
  cvsStore: {
    cart_version: "integer",
    market_id: "string",
    service_code: "string",
    store_code: "string",
    store_name: "string",
    store_address: "string",
  },
  payment: {
    method_code: "string",
    method_version: "integer",
    locale: "string",
  },
  claim: { expected_bundle_version: "integer" },
  consent: privacyBodies.consent,
  erasure: privacyBodies.erasure,
};

function matchesShape(value: unknown, shape: Shape): boolean {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  return Object.entries(value).every(([key, field]) => {
    if (!Object.hasOwn(shape, key)) return false;
    const expected = shape[key];
    if (!expected) return false;
    if (expected === "string") return typeof field === "string";
    if (expected === "integer") return Number.isSafeInteger(field);
    if (expected === "boolean") return typeof field === "boolean";
    if (Array.isArray(expected))
      return (
        Array.isArray(field) &&
        field.every((item) => matchesShape(item, expected[0]))
      );
    return matchesShape(field, expected);
  });
}

// Checks beyond key/type shape for the CVS bodies (exact key sets, allowlisted return_path) and the checkout
// payment_mode enum. Go re-validates everything; this keeps malformed input off the private transport.
const UUID_TEXT = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
function extraShapeOK(shape: Shape, parsed: unknown): boolean {
  const v = parsed as Record<string, unknown>;
  if (shape === shapes.checkout)
    return v.payment_mode === undefined || isPaymentMode(v.payment_mode);
  if (shape !== shapes.cvsSelection && shape !== shapes.cvsStore) return true;
  if (
    Object.keys(v).length !== Object.keys(shape).length ||
    (v.cart_version as number) < 1 ||
    !UUID_TEXT.test(String(v.market_id)) ||
    !/^[a-z][a-z0-9_-]{0,39}$/.test(String(v.service_code))
  )
    return false;
  return shape === shapes.cvsSelection
    ? validReturnPath(v.return_path)
    : ["store_code", "store_name", "store_address"].every(
        (k) => typeof v[k] === "string" && (v[k] as string).length <= 512,
      );
}

async function bodyJSON(
  request: Request,
  shape: Shape,
  nonretryable = false,
): Promise<{ body?: string; error?: Response }> {
  const mime = request.headers.get("content-type") ?? "";
  if (!/^application\/json(?:\s*;\s*charset=(?:utf-8|"utf-8"))?$/i.test(mime))
    return { error: failure(415, "json_required", nonretryable) };
  const length = request.headers.get("content-length");
  if (length !== null && (!/^[0-9]+$/.test(length) || +length > MAX_JSON))
    return { error: failure(422, "invalid_request", nonretryable) };
  let bytes: Uint8Array | null;
  try {
    bytes = await readBounded(request.body, MAX_JSON, INBOUND_DEADLINE);
  } catch {
    return { error: failure(503, "unavailable", nonretryable) };
  }
  if (!bytes) return { error: failure(422, "invalid_request", nonretryable) };
  let text: string;
  try {
    text = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
  } catch {
    return { error: failure(400, "invalid_json", nonretryable) };
  }
  try {
    const parsed = strictJSON(text);
    if (
      !matchesShape(parsed, shape) ||
      !extraShapeOK(shape, parsed) ||
      (shape === shapes.payment &&
        (!parsed ||
          typeof parsed !== "object" ||
          Array.isArray(parsed) ||
          Object.keys(parsed).length !== 3 ||
          !["payuni_credit", "stripe_checkout"].includes(
            (parsed as Record<string, string>).method_code,
          ) ||
          !Number.isSafeInteger(
            (parsed as Record<string, unknown>).method_version,
          ) ||
          ((parsed as Record<string, unknown>).method_version as number) < 1 ||
          !["zh-CN", "zh-TW", "en"].includes(
            (parsed as Record<string, string>).locale,
          )))
    )
      return { error: failure(400, "invalid_json", nonretryable) };
  } catch {
    return { error: failure(400, "invalid_json", nonretryable) };
  }
  return { body: text };
}

async function noBody(
  request: Request,
): Promise<"empty" | "invalid" | "unavailable"> {
  if (
    request.headers.get("content-length") &&
    request.headers.get("content-length") !== "0"
  )
    return "invalid";
  try {
    const bytes = await readBounded(request.body, 0, INBOUND_DEADLINE);
    return bytes !== null && bytes.length === 0 ? "empty" : "invalid";
  } catch {
    return "unavailable";
  }
}

async function upstream(
  request: Request,
  config: Config,
  origin: string,
  token: string,
  path: string,
  method: string,
  body?: string,
  key?: string,
  claimToken?: string,
): Promise<Response> {
  const outbound = new Headers({
    Accept: "application/json",
    "X-Commerce-Buyer-BFF-Key": config.bff,
    "X-Commerce-Storefront-Origin": origin,
    Authorization: `Bearer ${token}`,
  });
  if (body !== undefined) outbound.set("Content-Type", "application/json");
  if (key) outbound.set("Idempotency-Key", key);
  if (claimToken) outbound.set("X-Commerce-Claim-Token", claimToken);
  // meta-ads-v1 A-3: Go consentPut records the BROWSER User-Agent for CAPI after an ads_personalization grant
  // (ads.put_capi_context); without this the API would see this server's fetch agent. Consents PUT only.
  const agent = request.headers.get("user-agent");
  if (path === "consents" && method === "PUT" && agent) outbound.set("User-Agent", agent);
  // B20: ECPay map Device=1 for phone user agents (buyers arrive from FB/IG on phones); Go buyerhttp/cvs.go mobileHint reads it.
  if (
    path === "cvs-selections" &&
    /Mobile|Android|iPhone/i.test(request.headers.get("user-agent") ?? "")
  )
    outbound.set("X-Commerce-Device", "mobile");
  try {
    return await fetch(`${config.api}/v1/buyer/${path}`, {
      method,
      headers: outbound,
      body,
      cache: "no-store",
      redirect: "error",
      signal: AbortSignal.any([request.signal, AbortSignal.timeout(12000)]),
    });
  } catch {
    return failure(503, "unavailable");
  }
}

async function upstreamJSON(
  response: Response,
  payment = false,
): Promise<unknown | null> {
  const mime = response.headers.get("content-type") ?? "";
  if (
    (payment &&
      response.headers.has("content-encoding") &&
      response.headers.get("content-encoding") !== "identity") ||
    (payment
      ? !/^application\/json(?:\s*;\s*charset=(?:utf-8|"utf-8"))?$/i.test(mime)
      : mime.split(";", 1)[0].trim().toLowerCase() !== "application/json")
  )
    return null;
  const advertised = response.headers.get("content-length");
  if (
    advertised !== null &&
    (!/^[0-9]+$/.test(advertised) || +advertised > MAX_UPSTREAM)
  )
    return null;
  try {
    const bytes = await readBounded(response.body, MAX_UPSTREAM);
    if (!bytes) return null;
    const text = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
    return payment ? strictJSON(text, true) : (JSON.parse(text) as unknown);
  } catch {
    return null;
  }
}

async function upstreamError(
  response: Response,
  nonretryable = false,
): Promise<Response> {
  const body = await upstreamJSON(response);
  const code =
    body && typeof body === "object" && !Array.isArray(body)
      ? (body as { code?: unknown }).code
      : undefined;
  if (
    typeof code !== "string" ||
    !Object.hasOwn(messages, code) ||
    ![400, 401, 403, 404, 409, 410, 415, 422, 429, 503].includes(response.status)
  )
    return failure(503, "unavailable", nonretryable);
  return failure(response.status, code, nonretryable);
}

function session(
  state: Session["state"],
  contextValue: string | null,
  expires: number | null,
): Session {
  return {
    state,
    context: contextValue,
    expires_at:
      expires === null ? null : new Date(expires * 1000).toISOString(),
  };
}

function prepared(config: Config, origin: string): Response {
  const iat = Math.floor(Date.now() / 1000);
  const e: Envelope = {
    v: 1,
    token: randomBytes(32).toString("base64url"),
    iat,
    exp: iat + config.ttl,
    origin,
  };
  const value = envelopeValue(config, e);
  const setCookie = `${COOKIE}=${value}; Path=/; Max-Age=${config.ttl}; Secure; HttpOnly; SameSite=Lax`;
  return success(session("inactive", context(config, origin, value), e.exp), {
    "Set-Cookie": setCookie,
  });
}

function forbiddenHeaders(request: Request): boolean {
  return [
    "authorization",
    "x-tenant-id",
    "x-store-id",
    "x-commerce-buyer-bff-key",
    "x-commerce-storefront-origin",
    "x-commerce-bff-key",
  ].some((name) => request.headers.has(name));
}

export async function handleBuyerRequest(request: Request): Promise<Response> {
  const pathname = new URL(request.url).pathname;
  // Classify before config/auth: even an early failure must not invite a second
  // one-shot handoff. See buyer-payment-public-v1, not ordinary keyed writes.
  // Refresh and cancel share the keyless, nonretryable class (stripe-buyer-ui-v1 §2).
  const handoff =
    /^\/api\/buyer\/orders\/[^/]+\/payment\/(?:handoff|refresh|cancel)(?:\/.*)?$/.test(
      pathname,
    );
  const changingCookie =
    request.method === "POST" &&
    /^\/api\/buyer\/session\/(?:prepare|reset)$/.test(pathname);
  const fail = (status: number, code: string) =>
    failure(status, code, changingCookie || handoff);
  let cfg: Config | null;
  try {
    cfg = config();
  } catch {
    return fail(503, "unavailable");
  }
  if (!cfg) return fail(404, "not_found");
  const origin = candidateOrigin(request);
  if (!origin || forbiddenHeaders(request)) return fail(403, "forbidden");
  const url = new URL(request.url);
  const selected = route(url.pathname, request.method);
  if (!selected.known) return fail(404, "not_found");
  if (selected.invalidID) return fail(422, "invalid_request");
  if (!selected.route) return fail(405, "method_not_allowed");
  const target = selected.route;
  // The claim token travels only in its header and only on the two claim routes; it is
  // never read from a path, query or body and never echoed (live-keyword-claims-v1 §7.2).
  const claimToken = request.headers.get("x-commerce-claim-token");
  if (!target.claim && claimToken !== null) return fail(403, "forbidden");
  if (target.claim && (claimToken === null || !CLAIM_TOKEN.test(claimToken)))
    return fail(422, "invalid_request");
  const query = validQuery(request.url, target.query);
  if (query === null || url.hash) return fail(422, "invalid_request");
  const isMutation = request.method !== "GET";
  if (isMutation && request.headers.get("origin") !== origin)
    return fail(403, "forbidden");
  const found = identify(request, cfg, origin);
  if (found.invalid) return fail(401, "unauthorized");
  if (
    target.session === "prepare" &&
    !found.envelope &&
    request.headers.has("x-buyer-context")
  )
    return fail(409, "context_changed");
  const isSession = !!target.session;
  const key = request.headers.get("idempotency-key");
  if (isSession || request.method === "GET" || handoff || target.keyless) {
    if (key !== null) return fail(422, "invalid_request");
  } else if (!key || !KEY.test(key)) return fail(422, "invalid_request");
  if (
    isMutation &&
    !(target.session === "prepare" && !found.envelope) &&
    request.headers.get("x-buyer-context") !== found.context
  )
    return fail(409, "context_changed");
  if (
    !isSession &&
    (!found.envelope || found.envelope.exp <= Math.floor(Date.now() / 1000))
  )
    return fail(401, "unauthorized");
  if (
    isSession &&
    target.session !== "prepare" &&
    target.session !== "status" &&
    !found.envelope
  )
    return fail(401, "unauthorized");
  if (isSession && target.session === "prepare" && found.envelope)
    return fail(409, "conflict");
  if (
    isSession &&
    target.session === "activate" &&
    found.envelope!.exp <= Math.floor(Date.now() / 1000)
  )
    return fail(401, "unauthorized");
  if (!isSession && request.headers.get("x-buyer-context") !== found.context)
    return fail(409, "context_changed");
  let body: string | undefined;
  if (target.body) {
    const result = await bodyJSON(request, shapes[target.body], changingCookie);
    if (result.error) return result.error;
    // Go privacyExport takes no body (internal/buyerhttp noBody → 422); the browser hop's {} stops here.
    body = target.privatePath === "privacy/export" ? undefined : result.body;
  } else {
    const result = await noBody(request);
    if (result !== "empty")
      return fail(
        result === "unavailable" ? 503 : 422,
        result === "unavailable" ? "unavailable" : "invalid_request",
      );
  }
  if (request.signal.aborted) return fail(503, "unavailable");
  if (target.session === "prepare") return prepared(cfg, origin);
  if (target.session === "reset" || target.session === "logout") {
    const response = await upstream(
      request,
      cfg,
      origin,
      found.envelope!.token,
      "session/retire",
      "POST",
      "{}",
    );
    if (request.signal.aborted) return fail(503, "unavailable");
    if (response.status !== 204)
      return upstreamError(response, target.session === "reset");
    return target.session === "reset"
      ? prepared(cfg, origin)
      : new Response(null, { status: 204, headers: headers() });
  }
  if (target.session === "status") {
    if (!found.envelope) return success(session("absent", null, null));
    if (found.envelope.exp <= Math.floor(Date.now() / 1000))
      return success(session("expired", found.context, found.envelope.exp));
    const response = await upstream(
      request,
      cfg,
      origin,
      found.envelope.token,
      "session",
      "GET",
    );
    if (request.signal.aborted) return fail(503, "unavailable");
    if (response.status === 401)
      return success(session("inactive", found.context, found.envelope.exp));
    if (!response.ok) return upstreamError(response);
    const result = await upstreamJSON(response);
    if (
      !result ||
      typeof result !== "object" ||
      (result as { authenticated?: unknown }).authenticated !== true
    )
      return fail(503, "unavailable");
    return success(session("active", found.context, found.envelope.exp));
  }
  if (target.session === "activate") {
    const response = await upstream(
      request,
      cfg,
      origin,
      found.envelope!.token,
      "session/bootstrap",
      "POST",
      "{}",
    );
    if (request.signal.aborted) return fail(503, "unavailable");
    if (!response.ok) return upstreamError(response);
    const result = await upstreamJSON(response);
    const expiry =
      result && typeof result === "object"
        ? (result as { authenticated?: unknown; expires_at?: unknown })
        : null;
    const effective =
      typeof expiry?.expires_at === "string"
        ? Date.parse(expiry.expires_at)
        : NaN;
    if (
      expiry?.authenticated !== true ||
      !Number.isFinite(effective) ||
      effective <= Date.now()
    )
      return fail(503, "unavailable");
    return success(
      session(
        "active",
        found.context,
        Math.min(found.envelope!.exp, Math.floor(effective / 1000)),
      ),
    );
  }
  const response = await upstream(
    request,
    cfg,
    origin,
    found.envelope!.token,
    target.privatePath + query,
    request.method,
    body,
    key ?? undefined,
    claimToken ?? undefined,
  );
  if (request.signal.aborted) return fail(503, "unavailable");
  // Erasure revokes the capability (customers-billing-v1 §5): the cookie is cleared on the 200 and on the replay 410.
  const erasure = target.privatePath === "privacy/erasure";
  const revoked = {
    "Set-Cookie": `${COOKIE}=; Path=/; Max-Age=0; Secure; HttpOnly; SameSite=Lax`,
  };
  if (erasure && response.status === 410) {
    const gone = failure(410, "erased", true);
    gone.headers.append("Set-Cookie", revoked["Set-Cookie"]);
    return gone;
  }
  if (!response.ok) return upstreamError(response, handoff);
  const data = await upstreamJSON(response, !!target.payment);
  if (request.signal.aborted || data === null || typeof data !== "object")
    return fail(503, "unavailable");
  if (
    target.cvs &&
    (![200, 201].includes(response.status) ||
      (target.cvs === "open" && !validCvsSelectionOpen(data)) ||
      (target.cvs === "store" && !validBuyerStore(data)) ||
      ((target.cvs === "get" || target.cvs === "verify") &&
        (!validCvsSelection(data) ||
          (data as { selection_id: string }).selection_id !== target.selectionID)))
  )
    return fail(503, "unavailable");
  if (
    target.claim &&
    (response.status !== 200 ||
      (target.claim === "preview" && !validClaimPreview(data)) ||
      (target.claim === "redeem" && !validClaimRedeemed(data)))
  )
    return fail(503, "unavailable");
  if (
    target.payment &&
    (response.status !== 200 ||
      (target.payment === "view" &&
        !validOrderPayment(data, target.orderID!)) ||
      (target.payment === "prepare" &&
        !validPaymentPrepared(
          data,
          target.orderID!,
          // bodyJSON already proved the body is JSON with a valid method_code.
          (JSON.parse(body!) as { method_code: PaymentMethodCode }).method_code,
        )) ||
      (target.payment === "handoff" &&
        !validHostedHandoff(data, target.orderID!)) ||
      ((target.payment === "refresh" || target.payment === "cancel") &&
        !validPaymentSignal(data, target.orderID!)))
  )
    return fail(503, "unavailable");
  // Buyer privacy: only the frozen closed shapes leave the BFF; the export is offered as a download, never cached.
  const privacyValid =
    target.privatePath === "privacy"
      ? validBuyerPrivacy(data)
      : target.privatePath === "consents"
        ? validConsentResult(data)
        : target.privatePath === "privacy/export"
          ? validBuyerExport(data)
          : erasure
            ? // Go /v1/buyer/privacy/erasure answers {erased, orders_retained, summary} (internal/buyerhttp erasureResponse).
              // The envelope is validated and passed through whole: PrivacyCenter re-validates the same closed shape.
              validErasureSummary(data)
            : true;
  if (!privacyValid) return fail(503, "unavailable");
  if (erasure) return success(data, revoked);
  if (target.privatePath === "privacy/export")
    return success(data, { "Content-Disposition": 'attachment; filename="my-data.json"' });
  return success(data);
}
