// Owns the /{locale}/privacy route (customers-billing-v1 §5, U8): server-side locale validation and the document shell
// for PrivacyCenter. BFF calls happen in the browser component (GET /api/buyer/privacy, PUT /api/buyer/consents,
// POST /api/buyer/privacy/export | privacy/erasure -> Go /v1/buyer/... internal/buyerhttp/privacy.go).
// Non-goals: the server renders no personal data and reads no cookie (the buyer capability cookie stays HttpOnly and is
// used only by the BFF); `?notice=<version>` from the checkout link only highlights the notice section, nothing else.
// Depends on: components/PrivacyCenter.tsx, lib/privacy-contract.ts (notice version).
import { notFound } from "next/navigation";
import { isLocale } from "@live-commerce/i18n";
import PrivacyCenter from "../../../components/PrivacyCenter";
import { LC_PRIVACY_POLICY_VERSION } from "../../../lib/privacy-contract";

export default async function Page({
  params,
  searchParams,
}: {
  params: Promise<{ locale: string }>;
  searchParams: Promise<Record<string, string | string[] | undefined>>;
}) {
  const { locale } = await params;
  if (!isLocale(locale)) notFound();
  const { notice } = await searchParams;
  return <PrivacyCenter locale={locale} notice={notice === LC_PRIVACY_POLICY_VERSION} />;
}
