"use client";

// Shipment section of the merchant order detail row (contracts/manual-fulfilment-v1.md §5.1, §5.4).
// BFF routes: PUT /api/stores/{store}/orders/{id}/shipment (Idempotency-Key), GET .../shipment/history
// -> Go internal/httpapi/shipments.go (PUT fulfillment:write, GET orders:read). Record, correct and void are
// one command route; the server owns eligibility (MD6) and the version CAS. Nothing here is optimistic:
// after every response the order and history are re-read, and this UI never claims transit or delivery (MD3).

import { useEffect, useRef, useState } from "react";
import type { Locale } from "@live-commerce/i18n";
import { putShipment, readShipmentHistory } from "@/lib/orders-client";
import {
  carrierCodes,
  displayTime,
  trackingNumber,
  trackingURLHost,
  voidReasons,
  type CarrierCode,
  type OrderDetail,
  type Shipment,
  type ShipmentVersion,
  type VoidReason,
} from "@/lib/orders-model";
import type { OrdersCopy } from "@/lib/orders-copy";
import { errorText } from "./OrderRefunds";
import "./order-actions.css";

function carrierLabel(c: OrdersCopy, item: { carrier_code: CarrierCode; carrier_name: string | null }) {
  return item.carrier_name ?? c.carriers[item.carrier_code];
}

export function OrderShipment({
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
  const orderID = detail.order_id;
  const head: Shipment | null = detail.shipment;
  const [history, setHistory] = useState<ShipmentVersion[] | null>(null);
  const [status, setStatus] = useState<"loading" | "ready" | "error">("loading");
  const [tick, setTick] = useState(0);
  const [mode, setMode] = useState<"record" | "correct" | "void">("record");
  const [carrier, setCarrier] = useState<CarrierCode | "">("");
  const [carrierName, setCarrierName] = useState("");
  const [tracking, setTracking] = useState("");
  const [url, setURL] = useState("");
  const [note, setNote] = useState("");
  const [voidReason, setVoidReason] = useState<VoidReason>("wrong_order");
  const [problem, setProblem] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);
  const [uncertain, setUncertain] = useState(false);
  // One key per submission; only a byte-identical retry after an unknown outcome reuses it.
  const pending = useRef<{ key: string; body: string } | null>(null);

  useEffect(() => {
    const active = new AbortController();
    readShipmentHistory(store, orderID, active.signal).then(
      (value) => {
        setHistory(value);
        setStatus("ready");
      },
      () => {
        if (active.signal.aborted) return;
        setHistory(null);
        setStatus("error");
      },
    );
    return () => active.abort();
  }, [store, orderID, tick, detail.updated_at]);

  // The head version is the CAS token. After a void the detail shows null, so the history's newest version is used.
  const expectedVersion = Math.max(head?.version ?? 0, ...(history ?? []).map((item) => item.version));
  // Hint only (MD6): commercial CONFIRMED, work READY, unassigned, not fully refunded. The server decides.
  const eligible =
    detail.commercial_state === "CONFIRMED" &&
    detail.fulfillment_state === "MANUAL_UNASSIGNED" &&
    detail.work_state === "READY" &&
    detail.refunded_minor + detail.refund_pending_minor < detail.total_minor;
  const latestNote = [...(history ?? [])].reverse().find((item) => item.status === "SHIPPED" && item.version === head?.version)?.note ?? "";

  function fillFrom(shipment: Shipment | null) {
    setCarrier(shipment?.carrier_code ?? "");
    setCarrierName(shipment?.carrier_name ?? "");
    setTracking(shipment?.tracking_number ?? "");
    setURL(shipment?.tracking_url ?? "");
    setNote(shipment ? latestNote : "");
  }
  function choose(next: "record" | "correct" | "void") {
    pending.current = null;
    setProblem("");
    setUncertain(false);
    setNotice("");
    if (next === "correct") fillFrom(head);
    setMode(next);
  }
  function invalid() {
    if (!carrier) return c.errors.invalid_carrier;
    const name = carrierName.trim();
    if ((carrier === "other" && name === "") || Array.from(name).length > 80) return c.shipCarrierNameRequired;
    if (!trackingNumber.test(tracking.trim())) return c.shipInvalidTracking;
    if (url.trim() !== "" && trackingURLHost(url.trim()) === null) return c.shipInvalidUrl;
    if (Array.from(note.trim()).length > 200) return c.shipNoteTooLong;
    return "";
  }
  async function send() {
    if (busy) return;
    let body: string;
    if (mode === "void") {
      // manual-fulfilment-v1 §5.1 (A1): a void body nulls every carrier/tracking/note field.
      body = JSON.stringify({
        expected_version: expectedVersion, status: "VOIDED", carrier_code: null, carrier_name: null,
        tracking_number: null, tracking_url: null, note: null, void_reason: voidReason,
      });
    } else {
      const bad = invalid();
      if (bad) {
        setProblem(bad);
        return;
      }
      body = JSON.stringify({
        expected_version: expectedVersion, status: "SHIPPED", carrier_code: carrier,
        carrier_name: carrierName.trim() || null, tracking_number: tracking.trim(),
        tracking_url: url.trim() || null, note: note.trim() || null, void_reason: null,
      });
    }
    if (pending.current?.body !== body) pending.current = { key: `ship-${crypto.randomUUID()}`, body };
    setBusy(true);
    setProblem("");
    const result = await putShipment(store, orderID, pending.current.key, body, boundary);
    setBusy(false);
    if (result.ok) {
      pending.current = null;
      setUncertain(false);
      setNotice(c.shipDone);
      setMode("record");
      setTracking("");
      setURL("");
      setNote("");
      setTick((value) => value + 1);
      void onChanged();
      return;
    }
    if (result.uncertain) {
      setUncertain(true);
      setProblem(c.shipUncertain);
      return;
    }
    pending.current = null;
    setProblem(errorText(c, result.code));
    if (result.code === "version_changed") {
      setTick((value) => value + 1);
      void onChanged();
    }
  }

  const form = (
    <form
      className="orders-form"
      noValidate
      data-testid="shipment-form"
      onSubmit={(event) => {
        event.preventDefault();
        void send();
      }}
    >
      {mode === "void" ? (
        <label>
          {c.shipVoidReason}
          <select data-testid="void-reason" value={voidReason} disabled={uncertain}
            onChange={(event) => setVoidReason(event.target.value as VoidReason)}>
            {voidReasons.map((value) => <option key={value} value={value}>{c.voidReasons[value]}</option>)}
          </select>
        </label>
      ) : (
        <>
          <label>
            {c.shipCarrier}
            <select data-testid="ship-carrier" value={carrier} disabled={uncertain}
              onChange={(event) => setCarrier(event.target.value as CarrierCode | "")}>
              <option value="">—</option>
              {carrierCodes.map((value) => <option key={value} value={value}>{c.carriers[value]}</option>)}
            </select>
          </label>
          <label>
            {c.shipCarrierName}{carrier === "other" ? " *" : ""}
            <input data-testid="ship-carrier-name" value={carrierName} maxLength={80} autoComplete="off"
              required={carrier === "other"} disabled={uncertain} onChange={(event) => setCarrierName(event.target.value)} />
          </label>
          <label>
            {c.shipTracking}
            <input data-testid="ship-tracking" value={tracking} maxLength={64} autoComplete="off" required
              disabled={uncertain} onChange={(event) => setTracking(event.target.value)} />
          </label>
          <label>
            {c.shipUrl}
            <input data-testid="ship-url" type="url" inputMode="url" value={url} maxLength={512} autoComplete="off"
              disabled={uncertain} onChange={(event) => setURL(event.target.value)} />
          </label>
          <label>
            {c.shipNote}
            <input data-testid="ship-note" value={note} maxLength={200} autoComplete="off" disabled={uncertain}
              onChange={(event) => setNote(event.target.value)} />
          </label>
        </>
      )}
      {problem && <p className="orders-bad" role="alert" data-testid="shipment-problem">{problem}</p>}
      <div className="orders-form-actions">
        {mode !== "record" && !uncertain && <button type="button" onClick={() => choose("record")}>{c.cancel}</button>}
        <button type="submit" className="primary" data-testid="shipment-submit" disabled={busy}>
          {busy ? c.shipSending : uncertain ? c.shipRetry : mode === "void" ? c.shipVoidConfirm : mode === "correct" ? c.shipCorrectSave : c.shipMark}
        </button>
      </div>
    </form>
  );

  return (
    <section className="orders-section" data-testid="order-shipment" aria-label={c.shipTitle}>
      <h2>{c.shipTitle}</h2>
      {status === "error" && (
        <div role="status">
          <p>{c.sectionUnavailable}</p>
          <button type="button" onClick={() => setTick((value) => value + 1)}>{c.retry}</button>
        </div>
      )}
      {head ? (
        <dl className="orders-facts" data-testid="shipment-record">
          <div><dt>{c.shipCarrier}</dt><dd>{carrierLabel(c, head)}</dd></div>
          <div><dt>{c.shipTracking}</dt><dd className="orders-mono">{head.tracking_number}</dd></div>
          {head.tracking_url && (
            <div>
              <dt>{c.shipLink}</dt>
              <dd>
                <a href={head.tracking_url} target="_blank" rel="noopener noreferrer nofollow">
                  {trackingURLHost(head.tracking_url)}
                </a>
              </dd>
            </div>
          )}
          <div><dt>{c.shipRecorded}</dt><dd>{displayTime(locale, head.recorded_at)}</dd></div>
        </dl>
      ) : (
        <p className="orders-empty">{c.shipNone}</p>
      )}
      <p className="orders-hint">{c.shipHint}</p>
      {canWrite && head && mode === "record" && (
        <div className="orders-form-actions">
          <button type="button" className="orders-compact" data-testid="shipment-correct" disabled={status !== "ready"} onClick={() => choose("correct")}>
            {c.shipCorrect}
          </button>
          <button type="button" className="orders-compact" data-testid="shipment-void" disabled={status !== "ready"} onClick={() => choose("void")}>
            {c.shipVoid}
          </button>
        </div>
      )}
      {canWrite && !head && !eligible && <p className="orders-hint" data-testid="shipment-ineligible">{c.shipNotEligible}</p>}
      {canWrite && status === "ready" && ((!head && eligible) || (head && mode !== "record")) && form}
      {notice && <p className="orders-notice" role="status" data-testid="shipment-notice">{notice}</p>}
      {history && (
        <details className="orders-history" data-testid="shipment-history">
          <summary>{c.shipHistory} ({history.length})</summary>
          {history.length === 0 ? (
            <p>{c.shipHistoryEmpty}</p>
          ) : (
            <ol>
              {[...history].reverse().map((item) => (
                <li key={item.version}>
                  <strong>{c.shipVersion} {item.version} · {c.shipStatus[item.status]}</strong>
                  <span>
                    {carrierLabel(c, item)} · <span className="orders-mono">{item.tracking_number}</span> · {displayTime(locale, item.recorded_at)}
                  </span>
                  {item.void_reason && <span>{c.shipVoidReason}: {c.voidReasons[item.void_reason]}</span>}
                  {item.note && <span>{c.shipNoteLabel}: {item.note}</span>}
                  <span>{c.shipBy}: <span className="orders-mono">{item.principal_id}</span></span>
                </li>
              ))}
            </ol>
          )}
        </details>
      )}
    </section>
  );
}
