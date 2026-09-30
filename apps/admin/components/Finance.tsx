"use client";

// Merchant finance summary (/{locale}/finance): date range (native date inputs), daily table + totals row, CSV link,
// test-mode badge. BFF GET /api/stores/{store}/finance/summary?from&to (orders:read) and the plain download link
// GET .../finance/summary.csv (orders:read + orders:export, streamed by the BFF, never held in JS) -> Go
// /v1/admin/stores/{id}/finance/summary[.csv] (internal/httpapi/finance.go; internal/reporting.Finance/FinanceCSV).
// The CSV link shows only when GET order-actions says orders_export (same probe MerchantOrders uses).
// Money uses the shared `money` formatter; sums come from Go (I05), nothing is recomputed here.
import { useEffect, useState, type FormEvent } from "react";
import { useRouter } from "next/navigation";
import type { Locale } from "@live-commerce/i18n";
import type { Store } from "@/lib/model";
import { money } from "@/lib/client";
import { financeCSVHref, readFinance, useGuardedRead, type ReadCode } from "@/lib/customers-client";
import { isSandbox, type FinanceRow } from "@/lib/customers-model";
import { readOrderActions } from "@/lib/orders-client";
import { customersCopy } from "@/lib/customers-copy";
import { WorkspaceFrame } from "./WorkspaceFrame";
import "./orders.css";
import "./order-actions.css";
import "./customers.css";

const DAY_MS = 86_400_000;
// Same rule as the BFF grammar and Go D13: start not after end, at most 91 days apart, real calendar days.
function rangeOK(from: string, to: string) {
  const a = Date.parse(`${from}T00:00:00Z`);
  const b = Date.parse(`${to}T00:00:00Z`);
  return Number.isFinite(a) && Number.isFinite(b) && b >= a && (b - a) / DAY_MS <= 91;
}

export function Finance({
  locale,
  stores,
  store,
  from,
  to,
  today,
  initialError,
  renderKey,
}: {
  locale: Locale;
  stores: Store[];
  store: Store | null;
  from: string;
  to: string;
  today: string;
  initialError: ReadCode | null;
  renderKey: string;
}) {
  const c = customersCopy[locale];
  const router = useRouter();
  const [draftFrom, setDraftFrom] = useState(from);
  const [draftTo, setDraftTo] = useState(to);
  const [canExport, setCanExport] = useState(false);
  const valid = rangeOK(draftFrom, draftTo);
  const validQuery = rangeOK(from, to);
  const read = useGuardedRead(
    `${renderKey}|${locale}|${store?.id ?? ""}|${from}|${to}`,
    store && validQuery ? (signal) => readFinance(store.id, from, to, signal) : null,
    initialError,
  );

  // Permission probe for the CSV link only; a failed probe hides it and Go re-authorizes the download anyway.
  useEffect(() => {
    if (!store || initialError) return setCanExport(false);
    const active = new AbortController();
    readOrderActions(store.id, active.signal).then((value) => setCanExport(value.orders_export), () => setCanExport(false));
    return () => active.abort();
  }, [store, initialError]);

  const navigate = (nextStore: string, nextFrom: string, nextTo: string) => {
    const params = new URLSearchParams();
    if (nextStore) params.set("store", nextStore);
    params.set("from", nextFrom);
    params.set("to", nextTo);
    router.push(`/${locale}/finance?${params}`);
  };
  function submit(event: FormEvent) {
    event.preventDefault();
    if (valid) navigate(store?.id ?? "", draftFrom, draftTo);
  }
  const failure =
    read.status === "signed-out" ? c.financeSignedOut
    : read.status === "forbidden" ? c.financeForbidden
    : read.status === "not-found" ? (store ? c.notFound : c.noStore)
    : read.status === "unavailable" ? c.financeUnavailable
    : "";
  const summary = read.data;
  const sandbox = !!summary && [...summary.rows, ...summary.totals].some(isSandbox);
  return (
    <WorkspaceFrame locale={locale} storeName={store?.name ?? c.noStore} active="finance">
      <div className="orders-page customers-page" data-testid="finance-page">
        <header className="orders-heading">
          <h1>{c.financeTitle}</h1>
          <p>{c.financeSubtitle}</p>
        </header>
        <form className="orders-controls customers-finance-controls" onSubmit={submit}>
          {stores.length > 1 && (
            <label>
              {c.store}
              <select data-testid="store-selector" value={store?.id ?? ""}
                onChange={(event) => navigate(event.target.value, from, to)}>
                {stores.map((item) => (
                  <option key={item.id} value={item.id}>{item.name}</option>
                ))}
              </select>
            </label>
          )}
          <label>
            {c.from}
            <input type="date" data-testid="finance-from" value={draftFrom} max={today} required
              onChange={(event) => setDraftFrom(event.target.value)} />
          </label>
          <label>
            {c.to}
            <input type="date" data-testid="finance-to" value={draftTo} max={today} required
              onChange={(event) => setDraftTo(event.target.value)} />
          </label>
          <button type="submit" data-testid="finance-show" disabled={!store || !valid || read.status === "loading"}>
            {c.show}
          </button>
          {store && canExport && validQuery && (
            <a className="orders-export" data-testid="finance-csv" href={financeCSVHref(store.id, from, to)} download>
              {c.csv}
            </a>
          )}
          {!valid && <p className="orders-bad" role="alert" data-testid="finance-range-invalid">{c.rangeInvalid}</p>}
          <p className="orders-export-hint">{c.csvHint}</p>
        </form>
        {!validQuery && <p className="orders-message" role="status">{c.rangeInvalid}</p>}
        {(read.status === "loading" || read.status === "hidden") && validQuery && (
          <p className="orders-message" role="status">{c.financeLoading}</p>
        )}
        {failure && validQuery && (
          <div className="orders-message" role="status">
            <p>{failure}</p>
            <button type="button" onClick={read.reload}>{c.retry}</button>
          </div>
        )}
        {read.status === "ready" && summary && (
          <>
            {sandbox && <p className="orders-badge orders-tone-warning" data-testid="finance-test-badge">{c.testBadge}</p>}
            <p className="orders-hint">{c.timezone(summary.timezone)}</p>
            {summary.rows.length === 0 ? (
              <p className="orders-message" role="status">{c.financeEmpty}</p>
            ) : (
              <div className="orders-actions-scroll">
                <table className="orders-actions-table customers-finance-table" data-testid="finance-table">
                  <thead>
                    <tr>
                      <th>{c.day}</th><th>{c.currency}</th><th>{c.environment}</th>
                      <th>{c.paidOrders}</th><th>{c.captured}</th><th>{c.refunded}</th><th>{c.net}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {summary.rows.map((row) => (
                      <FinanceLine key={`${row.day}|${row.currency}|${row.environment}`} row={row} locale={locale} c={c} />
                    ))}
                  </tbody>
                  <tfoot>
                    {summary.totals.map((row) => (
                      <FinanceLine key={`total|${row.currency}|${row.environment}`} row={row} locale={locale} c={c} />
                    ))}
                  </tfoot>
                </table>
              </div>
            )}
          </>
        )}
      </div>
    </WorkspaceFrame>
  );
}

function FinanceLine({ row, locale, c }: { row: FinanceRow; locale: Locale; c: (typeof customersCopy)["en"] }) {
  const m = (value: number) => money(locale, row.currency, value);
  return (
    <tr data-testid={row.day ? `finance-row-${row.day}-${row.currency}` : `finance-total-${row.currency}`}>
      <td data-label={c.day}>{row.day || c.totalRow}</td>
      <td data-label={c.currency}>{row.currency}</td>
      <td data-label={c.environment}>
        <span className={`orders-badge ${isSandbox(row) ? "orders-tone-warning" : "orders-tone-success"}`}>{row.environment}</span>
      </td>
      <td data-label={c.paidOrders}>{row.captured_count}</td>
      <td data-label={c.captured}>{m(row.captured_minor)}</td>
      <td data-label={c.refunded}>{m(row.refunded_minor)}</td>
      <td data-label={c.net}>{m(row.net_minor)}</td>
    </tr>
  );
}
