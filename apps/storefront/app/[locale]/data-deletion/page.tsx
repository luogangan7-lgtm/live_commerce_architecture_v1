// Owns the public /{locale}/data-deletion route (customers-billing-v1 §5 Storefront line, Q9, Meta App Review
// "data deletion instructions" URL, F-P2): static instructions, no API call, no cookie, no personal data.
// Non-goals: not a Meta data-deletion callback (Q9: instructions page only in v1) and not a request form; store data is
// erased from /{locale}/privacy or by asking the store. Indexable on purpose (App Review must fetch it), unlike the
// rest of the storefront (layout.tsx sets noindex).
// Depends on: lib/privacy-copy.ts.
import type { Metadata } from "next";
import { notFound } from "next/navigation";
import { isLocale, type Locale } from "@live-commerce/i18n";
import { privacyCopy } from "../../../lib/privacy-copy";

export const metadata: Metadata = { robots: { index: true, follow: true } };

const names: Record<Locale, string> = { "zh-CN": "简体中文", "zh-TW": "繁體中文", en: "English" };

export default async function Page({ params }: { params: Promise<{ locale: string }> }) {
  const { locale } = await params;
  if (!isLocale(locale)) notFound();
  const copy = privacyCopy[locale];
  return (
    <>
      <header className="shop-header">
        <span>{copy.deletionTitle}</span>
        <nav aria-label={copy.language}>
          {(Object.keys(names) as Locale[]).map((item) => (
            <a key={item} href={`/${item}/data-deletion`} lang={item} aria-current={item === locale ? "page" : undefined}>
              {names[item]}{" "}
            </a>
          ))}
        </nav>
      </header>
      <main className="purchase-main claim-main" data-testid="data-deletion">
        <h1>{copy.deletionTitle}</h1>
        <p className="claim-intro">{copy.deletionIntro}</p>
        <section className="claim-preview" aria-labelledby="deletion-steps-title">
          <h2 id="deletion-steps-title">{copy.deletionStepsTitle}</h2>
          <ol>
            {copy.deletionSteps.map((step) => (
              <li key={step}>{step}</li>
            ))}
          </ol>
        </section>
        <section className="claim-preview" aria-labelledby="deletion-store-title">
          <h2 id="deletion-store-title">{copy.deletionStoreTitle}</h2>
          <p>{copy.deletionStoreText}</p>
          <p className="claim-note">{copy.deletionKept}</p>
          <p>
            <a href={`/${locale}/privacy`} data-testid="data-deletion-privacy-link">{copy.deletionLink}</a>
          </p>
        </section>
      </main>
    </>
  );
}
