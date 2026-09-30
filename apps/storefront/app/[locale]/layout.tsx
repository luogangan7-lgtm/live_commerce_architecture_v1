import type { Metadata } from "next";
import { notFound } from "next/navigation";
import { isLocale } from "@live-commerce/i18n";
import type { Locale } from "@live-commerce/i18n";
import LegalFooter from "../../components/LegalFooter";
import "../globals.css";

export const dynamic = "force-dynamic";
export const metadata: Metadata = {
  title: "Storefront",
  robots: { index: false, follow: false },
};

// An inert first-child template preserves the fixed design comment in React's
// rendered HTML. No user content is interpolated into this raw string.
const direction = `<!--
THESIS: Buy directly from a clear product detail, without an onboarding wizard.
OWN-WORLD: Existing navy, teal and fine gray rules; white working surface, system sans and semantic controls.
STORY: Read the product, select a SKU and quantity, then inspect actual delivery and quotation.
FIRST VIEWPORT: Compact language header, prominent title and price, vertical SKU rows, aligned quantity, one light sticky purchase footer. No invented shop identity or imagery.
FORM: Approved B inline product detail, ranked structure 3, seed baaadec1. Desktop preserves the mobile hierarchy.
FINISH: unreviewed and undocumented is unfinished; this build ends with the finish review, the verdict, DESIGN.md, and every shipping raster carrying its provenance
-->`;

export default async function Layout({
  children,
  params,
}: {
  children: React.ReactNode;
  params: Promise<{ locale: string }>;
}) {
  const { locale } = await params;
  if (!isLocale(locale)) notFound();
  return (
    <html lang={locale}>
      <body>
        <template
          data-commerce-design-contract="baaadec1"
          dangerouslySetInnerHTML={{ __html: direction }}
        />
        {children}
        <LegalFooter locale={locale as Locale} />
      </body>
    </html>
  );
}
