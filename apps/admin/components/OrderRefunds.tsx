"use client";

// Refund section of the merchant order detail row (contracts/stripe-refund-v1.md §7.1 "Admin UI").
// BFF routes: GET|POST /api/stores/{store}/orders/{id}/refunds, POST .../refunds/{refund}/refresh (keyless)
// -> Go internal/httpapi/refunds.go (GET orders:read; POST payments:refund). Money authority stays in Go/SQL:
// the amount check here is only a hint, and no success is shown before the server state is re-read (GET).

import { Fragment, useEffect, useRef, useState } from "react";
import type { Locale } from "@live-commerce/i18n";
import { money } from "@/lib/client";
import { postRefund, readRefunds, refreshRefund } from "@/lib/orders-client";
import {
  amountToMinor,
  displayTime,
  minorToInput,
  refundAmountOK,
  refundReasons,
  wholeOnly,
  type OrderDetail,
  type RefundItem,
  type RefundList,
  type RefundReason,
  type RefundState,
} from "@/lib/orders-model";
import type { OrdersCopy } from "@/lib/orders-copy";
import "./order-actions.css";

const tone = (state: RefundState) =>
  state === "SUCCEEDED"
    ? "success"
    : state === "FAILED" || state === "CANCELED" || state === "REJECTED" || state === "UNKNOWN"
      ? "warning"
      : "neutral";
// Non-terminal for the merchant: a refresh may still move it (UNKNOWN is resolved by the server's reconcile/manual path).
const refreshable = (state: RefundState) =>
  state === "REQUESTED" || state === "SUBMITTING" || state === "PENDING" || state === "UNKNOWN";

export function errorText(c: OrdersCopy, code: string) {
  return (c.errors as Record<string, string>)[code] ?? c.errors.default;
}

export function OrderRefunds({
  store,
  detail,
  locale,
  c,
  canRefund,
  boundary,
  onChanged,
}: {
  store: string;
  detail: OrderDetail;
  locale: Locale;
  c: OrdersCopy;
  canRefund: boolean;
  boundary: string;
  onChanged: () => Promise<boolean>;
}) {
  const orderID = detail.order_id;
  const currency = detail.currency;
  const m = (value: number) => money(locale, currency, value);
  const [list, setList] = useState<RefundList | null>(null);
  const [status, setStatus] = useState<"loading" | "ready" | "error">("loading");
  const [tick, setTick] = useState(0);
  const [notice, setNotice] = useState("");
  const [refreshing, setRefreshing] = useState("");
  const [open, setOpen] = useState(false);
  const [step, setStep] = useState<"form" | "confirm">("form");
  const [amountText, setAmountText] = useState("");
  const [reason, setReason] = useState<RefundReason>("requested_by_customer");
  const [problem, setProblem] = useState("");
  const [busy, setBusy] = useState(false);
  const [uncertain, setUncertain] = useState(false);
  const dialog = useRef<HTMLDialogElement>(null);
  const amountInput = useRef<HTMLInputElement>(null);
  const confirmText = useRef<HTMLParagraphElement>(null);
  // One key per submission, reused only for a byte-identical retry of that submission.
  const pending = useRef<{ key: string; body: string } | null>(null);

  useEffect(() => {
    const active = new AbortController();
    readRefunds(store, orderID, currency, active.signal).then(
      (value) => {
        setList(value);
        setStatus("ready");
      },
      () => {
        if (active.signal.aborted) return;
        // Never keep a stale refundable amount when the re-GET failed.
        setList(null);
        setStatus("error");
      },
    );
    return () => active.abort();
  }, [store, orderID, currency, tick, detail.updated_at]);

  useEffect(() => {
    const element = dialog.current;
    if (!element) return;
    if (open && !element.open) element.showModal();
    if (!open && element.open) element.close();
  }, [open]);

  // Step change unmounts the focused control; park focus on non-submit content so a repeated Enter/Space
  // cannot activate "Confirm refund" before the restated amount was shown (§7.1 two-step confirmation).
  useEffect(() => {
    if (!open) return;
    if (step === "confirm") confirmText.current?.focus();
    else amountInput.current?.focus();
  }, [open, step]);

  const refundable = list?.refundable_minor ?? 0;
  const whole = wholeOnly(currency);
  const typed = amountToMinor(amountText, currency);
  const amountOK = refundAmountOK(typed, refundable, currency);

  function begin() {
    if (!list) return;
    pending.current = null;
    setAmountText(minorToInput(list.refundable_minor, currency));
    setReason("requested_by_customer");
    setStep("form");
    setProblem("");
    setUncertain(false);
    setNotice("");
    setOpen(true);
  }
  function finish() {
    setOpen(false);
    // Closing after an unknown outcome still re-reads the server state.
    if (uncertain) setTick((value) => value + 1);
    setUncertain(false);
  }
  async function submit() {
    if (!list || busy || !amountOK) return;
    const body = JSON.stringify({
      amount_minor: typed,
      reason,
      expected_refundable_minor: list.refundable_minor,
    });
    if (pending.current?.body !== body) pending.current = { key: `refund-${crypto.randomUUID()}`, body };
    setBusy(true);
    setProblem("");
    const result = await postRefund(store, orderID, pending.current.key, body, boundary);
    setBusy(false);
    if (result.ok) {
      pending.current = null;
      setOpen(false);
      setUncertain(false);
      setNotice(c.refundDone);
      setTick((value) => value + 1);
      void onChanged();
      return;
    }
    if (result.uncertain) {
      // Same key + same body: a retry replays the stored answer instead of double-refunding.
      setUncertain(true);
      setProblem(c.refundUncertain);
      return;
    }
    pending.current = null;
    setProblem(errorText(c, result.code));
    if (result.code === "refundable_changed" || result.code === "exceeds_refundable") {
      setStep("form");
      setTick((value) => value + 1);
    }
  }
  async function refresh(item: RefundItem) {
    if (refreshing) return;
    setRefreshing(item.refund_id);
    setNotice("");
    const result = await refreshRefund(store, orderID, item.refund_id, boundary);
    setRefreshing("");
    setNotice(
      result === "scheduled" ? c.refundRefreshed : result === "later" ? c.refundRefreshLater : errorText(c, result.code),
    );
    setTick((value) => value + 1);
    void onChanged();
  }

  return (
    <section className="orders-section" data-testid="order-refunds" aria-label={c.refundTitle}>
      <h2>{c.refundTitle}</h2>
      {status === "loading" && !list && <p className="orders-empty" role="status">{c.sectionLoading}</p>}
      {status === "error" && (
        <div role="status">
          <p>{c.sectionUnavailable}</p>
          <button type="button" onClick={() => setTick((value) => value + 1)}>{c.retry}</button>
        </div>
      )}
      {list && (
        <>
          <dl className="orders-facts" data-testid="refund-summary">
            <div><dt>{c.refundCaptured}</dt><dd>{m(list.captured_minor)}</dd></div>
            <div><dt>{c.refundRefunded}</dt><dd data-testid="refund-refunded">{m(list.refunded_minor)}</dd></div>
            <div><dt>{c.refundInProgress}</dt><dd data-testid="refund-pending">{m(list.pending_minor)}</dd></div>
            <div><dt>{c.refundRefundable}</dt><dd data-testid="refund-refundable">{m(list.refundable_minor)}</dd></div>
          </dl>
          {list.items.length === 0 ? (
            <p className="orders-empty">{c.refundNone}</p>
          ) : (
            <div className="orders-actions-scroll">
              <table className="orders-actions-table" data-testid="refund-list">
                <thead>
                  <tr>
                    <th>{c.refundColAmount}</th>
                    <th>{c.refundColReason}</th>
                    <th>{c.refundColStatus}</th>
                    <th>{c.refundColRequested}</th>
                    <th><span className="orders-sr">{c.refundRefresh}</span></th>
                  </tr>
                </thead>
                <tbody>
                  {list.items.map((item) => (
                    <tr key={item.refund_id} data-testid={`refund-${item.refund_id}`}>
                      <td>{m(item.amount_minor)}</td>
                      <td>{c.refundReasons[item.reason]}</td>
                      <td>
                        <span className={`orders-badge orders-tone-${tone(item.state)}`} data-state={item.state}>
                          {c.refundStates[item.state]}
                        </span>
                        {item.failure_reason && <small>{c.failureReasons[item.failure_reason]}</small>}
                        <small>
                          {c.refundColStripe}: <span className="orders-mono">{item.stripe_refund_id ?? "—"}</span>
                        </small>
                      </td>
                      <td>{displayTime(locale, item.requested_at)}</td>
                      <td>
                        {canRefund && refreshable(item.state) && (
                          <button
                            type="button"
                            className="orders-compact"
                            data-testid={`refund-refresh-${item.refund_id}`}
                            disabled={refreshing !== ""}
                            onClick={() => void refresh(item)}
                          >
                            {c.refundRefresh}
                          </button>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          {canRefund && list.refundable_minor > 0 && (
            <button type="button" className="orders-section-action" data-testid="refund-open" onClick={begin}>
              {c.refundButton}
            </button>
          )}
        </>
      )}
      {notice && <p className="orders-notice" role="status" data-testid="refund-notice">{notice}</p>}
      <dialog
        ref={dialog}
        className="orders-dialog"
        aria-labelledby={`refund-title-${orderID}`}
        data-testid="refund-dialog"
        onClose={() => {
          if (open) finish();
        }}
      >
        {open && list && (
          <form
            onSubmit={(event) => {
              event.preventDefault();
              if (step === "form") {
                if (amountOK) setStep("confirm");
              } else void submit();
            }}
          >
            <h2 id={`refund-title-${orderID}`}>{step === "form" ? c.refundDialogTitle : c.refundConfirmTitle}</h2>
            {step === "form" ? (
              <>
                <label>
                  {c.refundAmount} ({currency})
                  <input
                    ref={amountInput}
                    data-testid="refund-amount"
                    inputMode="decimal"
                    autoComplete="off"
                    value={amountText}
                    aria-invalid={amountText !== "" && !amountOK}
                    aria-describedby={`refund-hint-${orderID}`}
                    onChange={(event) => setAmountText(event.target.value)}
                  />
                </label>
                <small id={`refund-hint-${orderID}`} className={amountText !== "" && !amountOK ? "orders-bad" : ""}>
                  {amountText !== "" && !amountOK
                    ? c.refundAmountInvalid(m(refundable), whole)
                    : c.refundAmountHint(m(refundable), whole)}
                </small>
                <label>
                  {c.refundReason}
                  <select
                    data-testid="refund-reason"
                    value={reason}
                    onChange={(event) => setReason(event.target.value as RefundReason)}
                  >
                    {refundReasons.map((value) => (
                      <option key={value} value={value}>{c.refundReasons[value]}</option>
                    ))}
                  </select>
                </label>
              </>
            ) : (
              <p ref={confirmText} tabIndex={-1} data-testid="refund-confirm-text">{c.refundConfirm(m(typed ?? 0), currency)}</p>
            )}
            {problem && <p className="orders-bad" role="alert" data-testid="refund-problem">{problem}</p>}
            <div className="orders-dialog-actions">
              {step === "form" ? (
                // Distinct keys: the confirm buttons must be new DOM elements, never the focused "Review" button relabelled.
                <Fragment key="form">
                  <button type="button" onClick={finish}>{c.cancel}</button>
                  <button type="submit" className="primary" data-testid="refund-continue" disabled={!amountOK}>
                    {c.refundContinue}
                  </button>
                </Fragment>
              ) : (
                <Fragment key="confirm">
                  <button type="button" disabled={busy || uncertain} onClick={() => setStep("form")}>{c.refundBack}</button>
                  <button type="submit" className="primary" data-testid="refund-submit" disabled={busy}>
                    {busy ? c.refundSending : uncertain ? c.refundRetry : c.refundSubmit}
                  </button>
                  {uncertain && <button type="button" onClick={finish}>{c.close}</button>}
                </Fragment>
              )}
            </div>
          </form>
        )}
      </dialog>
    </section>
  );
}
