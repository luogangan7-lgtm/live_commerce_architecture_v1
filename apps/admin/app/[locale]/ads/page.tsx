// Admin ads page (server): resolves session + store like orders/page.tsx, then hands the client component the
// verified query. It calls no ads endpoint itself; the client reads through BFF GET /api/stores/{store}/ads/*
// -> Go GET /v1/admin/stores/{store_id}/ads/* (see components/Ads.tsx for the full route list).
// `connect` (state id) and `connect_error` (fixed code) are set only by the callback BFF redirect
// (app/api/ads/meta/callback/route.ts); anything else in the query is a 404.
import { headers } from "next/headers";
import { notFound } from "next/navigation";
import { isLocale } from "@live-commerce/i18n";
import {
  authConfig,
  authenticatedStores,
  exactCookieHeader,
  isBase64URL32,
  safeError,
  SESSION_COOKIE,
} from "@/lib/auth";
import type { Store } from "@/lib/model";
import { canonicalUUID, connectErrors, type ConnectError } from "@/lib/ads-model";
import { Ads, type AdsInitialError } from "@/components/Ads";

export default async function AdsPage({
  params,
  searchParams,
}: {
  params: Promise<{ locale: string }>;
  searchParams: Promise<Record<string, string | string[] | undefined>>;
}) {
  const { locale } = await params;
  if (!isLocale(locale)) notFound();
  const query = await searchParams;
  if (Object.keys(query).some((key) => !["store", "connect", "connect_error", "draft"].includes(key))) notFound();
  const single = (key: string) => {
    const value = query[key];
    if (value !== undefined && (typeof value !== "string" || value === "")) notFound();
    return value ?? "";
  };
  const requested = single("store");
  const connect = single("connect");
  const connectError = single("connect_error");
  const draft = single("draft");
  if ((requested && !canonicalUUID.test(requested)) ||
    (connect && !canonicalUUID.test(connect)) ||
    (draft && !canonicalUUID.test(draft)) ||
    (connectError && !connectErrors.includes(connectError as ConnectError)) ||
    (connect && connectError)) notFound();

  let stores: Store[] = [];
  let store: Store | null = null;
  let error: AdsInitialError | null = null;
  if (!authConfig) error = "signed-out";
  else {
    const token = exactCookieHeader((await headers()).get("cookie"), SESSION_COOKIE);
    if (!token || !isBase64URL32(token)) error = "signed-out";
    else {
      const listed = await authenticatedStores(token);
      if (!listed.stores) {
        const safe = await safeError(listed.response);
        error = safe.status === 401 ? "signed-out" : safe.status === 403 ? "forbidden" : "unavailable";
      } else {
        stores = listed.stores;
        store = requested
          ? (stores.find((item) => item.id === requested) ?? null)
          : ([...stores].sort((a, b) => a.id.localeCompare(b.id))[0] ?? null);
        if (requested && !store) notFound();
      }
    }
  }
  return <Ads locale={locale} stores={stores} store={store} connect={connect} connectError={connectError as ConnectError | ""}
    draft={draft} initialError={error} />;
}
