// Route /{locale}/customers. BFF (browser, via Customers.tsx): GET /api/stores/{store}/customers?limit&after&q
// -> Go GET /v1/admin/stores/{id}/customers (internal/httpapi/customers.go, customers:read).
// This server file only resolves locale, query and authorized stores (page-data.ts); it fetches no customer data.
import { notFound } from "next/navigation";
import { canonicalCursor } from "@/lib/orders-model";
import { Customers } from "@/components/Customers";
import { loadPage } from "./page-data";

export default async function CustomersPage({
  params,
  searchParams,
}: {
  params: Promise<{ locale: string }>;
  searchParams: Promise<Record<string, string | string[] | undefined>>;
}) {
  const page = await loadPage(params, searchParams, ["store", "q", "after"]);
  const { q = "", after = "" } = page.query;
  // Same bounds as the BFF grammar (lib/customers-request.ts): q at most 40 characters, opaque cursor.
  if (Array.from(q).length > 40 || /[\p{C}\p{Zl}\p{Zp}]/u.test(q) || (after && !canonicalCursor.test(after))) notFound();
  return (
    <Customers locale={page.locale} stores={page.stores} store={page.store} q={q} after={after}
      initialError={page.initialError} renderKey={page.renderKey} />
  );
}
