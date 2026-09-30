// New-tab page that prints an ECPay store label (taiwan-cvs-logistics-v1 §8, U6). Server side it only validates the
// locale and the three query fields; the client component then calls the BFF
// POST /api/stores/{store}/orders/{order}/cvs-shipment/print-form (-> Go internal/httpapi/cvs.go) and posts the
// returned signed form to ECPay top-level. Merchant side only: the buyer map never opens a new window.
import { notFound } from "next/navigation";
import { isLocale } from "@live-commerce/i18n";
import { canonicalUUID } from "@/lib/orders-model";
import { CvsPrint } from "./CvsPrint";

export default async function CvsPrintPage({
  params,
  searchParams,
}: {
  params: Promise<{ locale: string }>;
  searchParams: Promise<Record<string, string | string[] | undefined>>;
}) {
  const { locale } = await params;
  if (!isLocale(locale)) notFound();
  const query = await searchParams;
  if (Object.keys(query).some((key) => !["store", "order", "thermal"].includes(key))) notFound();
  const single = (key: string) => {
    const value = query[key];
    return typeof value === "string" ? value : "";
  };
  const store = single("store");
  const order = single("order");
  const thermal = single("thermal");
  // Invalid links render the client's "not valid" message instead of a bare 404 so the merchant knows what to do.
  const valid = canonicalUUID.test(store) && canonicalUUID.test(order) && (thermal === "0" || thermal === "1");
  return <CvsPrint locale={locale} store={valid ? store : ""} order={valid ? order : ""} thermal={thermal === "1"} />;
}
