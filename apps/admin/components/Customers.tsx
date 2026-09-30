"use client";

// Merchant customers list (/{locale}/customers, U1 audit-first: one table, no card grid, orders table classes).
// BFF GET /api/stores/{store}/customers?limit&after&q -> Go GET /v1/admin/stores/{id}/customers
// (internal/httpapi/customers.go, customers:read; internal/customers.List). Rows link to CustomerDetail.
// Read lifecycle (session fence, PII cleared when hidden/signed out) is useGuardedRead; nothing is cached client-side.
import { useRef, useState, type FormEvent } from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import type { Locale } from "@live-commerce/i18n";
import type { Store } from "@/lib/model";
import { money } from "@/lib/client";
import { readCustomers, useGuardedRead, type ReadCode } from "@/lib/customers-client";
import type { Customer } from "@/lib/customers-model";
import { displayTime } from "@/lib/orders-model";
import { customersCopy, type CustomersCopy } from "@/lib/customers-copy";
import { WorkspaceFrame } from "./WorkspaceFrame";
import { Icon } from "./Icon";
import "./orders.css";
import "./order-actions.css";
import "./customers.css";

function url(locale: Locale, store: string, q: string, after: string) {
  const params = new URLSearchParams();
  if (store) params.set("store", store);
  if (q) params.set("q", q);
  if (after) params.set("after", after);
  return `/${locale}/customers${params.size ? `?${params}` : ""}`;
}
export const detailHref = (locale: Locale, store: string, id: string) =>
  `/${locale}/customers/${id}${store ? `?store=${store}` : ""}`;

export function Customers({
  locale,
  stores,
  store,
  q,
  after,
  initialError,
  renderKey,
}: {
  locale: Locale;
  stores: Store[];
  store: Store | null;
  q: string;
  after: string;
  initialError: ReadCode | null;
  renderKey: string;
}) {
  const c = customersCopy[locale];
  const router = useRouter();
  const [draft, setDraft] = useState(q);
  const previous = useRef<string[]>([]);
  const read = useGuardedRead(
    `${renderKey}|${locale}|${store?.id ?? ""}|${q}|${after}`,
    store ? (signal) => readCustomers(store.id, q, after, signal) : null,
    initialError,
  );
  const go = (nextStore: string, nextQ: string, nextAfter: string) => router.push(url(locale, nextStore, nextQ, nextAfter));
  function search(event: FormEvent) {
    event.preventDefault();
    previous.current = [];
    go(store?.id ?? "", draft.trim(), "");
  }
  const failure =
    read.status === "signed-out" ? c.signedOut
    : read.status === "forbidden" ? c.forbidden
    : read.status === "not-found" ? (store ? c.notFound : c.noStore)
    : read.status === "unavailable" ? c.unavailable
    : "";
  const page = read.data;
  return (
    <WorkspaceFrame locale={locale} storeName={store?.name ?? c.noStore} active="customers">
      <div className="orders-page customers-page" data-testid="customers-page">
        <header className="orders-heading">
          <h1>{c.title}</h1>
          <p>{c.subtitle}</p>
        </header>
        <form className="orders-controls" role="search" onSubmit={search}>
          {stores.length > 1 && (
            <label>
              {c.store}
              <select
                data-testid="store-selector"
                value={store?.id ?? ""}
                onChange={(event) => {
                  previous.current = [];
                  go(event.target.value, "", "");
                }}
              >
                {stores.map((item) => (
                  <option key={item.id} value={item.id}>{item.name}</option>
                ))}
              </select>
            </label>
          )}
          <label className="customers-search">
            {c.search}
            <input
              type="search"
              data-testid="customers-search"
              value={draft}
              maxLength={40}
              autoComplete="off"
              aria-describedby="customers-search-hint"
              onChange={(event) => setDraft(event.target.value)}
            />
          </label>
          <button type="submit" data-testid="customers-search-submit" disabled={!store || read.status === "loading"}>
            <Icon name="search" size={18} />
            {c.searchButton}
          </button>
          {q && (
            <button
              type="button"
              onClick={() => {
                setDraft("");
                previous.current = [];
                go(store?.id ?? "", "", "");
              }}
            >
              {c.clear}
            </button>
          )}
          <button type="button" data-testid="customers-refresh" disabled={read.status === "loading"} onClick={read.reload}>
            <Icon name="refresh" size={18} />
            {c.refresh}
          </button>
          <p id="customers-search-hint" className="orders-export-hint">{c.searchHint}</p>
        </form>
        {(read.status === "loading" || read.status === "hidden") && (
          <p className="orders-message" role="status">{c.loading}</p>
        )}
        {failure && (
          <div className="orders-message" role="status">
            <p>{failure}</p>
            <button type="button" onClick={read.reload}>{c.retry}</button>
          </div>
        )}
        {read.status === "ready" && page && (
          <>
            <div className="orders-table-scroll">
              <table className="orders-table customers-table" data-testid="customers-table">
                <thead>
                  <tr>
                    <th>{c.customer}</th>
                    <th>{c.orders}</th>
                    <th>{c.spent}</th>
                    <th>{c.claims}</th>
                    <th>{c.consents}</th>
                    <th>{c.lastActivity}</th>
                  </tr>
                </thead>
                <tbody>
                  {page.items.map((row) => (
                    <CustomerRow key={row.customer_id} row={row} c={c} locale={locale} store={store?.id ?? ""} />
                  ))}
                </tbody>
              </table>
            </div>
            {page.items.length === 0 && (
              <p className="orders-message" role="status" aria-live="polite">{q ? c.emptySearch : c.empty}</p>
            )}
            <footer className="orders-pager">
              <span>{c.pageCount}: {page.items.length}</span>
              <div>
                <button
                  type="button"
                  data-testid="customers-previous"
                  disabled={!previous.current.length}
                  onClick={() => go(store?.id ?? "", q, previous.current.pop() ?? "")}
                >
                  {c.previous}
                </button>
                <button
                  type="button"
                  data-testid="customers-next"
                  disabled={!page.next_cursor}
                  onClick={() => {
                    previous.current.push(after);
                    go(store?.id ?? "", q, page.next_cursor);
                  }}
                >
                  {c.next}
                </button>
              </div>
            </footer>
          </>
        )}
      </div>
    </WorkspaceFrame>
  );
}

function CustomerRow({ row, c, locale, store }: { row: Customer; c: CustomersCopy; locale: Locale; store: string }) {
  const m = (value: number) => (row.currency ? money(locale, row.currency, value) : String(value));
  const name = row.display_name ?? c.noName;
  return (
    <tr data-testid={`customer-row-${row.customer_id}`}>
      <td data-label={c.customer}>
        <Link href={detailHref(locale, store, row.customer_id)} data-testid={`customer-open-${row.customer_id}`}
          aria-label={`${c.open}: ${name}`}>
          <strong>{name}</strong>
          {row.phone_last3 && <small>{c.phoneEnding} {row.phone_last3}</small>}
        </Link>
        {!row.active && <span className="orders-badge orders-tone-neutral">{c.erased}</span>}
      </td>
      <td data-label={c.orders}>{c.ordersPaid(row.orders_count, row.paid_orders_count)}</td>
      <td data-label={c.spent}>
        {row.currency ? `${m(row.captured_minor)} / ${m(row.refunded_minor)}` : "—"}
      </td>
      <td data-label={c.claims}>
        {row.claims_count}
        {row.platforms.length > 0 && <small>{row.platforms.map((p) => c.platforms[p] ?? p).join(" · ")}</small>}
      </td>
      <td data-label={c.consents}>
        <ConsentChip label={c.marketing} short="DM" on={row.consents.marketing_messages} c={c} />
        <ConsentChip label={c.ads} short="Ads" on={row.consents.ads_personalization} c={c} />
      </td>
      <td data-label={c.lastActivity}>{displayTime(locale, row.last_activity_at)}</td>
    </tr>
  );
}
// Text + tone, never colour alone; the full purpose name is the accessible label.
function ConsentChip({ label, short, on, c }: { label: string; short: string; on: boolean; c: CustomersCopy }) {
  return (
    <span
      className={`orders-badge ${on ? "orders-tone-success" : "orders-tone-neutral"}`}
      title={label}
      aria-label={`${label}: ${on ? c.consentGranted : c.consentNone}`}
    >
      {short}: {on ? c.consentGranted : c.consentNone}
    </span>
  );
}
