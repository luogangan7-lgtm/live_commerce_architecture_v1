// Admin billing model: strict parsers for the frozen billing-core DTOs (BFF `/api/stores/{store}/billing*`
// -> Go `internal/httpapi/billing.go` + `internal/billing`, contract customers-billing-v1 §5, billing-core FROZEN block).
// Unknown key or broken invariant = invalid response. The redirect URLs are Stripe bearer links (I11): they are parsed
// here, handed to `location.assign` by the caller, and never stored, logged or put in an error.
// Depends on customers-model.ts (shared object/instant/count helpers).
import { count, isInstant, object } from "./customers-model.ts";

export const standings = ["GOOD", "GRACE", "RESTRICTED", "UNBILLED"] as const;
export type Standing = (typeof standings)[number];
// Stripe subscription statuses (docs.stripe.com/api/subscriptions/object#subscription_object-status, 2026-09-29).
export const subscriptionStatuses = [
  "incomplete", "incomplete_expired", "trialing", "active", "past_due", "canceled", "unpaid", "paused",
] as const;
export type SubscriptionStatus = (typeof subscriptionStatuses)[number];
export type Subscription = {
  status: SubscriptionStatus;
  price_id: string;
  current_period_start: string | null;
  current_period_end: string | null;
  cancel_at_period_end: boolean;
  retrieved_at: string;
};
export type Usage = {
  period_start: string;
  period_end: string;
  paid_orders: number;
  claim_windows_opened: number;
  private_replies_sent: number;
  members: number;
};
export type Plan = { price_id: string; name: string; amount_minor: number; currency: string; interval: string };
export type BillingStatus = {
  standing: Standing;
  payment_pending: boolean;
  customer_pinned: boolean;
  subscriptions: Subscription[];
  usage: Usage;
  plans: Plan[];
  stale: boolean;
};

const priceID = /^price_[A-Za-z0-9]{1,64}$/;
const period = /^\d{4}-\d{2}-\d{2}(?:T[0-9:.]+Z)?$/;

function oneOf<T extends string>(value: unknown, values: readonly T[]): value is T {
  return typeof value === "string" && values.includes(value as T);
}
function optionalInstant(value: unknown): value is string | null {
  return value === null || isInstant(value);
}

function parseSubscription(value: unknown): Subscription {
  const v = object(value, [
    "status", "price_id", "current_period_start", "current_period_end", "cancel_at_period_end", "retrieved_at",
  ]);
  if (!oneOf(v.status, subscriptionStatuses) || typeof v.price_id !== "string" || !priceID.test(v.price_id) ||
    !optionalInstant(v.current_period_start) || !optionalInstant(v.current_period_end) ||
    typeof v.cancel_at_period_end !== "boolean" || !isInstant(v.retrieved_at)) throw new Error("unavailable");
  return v as Subscription;
}
function parseUsage(value: unknown): Usage {
  const v = object(value, [
    "period_start", "period_end", "paid_orders", "claim_windows_opened", "private_replies_sent", "members",
  ]);
  if (typeof v.period_start !== "string" || !period.test(v.period_start) ||
    typeof v.period_end !== "string" || !period.test(v.period_end) ||
    ![v.paid_orders, v.claim_windows_opened, v.private_replies_sent, v.members].every(count)) throw new Error("unavailable");
  return v as Usage;
}
function parsePlan(value: unknown): Plan {
  const v = object(value, ["price_id", "name", "amount_minor", "currency", "interval"]);
  if (typeof v.price_id !== "string" || !priceID.test(v.price_id) ||
    typeof v.name !== "string" || v.name === "" || Array.from(v.name).length > 120 ||
    /[\p{C}\p{Zl}\p{Zp}]/u.test(v.name) || !count(v.amount_minor) ||
    typeof v.currency !== "string" || !/^[A-Z]{3}$/.test(v.currency) ||
    !oneOf(v.interval, ["day", "week", "month", "year"])) throw new Error("unavailable");
  return v as Plan;
}

export function parseBillingStatus(value: unknown): BillingStatus {
  const v = object(value, [
    "standing", "payment_pending", "customer_pinned", "subscriptions", "usage", "plans", "stale",
  ]);
  if (!oneOf(v.standing, standings) || typeof v.payment_pending !== "boolean" ||
    typeof v.customer_pinned !== "boolean" || typeof v.stale !== "boolean" ||
    !Array.isArray(v.subscriptions) || v.subscriptions.length > 50 || !Array.isArray(v.plans) || v.plans.length > 10)
    throw new Error("unavailable");
  const plans = v.plans.map(parsePlan);
  if (new Set(plans.map((plan) => plan.price_id)).size !== plans.length) throw new Error("unavailable");
  return {
    standing: v.standing,
    payment_pending: v.payment_pending,
    customer_pinned: v.customer_pinned,
    subscriptions: v.subscriptions.map(parseSubscription),
    usage: parseUsage(v.usage),
    plans,
    stale: v.stale,
  };
}

export function parseStanding(value: unknown): Standing {
  const v = object(value, ["standing"]);
  if (!oneOf(v.standing, standings)) throw new Error("unavailable");
  return v.standing;
}

// Hosted Stripe pages only: an open redirect from a compromised response is refused (I11, contract §5).
const stripeHosts = ["checkout.stripe.com", "billing.stripe.com"];
export function parseRedirect(value: unknown): string {
  const v = object(value, ["url"]);
  if (typeof v.url !== "string" || v.url.length > 2048) throw new Error("unavailable");
  let parsed: URL;
  try {
    parsed = new URL(v.url);
  } catch {
    throw new Error("unavailable");
  }
  if (parsed.protocol !== "https:" || parsed.username || parsed.password || parsed.port ||
    !stripeHosts.includes(parsed.hostname)) throw new Error("unavailable");
  return parsed.toString();
}

// Banner shape (U5): GRACE and RESTRICTED only. UNBILLED/GOOD show nothing; payment_pending lives on the billing page.
export type BannerKind = "grace" | "restricted" | null;
export const bannerKind = (standing: Standing): BannerKind =>
  standing === "GRACE" ? "grace" : standing === "RESTRICTED" ? "restricted" : null;

// The plan section offers checkout unless a live (non-terminal) subscription exists: same rule the server enforces with
// 409 subscription_exists (contract §5 step 2); the server stays the authority.
export const terminalStatuses: readonly SubscriptionStatus[] = ["canceled", "incomplete_expired"];
export const hasLiveSubscription = (status: BillingStatus) =>
  status.subscriptions.some((item) => !terminalStatuses.includes(item.status));
