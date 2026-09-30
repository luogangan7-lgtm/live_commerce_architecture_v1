// Route /{locale}/customers/{customer}. BFF (browser, via CustomerDetail.tsx): GET /api/stores/{store}/customers/{id},
// POST .../customers/{id}/consent-withdrawals | exports | erasure
// -> Go /v1/admin/stores/{id}/customers/{id}[/...] (internal/httpapi/customers.go; customers:read / customers:privacy).
// This server file only resolves locale, the customer id, query and authorized stores (page-data.ts).
import { notFound } from "next/navigation";
import { canonicalUUID } from "@/lib/orders-model";
import { CustomerDetail } from "@/components/CustomerDetail";
import { loadPage } from "../page-data";

export default async function CustomerPage({
  params,
  searchParams,
}: {
  params: Promise<{ locale: string; customer: string }>;
  searchParams: Promise<Record<string, string | string[] | undefined>>;
}) {
  const { customer } = await params;
  if (!canonicalUUID.test(customer)) notFound();
  const page = await loadPage(params, searchParams, ["store"]);
  return (
    <CustomerDetail locale={page.locale} stores={page.stores} store={page.store} customerID={customer}
      initialError={page.initialError} renderKey={page.renderKey} />
  );
}
