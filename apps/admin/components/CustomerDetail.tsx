"use client";

// Merchant customer detail (/{locale}/customers/{customer}): facts, orders, claims, consent with "Record withdrawal",
// privacy section ("Download data", typed-ERASE dialog). U1: inline sections reusing the orders table/dialog shapes.
// BFF GET /api/stores/{store}/customers/{id} (customers:read); POST .../customers/{id}/consent-withdrawals | exports |
// erasure (customers:privacy, Idempotency-Key) -> Go /v1/admin/stores/{id}/customers/{id}[/...]
// (internal/httpapi/customers.go; internal/customers.{Get,WithdrawConsent,Export,Erase}).
// No consent, erasure or money authority here: every write is re-authorized by Go and the page re-GETs afterwards.
import { useEffect, useRef, useState } from "react";
import Link from "next/link";
import type { Locale } from "@live-commerce/i18n";
import type { Store } from "@/lib/model";
import { money } from "@/lib/client";
import {
  postErasure,
  postExport,
  postWithdrawal,
  readCustomer,
  useGuardedRead,
  type ReadCode,
  type WriteOutcome,
} from "@/lib/customers-client";
import { consentPairs, newKey, type CustomerDetail as Detail } from "@/lib/customers-model";
import { displayTime } from "@/lib/orders-model";
import { ordersCopy } from "@/lib/orders-copy";
import { customersCopy, type CustomersCopy } from "@/lib/customers-copy";
import { WorkspaceFrame } from "./WorkspaceFrame";
import "./orders.css";
import "./order-actions.css";
import "./customers.css";

type Notice = { tone: "ok" | "bad"; text: string } | null;
const errorText = (c: CustomersCopy, code: string) => c.errors[code] ?? c.errors.default;

export function CustomerDetail({
  locale,
  stores,
  store,
  customerID,
  initialError,
  renderKey,
}: {
  locale: Locale;
  stores: Store[];
  store: Store | null;
  customerID: string;
  initialError: ReadCode | null;
  renderKey: string;
}) {
  const c = customersCopy[locale];
  const read = useGuardedRead(
    `${renderKey}|${locale}|${store?.id ?? ""}|${customerID}`,
    store ? (signal) => readCustomer(store.id, customerID, signal) : null,
    initialError,
  );
  const failure =
    read.status === "signed-out" ? c.signedOut
    : read.status === "forbidden" ? c.forbidden
    : read.status === "not-found" ? (store ? c.notFound : c.noStore)
    : read.status === "unavailable" ? c.unavailable
    : "";
  const back = `/${locale}/customers${store ? `?store=${store.id}` : ""}`;
  return (
    <WorkspaceFrame locale={locale} storeName={store?.name ?? c.noStore} active="customers">
      <div className="orders-page customers-page" data-testid="customer-detail">
        <header className="orders-heading">
          <h1>{read.data ? (read.data.display_name ?? c.noName) : c.title}</h1>
          <p>{c.subtitle}</p>
        </header>
        <Link className="customers-back" href={back}>{c.back}</Link>
        {(read.status === "loading" || read.status === "hidden") && (
          <p className="orders-message" role="status">{c.detailLoading}</p>
        )}
        {failure && (
          <div className="orders-message" role="status">
            <p>{failure}</p>
            <button type="button" onClick={read.reload}>{c.retry}</button>
          </div>
        )}
        {read.status === "ready" && read.data && store && (
          <Body detail={read.data} store={store.id} boundary={read.boundary} refresh={read.refresh} locale={locale} c={c} />
        )}
      </div>
    </WorkspaceFrame>
  );
}

type Pending = {
  kind: string;
  key: string;
  run: (key: string) => Promise<WriteOutcome<unknown>>;
  done: string;
};

function Body({
  detail,
  store,
  boundary,
  refresh,
  locale,
  c,
}: {
  detail: Detail;
  store: string;
  boundary: string;
  refresh: () => Promise<boolean>;
  locale: Locale;
  c: CustomersCopy;
}) {
  const oc = ordersCopy[locale];
  const m = (value: number) => (detail.currency ? money(locale, detail.currency, value) : String(value));
  const [busy, setBusy] = useState("");
  const [notice, setNotice] = useState<Notice>(null);
  const [retryKind, setRetryKind] = useState("");
  // One key per attempt; only "Retry same request" after an unknown result reuses it (server replays the stored answer).
  const pending = useRef<Pending | null>(null);

  async function attempt(next: Pending) {
    if (busy) return;
    pending.current = next;
    setBusy(next.kind);
    setNotice(null);
    const result = await next.run(next.key);
    setBusy("");
    if (result.ok) {
      pending.current = null;
      setRetryKind("");
      setNotice({ tone: "ok", text: next.done });
      void refresh();
    } else if (result.uncertain) {
      setRetryKind(next.kind);
      setNotice({ tone: "bad", text: c.uncertain });
    } else {
      pending.current = null;
      setRetryKind("");
      setNotice({ tone: "bad", text: errorText(c, result.code) });
    }
  }
  const withdraw = (purpose: string, channel: string) =>
    attempt({
      kind: `withdraw:${purpose}`,
      key: newKey("withdraw"),
      done: c.withdrawDone,
      run: (key) => postWithdrawal(store, detail.customer_id, key, JSON.stringify({ purpose, channel }), boundary),
    });
  const download = () =>
    attempt({
      kind: "export",
      key: newKey("export"),
      done: c.downloadDone,
      run: (key) => postExport(store, detail.customer_id, key, boundary),
    });

  const label = (purpose: string) => (purpose === "marketing_messages" ? c.marketing : c.ads);
  return (
    <>
      {!detail.active && <p className="customers-erased" role="status" data-testid="customer-erased">{c.erasedState}</p>}
      <section className="customers-section" aria-label={c.facts}>
        <h2>{c.facts}</h2>
        <dl className="orders-facts">
          <div><dt>{c.firstSeen}</dt><dd>{displayTime(locale, detail.first_seen_at)}</dd></div>
          <div><dt>{c.lastActivity}</dt><dd>{displayTime(locale, detail.last_activity_at)}</dd></div>
          {detail.phone_last3 && <div><dt>{c.phoneEnding}</dt><dd>{detail.phone_last3}</dd></div>}
          <div><dt>{c.orders}</dt><dd>{c.ordersPaid(detail.orders_count, detail.paid_orders_count)}</dd></div>
          {detail.currency && (
            <div><dt>{c.spent}</dt><dd>{m(detail.captured_minor)} / {m(detail.refunded_minor)}</dd></div>
          )}
          <div><dt>{c.claims}</dt><dd>{detail.claims_count}</dd></div>
        </dl>
      </section>

      <section className="customers-section" aria-label={c.ordersSection}>
        <h2>{c.ordersSection}</h2>
        {detail.orders.length === 0 ? (
          <p className="orders-empty">{c.ordersNone}</p>
        ) : (
          <div className="orders-actions-scroll">
            <table className="orders-actions-table" data-testid="customer-orders">
              <thead>
                <tr><th>{c.orderId}</th><th>{c.created}</th><th>{c.total}</th><th>{c.payment}</th><th>{c.status}</th></tr>
              </thead>
              <tbody>
                {detail.orders.map((order) => (
                  <tr key={order.order_id} data-testid={`customer-order-${order.order_id}`}>
                    <td>
                      <Link href={`/${locale}/orders?store=${store}&order=${order.order_id}`}
                        aria-label={`${c.orderOpen}: ${order.order_id}`}>
                        <span className="orders-mono">{order.order_id.slice(0, 4)}…{order.order_id.slice(-4)}</span>
                      </Link>
                    </td>
                    <td>{displayTime(locale, order.created_at)}</td>
                    <td>{money(locale, order.currency, order.total_minor)}</td>
                    <td><span className={`orders-badge orders-badge-${order.payment_state.toLowerCase()}`}>{oc.statuses[order.payment_state]}</span></td>
                    <td><span className={`orders-badge orders-badge-${order.commercial_state.toLowerCase()}`}>{oc.statuses[order.commercial_state]}</span></td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        <p className="orders-hint">{c.ordersNewest}</p>
      </section>

      <section className="customers-section" aria-label={c.claimsSection}>
        <h2>{c.claimsSection}</h2>
        {detail.claims.length === 0 ? (
          <p className="orders-empty">{c.claimsNone}</p>
        ) : (
          <div className="orders-actions-scroll">
            <table className="orders-actions-table" data-testid="customer-claims">
              <thead><tr><th>{c.platform}</th><th>{c.boundAt}</th><th>{c.lines}</th></tr></thead>
              <tbody>
                {detail.claims.map((claim) => (
                  <tr key={`${claim.session_id}-${claim.bound_at}`}>
                    <td>{c.platforms[claim.platform] ?? claim.platform}</td>
                    <td>{displayTime(locale, claim.bound_at)}</td>
                    <td>{claim.line_count}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      <section className="customers-section" aria-label={c.consentSection} data-testid="customer-consent">
        <h2>{c.consentSection}</h2>
        <ul className="customers-consent">
          {consentPairs.map(({ purpose, channel }) => {
            const on = detail.consents[purpose];
            return (
              <li key={purpose} data-testid={`consent-${purpose}`}>
                <span>{label(purpose)}</span>
                <span className={`orders-badge ${on ? "orders-tone-success" : "orders-tone-neutral"}`}>
                  {on ? c.consentGranted : c.consentNone}
                </span>
                {on && detail.active && (
                  <button type="button" data-testid={`withdraw-${purpose}`} disabled={busy !== ""}
                    onClick={() => void withdraw(purpose, channel)}>
                    {busy === `withdraw:${purpose}` ? c.withdrawing : c.withdraw}
                  </button>
                )}
              </li>
            );
          })}
        </ul>
        <p className="orders-hint">{c.withdrawHint}</p>
        <h3>{c.consentHistory}</h3>
        {detail.consent_history.length === 0 ? (
          <p className="orders-empty">{c.consentHistoryNone}</p>
        ) : (
          <div className="orders-actions-scroll">
            <table className="orders-actions-table" data-testid="consent-history">
              <thead>
                <tr><th>{c.when}</th><th>{c.consentSection}</th><th>{c.source}</th><th>{c.policy}</th></tr>
              </thead>
              <tbody>
                {detail.consent_history.map((event, index) => (
                  <tr key={`${event.occurred_at}-${index}`}>
                    <td>{displayTime(locale, event.occurred_at)}</td>
                    <td>
                      {label(event.purpose)}:{" "}
                      <span className={`orders-badge ${event.granted ? "orders-tone-success" : "orders-tone-neutral"}`}>
                        {event.granted ? c.consentGranted : c.consentNone}
                      </span>
                    </td>
                    <td>{c.sources[event.source] ?? event.source}</td>
                    <td>{event.policy_version}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      <section className="customers-section" aria-label={c.privacySection} data-testid="customer-privacy">
        <h2>{c.privacySection}</h2>
        <p>{c.privacyIntro}</p>
        <div className="customers-actions">
          <button type="button" data-testid="customer-download" disabled={busy !== ""} onClick={() => void download()}>
            {busy === "export" ? c.downloading : c.download}
          </button>
          {detail.active && <EraseDialog detail={detail} store={store} boundary={boundary} c={c} busy={busy !== ""}
            onDone={(text) => { setNotice({ tone: "ok", text }); void refresh(); }} />}
        </div>
        {notice && (
          <p className={notice.tone === "ok" ? "orders-notice" : "orders-bad"} role={notice.tone === "ok" ? "status" : "alert"}
            data-testid="customer-notice">
            {notice.text}
          </p>
        )}
        {retryKind && pending.current && (
          <button type="button" data-testid="customer-retry" disabled={busy !== ""}
            onClick={() => pending.current && void attempt({ ...pending.current })}>
            {c.eraseRetry}
          </button>
        )}
        <h3>{c.actionsSection}</h3>
        {detail.privacy_actions.length === 0 ? (
          <p className="orders-empty">{c.actionsNone}</p>
        ) : (
          <div className="orders-actions-scroll">
            <table className="orders-actions-table" data-testid="privacy-actions">
              <thead><tr><th>{c.when}</th><th>{c.actionCol}</th><th>{c.source}</th></tr></thead>
              <tbody>
                {detail.privacy_actions.map((action, index) => (
                  <tr key={`${action.completed_at}-${index}`}>
                    <td>{displayTime(locale, action.completed_at)}</td>
                    <td>{c.kinds[action.kind.toLowerCase()] ?? action.kind}</td>
                    <td>{c.via[action.via] ?? action.via}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
    </>
  );
}

// U2: native <dialog>; the typed word enables the button; one Idempotency-Key per dialog open, reused on retry after an
// unknown result (same key = the stored answer, never a second erasure). Text states what is kept.
function EraseDialog({
  detail,
  store,
  boundary,
  c,
  busy: otherBusy,
  onDone,
}: {
  detail: Detail;
  store: string;
  boundary: string;
  c: CustomersCopy;
  busy: boolean;
  onDone: (text: string) => void;
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  const typedInput = useRef<HTMLInputElement>(null);
  const [open, setOpen] = useState(false);
  const [typed, setTyped] = useState("");
  const [problem, setProblem] = useState("");
  const [busy, setBusy] = useState(false);
  const [uncertain, setUncertain] = useState(false);
  const key = useRef("");

  useEffect(() => {
    const element = dialog.current;
    if (!element) return;
    if (open && !element.open) element.showModal();
    if (!open && element.open) element.close();
    if (open) typedInput.current?.focus();
  }, [open]);

  function begin() {
    key.current = newKey("erase");
    setTyped("");
    setProblem("");
    setUncertain(false);
    setOpen(true);
  }
  async function submit() {
    if (typed !== "ERASE" || busy) return;
    setBusy(true);
    setProblem("");
    const result = await postErasure(store, detail.customer_id, key.current, boundary);
    setBusy(false);
    if (result.ok) {
      setOpen(false);
      const s = result.value;
      onDone(c.erasedDone({ consents: s.consents_withdrawn, sessions: s.sessions_revoked, snapshots: s.snapshots_redacted, bundles: s.bundles_relabelled }));
    } else if (result.uncertain) {
      setUncertain(true);
      setProblem(c.uncertain);
    } else {
      // A definite refusal (e.g. erasure_blocked) ends this key; the next attempt is a new request.
      key.current = newKey("erase");
      setUncertain(false);
      setProblem(errorText(c, result.code));
    }
  }
  return (
    <>
      <button type="button" className="danger" data-testid="customer-erase" disabled={otherBusy} onClick={begin}>
        {c.erase}
      </button>
      <dialog ref={dialog} className="orders-dialog" aria-labelledby="erase-title" data-testid="erase-dialog"
        onClose={() => setOpen(false)}>
        {open && (
          <form onSubmit={(event) => { event.preventDefault(); void submit(); }}>
            <h2 id="erase-title">{c.eraseTitle}</h2>
            <p>{c.eraseBody}</p>
            <p data-testid="erase-kept">{c.eraseKept}</p>
            <label>
              {c.eraseType}
              <input ref={typedInput} data-testid="erase-typed" autoComplete="off" autoCapitalize="off" spellCheck={false}
                value={typed} onChange={(event) => setTyped(event.target.value)} />
            </label>
            {problem && <p className="orders-bad" role="alert" data-testid="erase-problem">{problem}</p>}
            <div className="orders-dialog-actions">
              <button type="button" onClick={() => setOpen(false)}>{c.cancel}</button>
              <button type="submit" className="primary danger" data-testid="erase-submit" disabled={typed !== "ERASE" || busy}>
                {busy ? c.erasing : uncertain ? c.eraseRetry : c.eraseConfirm}
              </button>
            </div>
          </form>
        )}
      </dialog>
    </>
  );
}
