// Owns the /{locale}/legal/{slug} route (slug in lib/legal-copy.ts legalSlugs): a static, server-rendered
// policy page. Calls no BFF / no Go endpoint (static); reads only lib/legal-copy.ts.
// Non-goals: no consent capture, no deletion execution (data-deletion and privacy self-service belong
// to customers-billing-ui), no per-store text. Unknown locale or slug -> notFound().
// Owner text: every non-final LegalText renders as a visibly marked block (data-owner-text) so
// LC_LEGAL_REQUIRE_FINAL (LG01) can fail while any marker remains. Indexing: inherits the layout's noindex.

import type { Metadata } from "next";
import { notFound } from "next/navigation";
import { isLocale } from "@live-commerce/i18n";
import { legalChrome, legalPage, legalSlugs, markerLabel } from "../../../../lib/legal-copy";
import type { LegalSlug, LegalText } from "../../../../lib/legal-copy";
import styles from "../legal.module.css";

type Params = Promise<{ locale: string; slug: string }>;
const isSlug = (v: string): v is LegalSlug => (legalSlugs as readonly string[]).includes(v);

export async function generateMetadata({ params }: { params: Params }): Promise<Metadata> {
  const { locale, slug } = await params;
  if (!isLocale(locale) || !isSlug(slug)) return {};
  return { title: legalPage(locale, slug).title };
}

function Text({ t }: { t: LegalText }) {
  if (t.kind === "final") return <p>{t.text}</p>;
  return (
    <p className={t.kind === "pending" ? styles.pending : styles.draft} data-owner-text={t.kind}>
      <strong>{markerLabel[t.kind]}</strong>
      {t.kind === "pending" ? " " : <br />}
      {t.text}
    </p>
  );
}

export default async function Page({ params }: { params: Params }) {
  const { locale, slug } = await params;
  if (!isLocale(locale) || !isSlug(slug)) notFound();
  const page = legalPage(locale, slug);
  const Wrap = slug === "contact" ? "address" : "div";
  return (
    <main lang={locale} className={styles.page}>
      <h1>{page.title}</h1>
      <div className={styles.updated}>
        {legalChrome(locale).updated}: {page.updated.kind === "final" ? page.updated.text : null}
        {page.updated.kind === "final" ? null : <Text t={page.updated} />}
      </div>
      {page.sections.map((s) => (
        <section key={s.heading}>
          <h2>{s.heading}</h2>
          <Wrap>
            {s.body.map((t, i) => (
              <Text key={i} t={t} />
            ))}
          </Wrap>
        </section>
      ))}
    </main>
  );
}
