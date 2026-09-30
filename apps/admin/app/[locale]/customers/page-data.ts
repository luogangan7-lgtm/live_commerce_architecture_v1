// Shared server-side page loader for the customers, customer detail, finance and billing routes (this folder is the
// only place the four pages resolve locale, query and authorized stores, so their guards cannot drift apart).
// Server-only: reads the session cookie and lists the stores through Go `GET /v1/admin/stores` (lib/auth.ts
// authenticatedStores), exactly like app/[locale]/orders/page.tsx. It never calls the customers/billing endpoints:
// those are read in the browser through the BFF so the response is fenced by the session boundary (useGuardedRead).
import { headers } from "next/headers";
import { notFound } from "next/navigation";
import { isLocale, type Locale } from "@live-commerce/i18n";
import {
  authConfig,
  authenticatedStores,
  exactCookieHeader,
  isBase64URL32,
  safeError,
  SESSION_COOKIE,
} from "@/lib/auth";
import type { Store } from "@/lib/model";
import { canonicalUUID } from "@/lib/orders-model";
import type { ReadCode } from "@/lib/customers-client";

export type PageContext = {
  locale: Locale;
  stores: Store[];
  store: Store | null;
  initialError: ReadCode | null;
  query: Record<string, string>;
  renderKey: string;
};

// `allowed` is the page's whole query grammar: any other key, a repeated key or an empty value is a 404, never ignored.
export async function loadPage(
  params: Promise<{ locale: string }>,
  searchParams: Promise<Record<string, string | string[] | undefined>>,
  allowed: string[],
): Promise<PageContext> {
  const { locale } = await params;
  if (!isLocale(locale)) notFound();
  const raw = await searchParams;
  if (Object.keys(raw).some((key) => !allowed.includes(key))) notFound();
  const query: Record<string, string> = {};
  for (const [key, value] of Object.entries(raw)) {
    if (typeof value !== "string" || value === "") notFound();
    query[key] = value;
  }
  if (query.store && !canonicalUUID.test(query.store)) notFound();

  let stores: Store[] = [];
  let store: Store | null = null;
  let initialError: ReadCode | null = null;
  if (!authConfig) initialError = "signed-out";
  else {
    const token = exactCookieHeader((await headers()).get("cookie"), SESSION_COOKIE);
    if (!token || !isBase64URL32(token)) initialError = "signed-out";
    else {
      const listed = await authenticatedStores(token);
      if (!listed.stores) {
        const safe = await safeError(listed.response);
        initialError = safe.status === 401 ? "signed-out" : safe.status === 403 ? "forbidden" : "unavailable";
      } else {
        stores = listed.stores;
        store = query.store
          ? (stores.find((item) => item.id === query.store) ?? null)
          : ([...stores].sort((a, b) => a.id.localeCompare(b.id))[0] ?? null);
        if (query.store && !store) notFound();
      }
    }
  }
  return { locale, stores, store, initialError, query, renderKey: crypto.randomUUID() };
}
