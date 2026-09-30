// Buyer-safe payment wire projections. Admission remains in the Go service.
// Mirrors internal/checkout/payment_view.go (validPaymentViewFor) and stripe.go
// (validStripeHandoff, PaymentSignal); Stripe deltas per contracts/stripe-buyer-ui-v1.md §3.
// Used by lib/buyer-server.ts (BFF response gate) and lib/order-payment.ts (client gate).
export type PaymentMethodCode = "payuni_credit" | "stripe_checkout";

export type OrderPayment = {
  order_id: string;
  currency: string;
  total_minor: number;
  commercial_state: "DRAFT" | "AWAITING_PAYMENT" | "CONFIRMED" | "CANCELLED";
  test_mode: boolean;
  payment_state:
    | "NOT_STARTED"
    | "PENDING"
    | "AUTHORIZED"
    | "CAPTURED"
    | "PARTIALLY_REFUNDED"
    | "REFUNDED"
    | "REVIEW_REQUIRED"
    | "CLOSED_UNPAID";
  handoff_state:
    | "NONE"
    | "PREPARED"
    | "ISSUED"
    | "EXPIRED"
    | "UNAVAILABLE"
    | "CREATING"
    | "READY"
    | "CLOSED";
  handoff_expires_at: string | null;
  // Go emits this only for Stripe attempts; its presence marks the view as Stripe.
  cancel_requested?: boolean;
  // stripe-refund-v1 §7.2: buyer-safe totals, Stripe orders with refund activity only; absent ≡ null.
  refund?: { refunded_minor: number; pending_minor: number } | null;
  methods: {
    code: PaymentMethodCode;
    version: number;
    name_hans: string;
    name_hant: string;
    name_en: string;
  }[];
};

export type PaymentPrepared = {
  order_id: string;
  state: "PAYMENT_PENDING";
  currency: string;
  amount_minor: number;
};

export type PaymentSignal = { order_id: string; scheduled: boolean };

export type HostedForm = {
  action:
    | "https://sandbox-api.payuni.com.tw/api/upp"
    | "https://api.payuni.com.tw/api/upp";
  fields: {
    Version: "2.0";
    MerID: string;
    EncryptInfo: string;
    HashInfo: string;
  };
};

export type HostedHandoff = {
  order_id: string;
  disposition:
    | "ISSUED"
    | "ALREADY_ISSUED"
    | "REDIRECT"
    | "CREATING"
    | "CLOSED"
    | "UNAVAILABLE";
  expires_at: string;
  form?: HostedForm;
  // Stripe REDIRECT only. Callers keep it in a local variable; never store or log it.
  redirect_url?: string;
};

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

function exact(
  value: unknown,
  keys: readonly string[],
): value is Record<string, unknown> {
  return (
    value !== null &&
    typeof value === "object" &&
    !Array.isArray(value) &&
    Object.getPrototypeOf(value) === Object.prototype &&
    Reflect.ownKeys(value).length === keys.length &&
    keys.every((key) => Object.hasOwn(value, key))
  );
}

function utc(value: unknown): value is string {
  if (
    typeof value !== "string" ||
    !/^\d{4}-(?:0[1-9]|1[0-2])-(?:0[1-9]|[12]\d|3[01])T(?:[01]\d|2[0-3]):[0-5]\d:[0-5]\d(?:\.\d{1,9})?Z$/.test(
      value,
    )
  )
    return false;
  const time = Date.parse(value);
  return (
    Number.isFinite(time) &&
    new Date(time).toISOString().slice(0, 19) === value.slice(0, 19)
  );
}

function name(value: unknown): value is string {
  return (
    typeof value === "string" &&
    [...value].length >= 1 &&
    [...value].length <= 120 &&
    value.trim().length > 0 &&
    !/\p{Cc}/u.test(value)
  );
}

// Mirrors validPaymentViewFor (internal/checkout/payment_view.go): refund totals exist only on the
// Stripe projection, stay inside the order total, and agree with the refunded states.
function validRefund(
  value: Record<string, unknown>,
  stripe: boolean,
): boolean {
  const refund = value.refund;
  const state = value.payment_state;
  const refunded = state === "PARTIALLY_REFUNDED" || state === "REFUNDED";
  if (refund === undefined || refund === null) return !refunded;
  if (
    !stripe ||
    !exact(refund, ["refunded_minor", "pending_minor"]) ||
    !Number.isSafeInteger(refund.refunded_minor) ||
    !Number.isSafeInteger(refund.pending_minor)
  )
    return false;
  const done = refund.refunded_minor as number;
  const pending = refund.pending_minor as number;
  const total = value.total_minor as number;
  if (done < 0 || pending < 0 || done > total || pending > total - done)
    return false;
  return !refunded || (done >= 1 && (state === "REFUNDED") === (done >= total));
}

export function validOrderPayment(
  value: unknown,
  orderID: string,
): value is OrderPayment {
  if (!UUID.test(orderID)) return false;
  const keys = [
    "order_id",
    "currency",
    "total_minor",
    "commercial_state",
    "test_mode",
    "payment_state",
    "handoff_state",
    "handoff_expires_at",
    "methods",
  ];
  // cancel_requested is optional on the wire: Go omits it for PAYUNi and not-started orders.
  const hasCancel =
    value !== null &&
    typeof value === "object" &&
    Object.hasOwn(value, "cancel_requested");
  const hasRefund =
    value !== null &&
    typeof value === "object" &&
    Object.hasOwn(value, "refund");
  if (
    !exact(value, [
      ...keys,
      ...(hasCancel ? ["cancel_requested"] : []),
      ...(hasRefund ? ["refund"] : []),
    ])
  )
    return false;
  if (
    value.order_id !== orderID ||
    typeof value.currency !== "string" ||
    !/^[A-Z]{3}$/.test(value.currency) ||
    !Number.isSafeInteger(value.total_minor) ||
    (value.total_minor as number) < 0 ||
    (value.total_minor as number) > 1e12 ||
    typeof value.test_mode !== "boolean" ||
    typeof value.commercial_state !== "string" ||
    !["DRAFT", "AWAITING_PAYMENT", "CONFIRMED", "CANCELLED"].includes(
      value.commercial_state,
    ) ||
    typeof value.payment_state !== "string" ||
    ![
      "NOT_STARTED",
      "PENDING",
      "AUTHORIZED",
      "CAPTURED",
      "PARTIALLY_REFUNDED",
      "REFUNDED",
      "REVIEW_REQUIRED",
      "CLOSED_UNPAID",
    ].includes(value.payment_state) ||
    typeof value.handoff_state !== "string" ||
    ![
      "NONE",
      "PREPARED",
      "ISSUED",
      "EXPIRED",
      "UNAVAILABLE",
      "CREATING",
      "READY",
      "CLOSED",
    ].includes(value.handoff_state)
  )
    return false;
  if (
    value.handoff_state === "NONE"
      ? value.handoff_expires_at !== null
      : value.handoff_state !== "UNAVAILABLE"
        ? !utc(value.handoff_expires_at)
        : value.handoff_expires_at !== null && !utc(value.handoff_expires_at)
  )
    return false;
  // Stripe-only states need the Stripe projection (stripe-buyer-ui-v1 §3).
  if (
    (["CREATING", "READY", "CLOSED"].includes(value.handoff_state) ||
      value.payment_state === "CLOSED_UNPAID") &&
    !hasCancel
  )
    return false;
  if (!validRefund(value, hasCancel)) return false;
  if (hasCancel) {
    if (
      typeof value.cancel_requested !== "boolean" ||
      ["PREPARED", "ISSUED", "EXPIRED"].includes(value.handoff_state) ||
      !Array.isArray(value.methods) ||
      value.methods.length !== 0
    )
      return false;
  }
  if (!Array.isArray(value.methods) || value.methods.length > 2) return false;
  if (
    value.methods.length &&
    (value.payment_state !== "NOT_STARTED" ||
      value.commercial_state !== "DRAFT" ||
      value.handoff_state !== "NONE")
  )
    return false;
  const seen = new Set<string>();
  return value.methods.every((method: unknown) => {
    if (
      !exact(method, [
        "code",
        "version",
        "name_hans",
        "name_hant",
        "name_en",
      ]) ||
      (method.code !== "payuni_credit" && method.code !== "stripe_checkout") ||
      seen.has(method.code)
    )
      return false;
    seen.add(method.code);
    return (
      Number.isSafeInteger(method.version) &&
      (method.version as number) > 0 &&
      name(method.name_hans) &&
      name(method.name_hant) &&
      name(method.name_en)
    );
  });
}

/** Stripe iff cancel_requested is present, or a fresh view offers the Stripe method. */
export function isStripeView(view: OrderPayment): boolean {
  return (
    view.cancel_requested !== undefined ||
    (view.payment_state === "NOT_STARTED" &&
      view.methods[0]?.code === "stripe_checkout")
  );
}

// stripe-psp-v1 §0.2 corrected amount table (minor units); TWD is also whole-dollar steps.
const STRIPE_AMOUNTS: Record<string, [number, number]> = {
  HKD: [400, 99999999],
  USD: [50, 99999999],
  SGD: [50, 99999999],
  MYR: [200, 99999999],
  // TWD min 2500: Stripe SANDBOX rejected 100/1200, accepted 2500 (2026-09-29)
  TWD: [2500, 99999900],
};

export function validPaymentPrepared(
  value: unknown,
  orderID: string,
  method: PaymentMethodCode,
): value is PaymentPrepared {
  if (
    !UUID.test(orderID) ||
    !exact(value, ["order_id", "state", "currency", "amount_minor"]) ||
    value.order_id !== orderID ||
    value.state !== "PAYMENT_PENDING" ||
    !Number.isSafeInteger(value.amount_minor)
  )
    return false;
  const amount = value.amount_minor as number;
  if (method === "payuni_credit")
    return (
      value.currency === "TWD" &&
      amount >= 100 &&
      amount <= 19999900 &&
      amount % 100 === 0
    );
  if (method !== "stripe_checkout" || typeof value.currency !== "string")
    return false;
  const range = Object.hasOwn(STRIPE_AMOUNTS, value.currency)
    ? STRIPE_AMOUNTS[value.currency]
    : undefined;
  return (
    !!range &&
    amount >= range[0] &&
    amount <= range[1] &&
    (value.currency !== "TWD" || amount % 100 === 0)
  );
}

export function validPaymentSignal(
  value: unknown,
  orderID: string,
): value is PaymentSignal {
  return (
    UUID.test(orderID) &&
    exact(value, ["order_id", "scheduled"]) &&
    value.order_id === orderID &&
    typeof value.scheduled === "boolean"
  );
}

// Stripe Checkout URLs only: fixed origin, no userinfo/port, printable ASCII path
// (mirrors Go validStripeRedirect; host/URL origin per stripe-psp-v1 §9.2, docs retrieved there).
export function validStripeRedirectURL(u: unknown): u is string {
  if (
    typeof u !== "string" ||
    !/^https:\/\/checkout\.stripe\.com\/[!-~]{1,4000}$/.test(u)
  )
    return false;
  try {
    const parsed = new URL(u);
    return (
      parsed.origin === "https://checkout.stripe.com" &&
      parsed.username === "" &&
      parsed.password === ""
    );
  } catch {
    return false;
  }
}

// Union keyed by disjoint dispositions: PAYUNi ISSUED/ALREADY_ISSUED, Stripe
// REDIRECT/CREATING/CLOSED/UNAVAILABLE. `method` (client marker) restricts to one family;
// the BFF does not know the method for a keyless handoff and validates the union.
export function validHostedHandoff(
  value: unknown,
  orderID: string,
  method?: PaymentMethodCode,
): value is HostedHandoff {
  const disposition =
    value !== null && typeof value === "object"
      ? (value as Record<string, unknown>).disposition
      : undefined;
  if (
    disposition === "REDIRECT" ||
    disposition === "CREATING" ||
    disposition === "CLOSED" ||
    disposition === "UNAVAILABLE"
  ) {
    return (
      method !== "payuni_credit" &&
      UUID.test(orderID) &&
      exact(
        value,
        disposition === "REDIRECT"
          ? ["order_id", "disposition", "expires_at", "redirect_url"]
          : ["order_id", "disposition", "expires_at"],
      ) &&
      value.order_id === orderID &&
      utc(value.expires_at) &&
      (disposition !== "REDIRECT" || validStripeRedirectURL(value.redirect_url))
    );
  }
  if (method === "stripe_checkout") return false;
  if (
    !UUID.test(orderID) ||
    !exact(
      value,
      disposition === "ISSUED"
        ? ["order_id", "disposition", "expires_at", "form"]
        : ["order_id", "disposition", "expires_at"],
    ) ||
    value.order_id !== orderID ||
    !utc(value.expires_at)
  )
    return false;
  if (value.disposition === "ALREADY_ISSUED") return true;
  if (
    value.disposition !== "ISSUED" ||
    !exact(value.form, ["action", "fields"])
  )
    return false;
  const form = value.form;
  if (
    form.action !== "https://sandbox-api.payuni.com.tw/api/upp" &&
    form.action !== "https://api.payuni.com.tw/api/upp"
  )
    return false;
  if (!exact(form.fields, ["Version", "MerID", "EncryptInfo", "HashInfo"]))
    return false;
  const fields = form.fields;
  return (
    fields.Version === "2.0" &&
    typeof fields.MerID === "string" &&
    /^[A-Za-z0-9_-]{1,64}$/.test(fields.MerID) &&
    typeof fields.EncryptInfo === "string" &&
    fields.EncryptInfo.length >= 16 &&
    fields.EncryptInfo.length <= 24576 &&
    fields.EncryptInfo.length % 2 === 0 &&
    /^[0-9a-f]+$/.test(fields.EncryptInfo) &&
    typeof fields.HashInfo === "string" &&
    /^[0-9A-F]{64}$/.test(fields.HashInfo)
  );
}
