"use client";

// CVS section of the merchant order detail row, next to the 0063 shipment section (approved C inline row, U1: no new
// component language). BFF routes (lib/logistics-client.ts) -> Go internal/httpapi/cvs.go (taiwan-cvs-logistics-v1 §8):
//   GET  /api/stores/{store}/orders/{id}/cvs-shipment                  (orders:read)
//   POST .../cvs-shipment (create, Idempotency-Key), .../cvs-shipment/abandon (key), .../collection (key),
//   POST .../pay-at-pickup-release (key)                                (fulfillment:write)
//   the label print goes through the new-tab page app/[locale]/orders/cvs-print (BFF POST .../cvs-shipment/print-form).
// Nothing here is optimistic: every write re-GETs the shipment and re-reads the order (onChanged). One
// Idempotency-Key per dialog open, reused only for a byte-identical retry after an unknown outcome. A CREATED
// label is never called "shipped": ECPay shows only what it reports, and the money/eligibility rules stay in SQL.

import { useEffect, useRef, useState } from "react";
import type { Locale } from "@live-commerce/i18n";
import {
  postCollection,
  postCvsAbandon,
  postCvsShipment,
  postRelease,
  readCvsShipment,
  type WriteResult,
} from "@/lib/logistics-client";
import {
  abandonBody,
  collectionActions,
  collectionBody,
  currentAttempt,
  isCollectionState,
  labelLapsed,
  releaseBody,
  requestBody,
  type CollectionAction,
  type CvsShipment,
  type ReleaseAction,
} from "@/lib/logistics-model";
import { logisticsCopy, logisticsError } from "@/lib/logistics-copy";
import { displayTime, type OrderDetail } from "@/lib/orders-model";
import type { OrdersCopy } from "@/lib/orders-copy";
import "./order-actions.css";

type Action = "create" | "abandon" | CollectionAction | ReleaseAction;
type Open = { action: Action; expectedVersion: number };
const POLL_MS = 5000;
const POLL_MAX = 24; // two minutes of watching a REQUESTED attempt; the merchant can refresh after that

export function OrderCvsShipment({
  store,
  detail,
  locale,
  c,
  canWrite,
  boundary,
  onChanged,
}: {
  store: string;
  detail: OrderDetail;
  locale: Locale;
  c: OrdersCopy;
  canWrite: boolean;
  boundary: string;
  onChanged: () => Promise<boolean>;
}) {
  const lc = logisticsCopy[locale];
  const orderID = detail.order_id;
  const ecpay = detail.pickup_source === "ecpay_directory";
  const payAtPickup = detail.payment_mode === "pay_at_pickup";
  const collection = isCollectionState(detail.collection_state) ? detail.collection_state : null;
  const [shipment, setShipment] = useState<CvsShipment | null>(null);
  const [status, setStatus] = useState<"idle" | "loading" | "ready" | "error">(ecpay ? "loading" : "idle");
  const [tick, setTick] = useState(0);
  const [thermal, setThermal] = useState(false);
  const [open, setOpen] = useState<Open | null>(null);
  const [ack, setAck] = useState(false);
  const [problem, setProblem] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);
  const [uncertain, setUncertain] = useState(false);
  const [copied, setCopied] = useState<"" | "ok" | "failed">("");
  const dialog = useRef<HTMLDialogElement>(null);
  // One key per dialog open; the body is fixed by the first send, so a changed body (the acknowledgement
  // checkbox) starts a new key while an identical retry replays the old one.
  const pending = useRef<{ key: string; body: string | null } | null>(null);
  const lastState = useRef<string | null>(null);

  useEffect(() => {
    if (!ecpay) return;
    const active = new AbortController();
    readCvsShipment(store, orderID, active.signal).then(
      (value) => {
        setShipment(value);
        setStatus("ready");
      },
      () => {
        if (active.signal.aborted) return;
        setShipment(null); // never keep a stale CAS version when the re-GET failed
        setStatus("error");
      },
    );
    return () => active.abort();
  }, [store, orderID, ecpay, tick, detail.updated_at]);

  const attempt = currentAttempt(shipment);
  const attemptState = attempt?.state ?? null;

  // A REQUESTED attempt settles asynchronously (dispatcher): watch it briefly, and refresh the order when it
  // leaves REQUESTED so the order badge (PROVIDER_LABEL_CREATED) follows the server.
  useEffect(() => {
    if (lastState.current === "REQUESTED" && attemptState !== "REQUESTED") void onChanged();
    lastState.current = attemptState;
    if (attemptState !== "REQUESTED") return;
    let n = 0;
    const timer = window.setInterval(() => {
      if (++n > POLL_MAX) window.clearInterval(timer);
      else setTick((value) => value + 1);
    }, POLL_MS);
    return () => window.clearInterval(timer);
  }, [attemptState, attempt?.attempt]);

  useEffect(() => {
    const element = dialog.current;
    if (!element) return;
    if (open && !element.open) element.showModal();
    if (!open && element.open) element.close();
  }, [open]);

  const refunded =
    detail.total_minor > 0 && detail.refunded_minor + detail.refund_pending_minor >= detail.total_minor;
  // Hints only (the definer decides, §4.3): CONFIRMED, unassigned, not fully refunded, pay-at-pickup still PENDING.
  const eligible =
    detail.commercial_state === "CONFIRMED" &&
    detail.fulfillment_state === "MANUAL_UNASSIGNED" &&
    !refunded &&
    (!payAtPickup || collection === "PENDING");
  const canCreate =
    canWrite && ecpay && status === "ready" && eligible &&
    (attempt === null || attempt.state === "FAILED" || attempt.state === "ABANDONED");
  const live = attempt !== null && !["FAILED", "ABANDONED"].includes(attempt.state);
  const lapsed = attempt !== null && labelLapsed(attempt, shipment?.validity_days ?? null, Date.now());
  const canAbandon =
    canWrite && attempt !== null && (attempt.state === "UNKNOWN" || (attempt.state === "CREATED" && lapsed));
  const actions = collectionActions(collection, detail.fulfillment_state);

  function begin(action: Action) {
    pending.current = { key: `cvs-${crypto.randomUUID()}`, body: null };
    setAck(false);
    setProblem("");
    setUncertain(false);
    setNotice("");
    setOpen({ action, expectedVersion: shipment?.expected_version ?? 0 });
  }
  function finish() {
    setOpen(null);
    // Closing after an unknown outcome still re-reads the server state.
    if (uncertain) {
      setTick((value) => value + 1);
      void onChanged();
    }
    setUncertain(false);
  }
  const bodyOf = (o: Open) => {
    switch (o.action) {
      case "create":
        return requestBody(o.expectedVersion);
      case "abandon":
        return abandonBody(o.expectedVersion, attempt?.state === "UNKNOWN" && ack);
      case "cancel":
      case "restock":
        return releaseBody(o.action, o.action === "cancel" ? "PENDING" : "RETURNED");
      default:
        // collected/returned need PENDING; refunded_offline needs COLLECTED (the CAS token is the state we saw).
        return collectionBody(o.action === "refunded_offline" ? "COLLECTED" : "PENDING", o.action);
    }
  };
  async function send() {
    if (!open || busy || !pending.current) return;
    if (open.action === "abandon" && attempt?.state === "UNKNOWN" && !ack) return;
    const body = bodyOf(open);
    if (pending.current.body !== null && pending.current.body !== body)
      pending.current = { key: `cvs-${crypto.randomUUID()}`, body };
    else pending.current.body = body;
    const { key } = pending.current;
    setBusy(true);
    setProblem("");
    let result: WriteResult;
    if (open.action === "create") result = await postCvsShipment(store, orderID, key, body, boundary);
    else if (open.action === "abandon") result = await postCvsAbandon(store, orderID, key, body, boundary);
    else if (open.action === "cancel" || open.action === "restock")
      result = await postRelease(store, orderID, key, body, boundary);
    else result = await postCollection(store, orderID, key, body, boundary);
    setBusy(false);
    // ecpay_trade_found: ECPay already had the trade; the definer committed CREATED (a returned outcome).
    if (result.ok || (!result.ok && result.code === "ecpay_trade_found")) {
      const done = open.action;
      pending.current = null;
      setUncertain(false);
      setOpen(null);
      setNotice(
        !result.ok ? lc.tradeFound : done === "create" ? lc.createDone : done === "abandon" ? lc.manualDone : lc.payDone,
      );
      setTick((value) => value + 1);
      void onChanged();
      return;
    }
    if (result.uncertain) {
      setUncertain(true);
      setProblem(lc.uncertain);
      return;
    }
    pending.current = null;
    setProblem(logisticsError(lc, c, result.code));
    if (
      ["version_changed", "version_conflict", "collection_state_changed", "cvs_attempt_in_flight", "not_shippable"].includes(
        result.code,
      )
    ) {
      setTick((value) => value + 1);
      void onChanged();
    }
  }
  async function copyCode(code: string) {
    try {
      await navigator.clipboard.writeText(code);
      setCopied("ok");
    } catch {
      setCopied("failed");
    }
  }

  const confirmText = (o: Open) =>
    o.action === "create"
      ? lc.createConfirm(shipment?.validity_days ?? null)
      : o.action === "abandon"
        ? lc.manualConfirm
        : o.action === "collected"
          ? lc.confirmCollected
          : o.action === "returned"
            ? lc.confirmReturned
            : o.action === "refunded_offline"
              ? lc.confirmRefundedOffline
              : o.action === "cancel"
                ? lc.confirmCancel
                : lc.confirmRestock;
  const titleOf = (o: Open) =>
    o.action === "create" ? lc.createTitle : o.action === "abandon" ? lc.manualTitle : lc.confirmTitle;
  const submitOf = (o: Open) =>
    o.action === "create" ? lc.createSubmit : o.action === "abandon" ? lc.manualSubmit : lc.confirmSubmit;
  const printHref = `/${locale}/orders/cvs-print?store=${store}&order=${orderID}&thermal=${thermal ? 1 : 0}`;

  return (
    <section className="orders-section" data-testid="order-cvs" aria-label={lc.secTitle}>
      <h2>{lc.secTitle}</h2>
      {status === "error" && (
        <div role="status">
          <p>{lc.secLoadFailed}</p>
          <button type="button" onClick={() => setTick((value) => value + 1)}>{lc.retry}</button>
        </div>
      )}
      {!ecpay && detail.pickup_source === "buyer_entered" && (
        <p className="orders-hint" data-testid="cvs-manual-only">{lc.manualOnly}</p>
      )}

      {attempt && (
        <>
          {refunded && attempt.state === "CREATED" && (
            <p className="orders-bad" role="alert" data-testid="cvs-refunded-warning">{lc.refundedWarn}</p>
          )}
          {attempt.state === "UNKNOWN" && (
            <p className="orders-bad" role="alert" data-testid="cvs-unknown-banner">{lc.unknownBanner}</p>
          )}
          {attempt.state === "FAILED" && (
            <p className="orders-bad" role="status" data-testid="cvs-failed-banner">{lc.failedBanner}</p>
          )}
          {attempt.alerts.map((code) => (
            <p key={code} className="orders-bad" role="alert" data-testid={`cvs-alert-${code}`}>
              {lc.alerts[code as keyof typeof lc.alerts] ?? lc.alertGeneric}
            </p>
          ))}
          <dl className="orders-facts" data-testid="cvs-attempt">
            <div>
              <dt>{lc.attemptTitle} {attempt.attempt}</dt>
              <dd>
                <span className="orders-badge" data-state={attempt.state}>{lc.shipmentStates[attempt.state]}</span>
              </dd>
            </div>
            <div><dt>{lc.goods}</dt><dd>NT$ {attempt.goods_amount}</dd></div>
            {attempt.collection_amount !== null && <div><dt>{lc.collect}</dt><dd>NT$ {attempt.collection_amount}</dd></div>}
            <div><dt>{lc.updated}</dt><dd>{displayTime(locale, attempt.updated_at)}</dd></div>
            {attempt.state !== "REQUESTED" && attempt.state !== "FAILED" && attempt.state !== "ABANDONED" && (
              <div>
                <dt>{lc.code}</dt>
                <dd>
                  {attempt.code ? (
                    <>
                      <span className="orders-mono" data-testid="cvs-code">{attempt.code}</span>{" "}
                      <button type="button" className="orders-compact" data-testid="cvs-copy-code" onClick={() => void copyCode(attempt.code!)}>
                        {lc.copy}
                      </button>
                    </>
                  ) : (
                    <span className="orders-hint">{lc.codeMissing}</span>
                  )}
                </dd>
              </div>
            )}
          </dl>
          {copied && (
            <p className="orders-notice" role="status">{copied === "ok" ? lc.copied : lc.copyFailed}</p>
          )}
          {canWrite && attempt.print_available && (
            <div className="orders-form-actions">
              <label className="orders-hint">
                <input type="checkbox" checked={thermal} onChange={(event) => setThermal(event.target.checked)} />{" "}
                {lc.printThermal}
              </label>
              <a
                className="orders-export"
                data-testid="cvs-print"
                href={printHref}
                target="_blank"
                rel="noopener noreferrer"
              >
                {lc.printButton}
              </a>
              <p className="orders-hint">{lc.printHint}</p>
            </div>
          )}
          {attempt.state === "CREATED" && !attempt.print_available && attempt.subtype.startsWith("OKMART") && (
            <p className="orders-hint">{lc.printUnsupported}</p>
          )}
          {canAbandon && (
            <div className="orders-form-actions">
              <button type="button" className="orders-compact" data-testid="cvs-abandon" onClick={() => begin("abandon")}>
                {lc.manualButton}
              </button>
            </div>
          )}
          <details className="orders-history" data-testid="cvs-timeline">
            <summary>{lc.events} ({attempt.events.length})</summary>
            {attempt.events.length === 0 ? (
              <p>{lc.noEvents}</p>
            ) : (
              <ol>
                {[...attempt.events].reverse().map((item, index) => (
                  <li key={`${item.received_at}-${index}`}>
                    <strong>{item.event_code}</strong>
                    <span>
                      {displayTime(locale, item.received_at)} · {item.source}
                      {item.provider_code ? ` · ${item.provider_code}` : ""}
                      {item.from_state || item.to_state ? ` · ${item.from_state ?? "—"} → ${item.to_state ?? "—"}` : ""}
                    </span>
                    {item.provider_message && <span>{item.provider_message}</span>}
                  </li>
                ))}
              </ol>
            )}
          </details>
        </>
      )}
      {ecpay && status === "ready" && !live && canWrite && !canCreate && attempt === null && (
        <p className="orders-hint" data-testid="cvs-not-eligible">{lc.createNotEligible}</p>
      )}
      {canCreate && (
        <div className="orders-form-actions">
          <button type="button" className="primary" data-testid="cvs-create" onClick={() => begin("create")}>
            {lc.createButton}
          </button>
        </div>
      )}

      {payAtPickup && collection && (
        <div data-testid="cvs-collection">
          <h2>{lc.payTitle}</h2>
          <dl className="orders-facts">
            <div>
              <dt>{lc.payState}</dt>
              <dd data-testid="cvs-collection-state" data-state={collection}>{lc.payStates[collection]}</dd>
            </div>
          </dl>
          <p className="orders-hint">{lc.payNote}</p>
          {canWrite && (
            <div className="orders-form-actions">
              {actions.collected && (
                <button type="button" data-testid="cvs-collected" onClick={() => begin("collected")}>{lc.collected}</button>
              )}
              {actions.returned && (
                <button type="button" data-testid="cvs-returned" onClick={() => begin("returned")}>{lc.returned}</button>
              )}
              {actions.refunded_offline && (
                <button type="button" data-testid="cvs-refunded-offline" onClick={() => begin("refunded_offline")}>
                  {lc.refundedOffline}
                </button>
              )}
              {actions.cancel && (
                <button type="button" data-testid="cvs-cancel-order" onClick={() => begin("cancel")}>{lc.cancelOrder}</button>
              )}
              {actions.restock && (
                <button type="button" data-testid="cvs-restock" onClick={() => begin("restock")}>{lc.restockOrder}</button>
              )}
            </div>
          )}
        </div>
      )}
      {notice && <p className="orders-notice" role="status" data-testid="cvs-notice">{notice}</p>}
      <dialog
        ref={dialog}
        className="orders-dialog"
        aria-labelledby={`cvs-title-${orderID}`}
        data-testid="cvs-dialog"
        onClose={() => {
          if (open) finish();
        }}
      >
        {open && (
          <form
            onSubmit={(event) => {
              event.preventDefault();
              void send();
            }}
          >
            <h2 id={`cvs-title-${orderID}`}>{titleOf(open)}</h2>
            <p data-testid="cvs-confirm-text">{confirmText(open)}</p>
            {open.action === "abandon" && attempt?.state === "UNKNOWN" && (
              <label>
                <span>
                  <input
                    type="checkbox"
                    data-testid="cvs-ack"
                    checked={ack}
                    disabled={busy || uncertain}
                    onChange={(event) => setAck(event.target.checked)}
                  />{" "}
                  {lc.manualChecked}
                </span>
              </label>
            )}
            {problem && <p className="orders-bad" role="alert" data-testid="cvs-problem">{problem}</p>}
            <div className="orders-dialog-actions">
              <button type="button" onClick={finish}>{uncertain ? lc.close : lc.cancel}</button>
              <button
                type="submit"
                className="primary"
                data-testid="cvs-submit"
                disabled={busy || (open.action === "abandon" && attempt?.state === "UNKNOWN" && !ack)}
              >
                {busy ? lc.saving : uncertain ? lc.retrySame : submitOf(open)}
              </button>
            </div>
          </form>
        )}
      </dialog>
    </section>
  );
}
