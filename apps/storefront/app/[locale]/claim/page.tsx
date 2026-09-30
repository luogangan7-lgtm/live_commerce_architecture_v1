// Owns the /{locale}/claim route (live-keyword-claims-v1 §6, §11.1): server-side locale
// validation and a no-referrer, non-indexed document shell for ClaimLink.
// Non-goals: the server never sees the link token (it lives in the URL fragment) and
// renders no claim data; a crawler or preview bot GET carries no token and writes nothing.
// Depends on: components/ClaimLink.tsx, lib/demo-label.ts; the Referrer-Policy response
// header for this path is set in next.config.ts, the meta tag below repeats it.

import type { Metadata } from "next";
import { notFound } from "next/navigation";
import { isLocale } from "@live-commerce/i18n";
import ClaimLink from "../../../components/ClaimLink";
import { buyerDemoLabel } from "../../../lib/demo-label";

export const metadata: Metadata = {
  referrer: "no-referrer",
  robots: { index: false, follow: false },
};

export default async function Page({ params }: { params: Promise<{ locale: string }> }) {
  const { locale } = await params;
  if (!isLocale(locale)) notFound();
  return <ClaimLink locale={locale} demonstration={buyerDemoLabel(process.env)} />;
}
