"use client";

// Merchant billing page (/{locale}/billing): standing, plan choices -> Stripe Checkout redirect, "Manage payment &
// invoices" -> Stripe portal redirect, subscription rows, usage this period (U4).
// BFF GET /api/stores/{store}/billing (billing:manage), POST .../billing/checkout {price_id}, POST .../billing/portal
// (keyless) -> Go /v1/admin/stores/{id}/billing[/checkout|/portal] (internal/httpapi/billing.go; internal/billing).
// The returned Stripe URL goes straight to location.assign: never kept in state, storage or logs (I11).
// Coming back with `?checkout=done` shows "updating…" and re-GETs once; the server (webhook) stays the authority on
// standing, so nothing here marks a subscription active.
import { useEffect, useRef, useState } from "react";
import type { Locale } from "@live-commerce/i18n";
import type { Store } from "@/lib/model";
import { money } from "@/lib/client";
import { readBilling, openPortal, startCheckout } from "@/lib/billing-client";
import { useGuardedRead, type ReadCode } from "@/lib/customers-client";
import { hasLiveSubscription, type BillingStatus } from "@/lib/billing-model";
import { displayTime } from "@/lib/orders-model";
import { billingCopy, type BillingCopy } from "@/lib/billing-copy";
import { WorkspaceFrame } from "./WorkspaceFrame";
import "./orders.css";
import "./order-actions.css";
import "./customers.css";

type Phase = "idle" | "updating" | "slow";
const errorText = (c: BillingCopy, code: string) => c.errors[code] ?? c.errors.default;

export function Billing({
  locale,
  stores,
  store,
  checkout,
  initialError,
  renderKey,
}: {
  locale: Locale;
  stores: Store[];
  store: Store | null;
  checkout: "" | "done" | "cancel";
  initialError: ReadCode | null;
  renderKey: string;
}) {
  const c = billingCopy[locale];
  const read = useGuardedRead(
    `${renderKey}|${locale}|${store?.id ?? ""}`,
    store ? (signal) => readBilling(store.id, signal) : null,
    initialError,
  );
  const [phase, setPhase] = useState<Phase>(checkout === "done" ? "updating" : "idle");
  const rechecked = useRef(false);
  const storeID = store?.id ?? "";

  const refreshNow = useRef(read.refresh);
  refreshNow.current = read.refresh;
  // checkout=done: the webhook may land a moment after the redirect. One delayed re-GET, then say so honestly.
  useEffect(() => {
    if (checkout !== "done" || read.status !== "ready" || rechecked.current) return;
    rechecked.current = true;
    const timer = setTimeout(async () => {
      const ok = await refreshNow.current();
      setPhase(ok ? "idle" : "slow");
      // Drop the return marker so a reload does not repeat it; path only, no bearer data is in this URL.
      window.history.replaceState(window.history.state, "", `/${locale}/billing${storeID ? `?store=${storeID}` : ""}`);
    }, 2500);
    return () => clearTimeout(timer);
  }, [checkout, read.status, locale, storeID]);

  const failure =
    read.status === "signed-out" ? c.signedOut
    : read.status === "forbidden" ? c.forbidden
    : read.status === "not-found" ? (store ? c.notFound : c.noStore)
    : read.status === "unavailable" ? c.unavailable
    : "";
  const go = (nextStore: string) => {
    window.location.assign(`/${locale}/billing?store=${nextStore}`);
  };
  return (
    <WorkspaceFrame locale={locale} storeName={store?.name ?? c.noStore} active="billing">
      <div className="orders-page customers-page" data-testid="billing-page">
        <header className="orders-heading">
          <h1>{c.title}</h1>
          <p>{c.subtitle}</p>
        </header>
        <div className="orders-controls">
          {stores.length > 1 && (
            <label>
              {c.store}
              <select data-testid="store-selector" value={store?.id ?? ""} onChange={(event) => go(event.target.value)}>
                {stores.map((item) => (
                  <option key={item.id} value={item.id}>{item.name}</option>
                ))}
              </select>
            </label>
          )}
        </div>
        {checkout === "cancel" && <p className="orders-message" role="status" data-testid="billing-cancelled">{c.checkoutCancel}</p>}
        {phase === "updating" && <p className="orders-message" role="status" data-testid="billing-updating">{c.checkoutDone}</p>}
        {phase === "slow" && <p className="orders-message" role="status">{c.checkoutDoneSlow}</p>}
        {(read.status === "loading" || read.status === "hidden") && <p className="orders-message" role="status">{c.loading}</p>}
        {failure && (
          <div className="orders-message" role="status">
            <p>{failure}</p>
            <button type="button" onClick={read.reload}>{c.retry}</button>
          </div>
        )}
        {read.status === "ready" && read.data && store && (
          <Sections status={read.data} store={store.id} boundary={read.boundary} refresh={read.refresh} locale={locale} c={c} />
        )}
      </div>
    </WorkspaceFrame>
  );
}

function Sections({
  status,
  store,
  boundary,
  refresh,
  locale,
  c,
}: {
  status: BillingStatus;
  store: string;
  boundary: string;
  refresh: () => Promise<boolean>;
  locale: Locale;
  c: BillingCopy;
}) {
  const [busy, setBusy] = useState("");
  const [problem, setProblem] = useState("");
  const live = hasLiveSubscription(status);
  const time = (value: string | null) => (value ? displayTime(locale, value) : "—");

  async function redirect(kind: string, run: () => ReturnType<typeof startCheckout>) {
    if (busy) return;
    setBusy(kind);
    setProblem("");
    const result = await run();
    if (result.ok) {
      // I11: the bearer URL is used once, right here, and dropped; the page is left on purpose so `busy` stays set.
      window.location.assign(result.value);
      return;
    }
    setBusy("");
    setProblem(errorText(c, result.code));
    // The server may know better than this page (e.g. subscription_exists after a second tab): re-read.
    if (result.code === "subscription_exists" || result.code === "no_billing_customer") void refresh();
  }
  return (
    <>
      <p className="orders-hint" data-testid="billing-test-note">{c.testNote}</p>
      {status.stale && <p className="orders-bad" role="status" data-testid="billing-stale">{c.stale}</p>}
      <section className="customers-section" aria-label={c.standingTitle}>
        <h2>{c.standingTitle}</h2>
        <p>
          <span
            className={`orders-badge ${status.standing === "GOOD" ? "orders-tone-success" : status.standing === "UNBILLED" ? "orders-tone-neutral" : status.standing === "GRACE" ? "orders-tone-neutral" : "orders-tone-warning"}`}
            data-testid="billing-standing" data-standing={status.standing}
          >
            {c.standing[status.standing]}
          </span>
        </p>
        <p>{c.standingText[status.standing]}</p>
        {status.payment_pending && <p className="orders-hint" role="status" data-testid="billing-pending">{c.paymentPending}</p>}
      </section>

      <section className="customers-section" aria-label={c.subscriptionsTitle}>
        <h2>{c.subscriptionsTitle}</h2>
        {status.subscriptions.length === 0 ? (
          <p className="orders-empty">{c.subscriptionsNone}</p>
        ) : (
          <div className="orders-actions-scroll">
            <table className="orders-actions-table" data-testid="billing-subscriptions">
              <thead><tr><th>{c.plan}</th><th>{c.status}</th><th>{c.period}</th></tr></thead>
              <tbody>
                {status.subscriptions.map((item) => {
                  const plan = status.plans.find((candidate) => candidate.price_id === item.price_id);
                  return (
                    <tr key={`${item.price_id}-${item.retrieved_at}`}>
                      <td>{plan?.name ?? item.price_id}</td>
                      <td>
                        <span className={`orders-badge ${item.status === "active" || item.status === "trialing" ? "orders-tone-success" : item.status === "canceled" || item.status === "incomplete_expired" ? "orders-tone-neutral" : "orders-tone-warning"}`}>
                          {c.subStatus[item.status]}
                        </span>
                        {item.cancel_at_period_end && <small>{c.endsAtPeriodEnd}</small>}
                      </td>
                      <td>{time(item.current_period_start)} → {time(item.current_period_end)}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
        {status.customer_pinned && (
          <>
            <div className="customers-actions">
              <button type="button" data-testid="billing-portal" disabled={busy !== ""}
                onClick={() => void redirect("portal", () => openPortal(store, boundary))}>
                {busy === "portal" ? c.portalOpening : c.portal}
              </button>
            </div>
            <p className="orders-hint">{c.portalHint}</p>
          </>
        )}
      </section>

      <section className="customers-section" aria-label={c.plansTitle}>
        <h2>{c.plansTitle}</h2>
        {live && <p className="orders-hint">{c.plansHave}</p>}
        {status.plans.length === 0 ? (
          <p className="orders-empty">{c.plansNone}</p>
        ) : (
          <ul className="billing-plans" data-testid="billing-plans">
            {status.plans.map((plan) => (
              <li key={plan.price_id} data-testid={`plan-${plan.price_id}`}>
                <span>
                  <strong>{plan.name}</strong>
                  {money(locale, plan.currency, plan.amount_minor)} / {c.per[plan.interval] ?? plan.interval}
                </span>
                <button type="button" className="primary" data-testid={`plan-choose-${plan.price_id}`} disabled={busy !== "" || live}
                  onClick={() => void redirect("checkout", () => startCheckout(store, plan.price_id, boundary))}>
                  {busy === "checkout" ? c.choosing : c.choose}
                </button>
              </li>
            ))}
          </ul>
        )}
        {problem && <p className="orders-bad" role="alert" data-testid="billing-problem">{problem}</p>}
      </section>

      <section className="customers-section" aria-label={c.usageTitle}>
        <h2>{c.usageTitle}</h2>
        <div className="orders-actions-scroll">
          <table className="orders-actions-table" data-testid="billing-usage">
            <tbody>
              <tr><th scope="row">{c.usagePeriod}</th><td>{status.usage.period_start.slice(0, 10)} → {status.usage.period_end.slice(0, 10)}</td></tr>
              <tr><th scope="row">{c.paidOrders}</th><td>{status.usage.paid_orders}</td></tr>
              <tr><th scope="row">{c.claimWindows}</th><td>{status.usage.claim_windows_opened}</td></tr>
              <tr><th scope="row">{c.privateReplies}</th><td>{status.usage.private_replies_sent}</td></tr>
              <tr><th scope="row">{c.members}</th><td>{status.usage.members}</td></tr>
            </tbody>
          </table>
        </div>
      </section>
    </>
  );
}
