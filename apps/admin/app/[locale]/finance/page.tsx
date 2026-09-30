// Route /{locale}/finance. BFF (browser, via Finance.tsx): GET /api/stores/{store}/finance/summary?from&to and the plain
// download link finance/summary.csv, plus GET order-actions (orders_export) for the CSV link
// -> Go GET /v1/admin/stores/{id}/finance/summary[.csv] (internal/httpapi/finance.go; orders:read, + orders:export).
// This server file resolves the range: query from/to, else the last 30 finance days ending today (UTC+8, Q11).
import { notFound } from "next/navigation";
import { financeDay } from "@/lib/customers-model";
import { Finance } from "@/components/Finance";
import { loadPage } from "../customers/page-data";

const dayPattern = /^\d{4}-\d{2}-\d{2}$/;

export default async function FinancePage({
  params,
  searchParams,
}: {
  params: Promise<{ locale: string }>;
  searchParams: Promise<Record<string, string | string[] | undefined>>;
}) {
  const page = await loadPage(params, searchParams, ["store", "from", "to"]);
  const now = new Date();
  const { from = financeDay(now, -29), to = financeDay(now) } = page.query;
  if (!dayPattern.test(from) || !dayPattern.test(to)) notFound();
  return (
    <Finance locale={page.locale} stores={page.stores} store={page.store} from={from} to={to}
      today={financeDay(now)} initialError={page.initialError} renderKey={page.renderKey} />
  );
}
