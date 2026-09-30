// Route /{locale}/billing. BFF (browser, via Billing.tsx): GET /api/stores/{store}/billing, POST .../billing/checkout,
// POST .../billing/portal -> Go /v1/admin/stores/{id}/billing[/checkout|/portal] (internal/httpapi/billing.go,
// billing:manage). Stripe returns here as `?store=<id>&checkout=done|cancel` (LC_BILLING_RETURN_ORIGIN, billing-core B3).
import { notFound } from "next/navigation";
import { Billing } from "@/components/Billing";
import { loadPage } from "../customers/page-data";

export default async function BillingPage({
  params,
  searchParams,
}: {
  params: Promise<{ locale: string }>;
  searchParams: Promise<Record<string, string | string[] | undefined>>;
}) {
  const page = await loadPage(params, searchParams, ["store", "checkout"]);
  const checkout = page.query.checkout ?? "";
  if (checkout && checkout !== "done" && checkout !== "cancel") notFound();
  return (
    <Billing locale={page.locale} stores={page.stores} store={page.store}
      checkout={checkout as "" | "done" | "cancel"} initialError={page.initialError} renderKey={page.renderKey} />
  );
}
