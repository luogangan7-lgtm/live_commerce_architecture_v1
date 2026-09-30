// Owns the storefront footer row of legal links (5 policy pages + data deletion). Server component;
// calls no BFF / no Go endpoint (static); links only. The integrator mounts it once in
// app/[locale]/layout.tsx after {children}. /{locale}/data-deletion belongs to customers-billing-ui
// and 404s until that unit merges. Non-goals: no consent capture, no language switcher.

import type { Locale } from "@live-commerce/i18n";
import { legalChrome, legalFooterLinks } from "../lib/legal-copy";
import styles from "../app/[locale]/legal/legal.module.css";

export default function LegalFooter({ locale }: { locale: Locale }) {
  return (
    <footer className={styles.footer}>
      <nav aria-label={legalChrome(locale).nav}>
        <ul>
          {legalFooterLinks(locale).map((l) => (
            <li key={l.href}>
              <a href={l.href}>{l.label}</a>
            </li>
          ))}
        </ul>
      </nav>
    </footer>
  );
}
