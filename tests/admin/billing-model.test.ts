// customers-billing-ui: strict parsers for the frozen billing DTOs (apps/admin/lib/billing-model.ts) and the redirect
// URL fence (Stripe bearer links, I11). Synthetic fixtures only; no Stripe key or real ids.
import assert from "node:assert/strict";
import { test } from "node:test";
import {
  bannerKind,
  hasLiveSubscription,
  parseBillingStatus,
  parseRedirect,
  parseStanding,
} from "../../apps/admin/lib/billing-model.ts";

const status = () => ({
  standing: "GOOD", payment_pending: false, customer_pinned: true,
  subscriptions: [{ status: "active", price_id: "price_1Abc", current_period_start: "2026-09-01T00:00:00Z",
    current_period_end: "2026-10-01T00:00:00Z", cancel_at_period_end: false, retrieved_at: "2026-09-29T00:00:00Z" }],
  usage: { period_start: "2026-09-01", period_end: "2026-10-01", paid_orders: 4, claim_windows_opened: 2,
    private_replies_sent: 9, members: 3 },
  plans: [{ price_id: "price_1Abc", name: "Standard", amount_minor: 99900, currency: "TWD", interval: "month" }],
  stale: false,
});

test("billing status accepts the frozen shape (incl. UNBILLED with no customer, plans [])", () => {
  assert.equal(parseBillingStatus(status()).plans[0].amount_minor, 99900);
  const unbilled = { ...status(), standing: "UNBILLED", customer_pinned: false, subscriptions: [], plans: [] };
  assert.equal(parseBillingStatus(unbilled).standing, "UNBILLED");
  const withTimes = { ...status(), usage: { ...status().usage, period_start: "2026-09-01T00:00:00Z", period_end: "2026-10-01T00:00:00Z" } };
  assert.equal(parseBillingStatus(withTimes).usage.members, 3);
});

test("billing status rejects unknown keys, unknown enums, bad money and duplicate plans", () => {
  const bad: Record<string, unknown>[] = [
    { standing: "PAST_DUE" }, { extra: 1 }, { stale: "no" }, { plans: [{ ...status().plans[0], interval: "decade" }] },
    { plans: [{ ...status().plans[0], amount_minor: -1 }] }, { plans: [status().plans[0], status().plans[0]] },
    { plans: [{ ...status().plans[0], price_id: "prod_1" }] },
    { subscriptions: [{ ...status().subscriptions[0], status: "weird" }] },
    { subscriptions: [{ ...status().subscriptions[0], secret: "x" }] },
    { usage: { ...status().usage, members: 1.5 } }, { usage: { ...status().usage, period_start: "soon" } },
  ];
  for (const patch of bad) assert.throws(() => parseBillingStatus({ ...status(), ...patch }), JSON.stringify(patch));
  const { usage: _u, ...noUsage } = status();
  assert.throws(() => parseBillingStatus(noUsage));
});

test("standing endpoint is exactly {standing}", () => {
  assert.equal(parseStanding({ standing: "GRACE" }), "GRACE");
  assert.throws(() => parseStanding({ standing: "GRACE", x: 1 }));
  assert.throws(() => parseStanding({ standing: "grace" }));
  assert.throws(() => parseStanding(null));
});

test("banner shows only for GRACE and RESTRICTED (U5)", () => {
  assert.deepEqual(["GOOD", "GRACE", "RESTRICTED", "UNBILLED"].map((s) => bannerKind(s as never)), [null, "grace", "restricted", null]);
});

test("live subscription rule mirrors 409 subscription_exists", () => {
  const withStatus = (...statuses: string[]) => parseBillingStatus({ ...status(), subscriptions: statuses.map((s) => ({ ...status().subscriptions[0], status: s })) });
  assert.equal(hasLiveSubscription(withStatus()), false);
  assert.equal(hasLiveSubscription(withStatus("canceled", "incomplete_expired")), false);
  assert.equal(hasLiveSubscription(withStatus("canceled", "trialing")), true);
  assert.equal(hasLiveSubscription(withStatus("incomplete")), true);
});

test("redirect URL: https on the two hosted Stripe hosts only, no credentials/port, never echoed on failure", () => {
  assert.equal(parseRedirect({ url: "https://checkout.stripe.com/c/pay/cs_test_x#frag" }), "https://checkout.stripe.com/c/pay/cs_test_x#frag");
  assert.equal(parseRedirect({ url: "https://billing.stripe.com/p/session/test_x" }), "https://billing.stripe.com/p/session/test_x");
  for (const url of [
    "http://checkout.stripe.com/x", "https://evil.example/x", "https://checkout.stripe.com.evil.example/x",
    "https://user:pw@checkout.stripe.com/x", "https://checkout.stripe.com:8443/x", "javascript:alert(1)", "/relative", "",
    `https://checkout.stripe.com/${"a".repeat(2100)}`,
  ])
    assert.throws(() => parseRedirect({ url }), (error: Error) => error.message === "unavailable" && !error.message.includes("http"), url);
  assert.throws(() => parseRedirect({ url: "https://checkout.stripe.com/x", extra: 1 }));
});
