# Unit legal-pages — storefront privacy policy, terms, refunds/returns/cancellation, shipping, contact + footer

Role: ui_worker (mid tier). Base `00c1d94`. Worktree `.worktrees/legal-pages`, branch `unit/legal-pages`.
No delegation, no new dependency, no lockfile change, no backend call. Parallel with every other unit
(disjoint paths under `apps/storefront`). Sources: `contracts/stripe-live-enable-v1.md` §1 L9, §9
`policy_pages`, §13, §14 LQ7 (default: owner supplies text; one static policy set linked from the
storefront footer and used as the Stripe business website); `contracts/customers-billing-v1.md` F-P2, Q9
(public data-deletion instructions page, no callback), CD7 (what erasure does/keeps);
`contracts/meta-ads-v1.md` App Review row (privacy policy, terms, data-deletion URLs);
`r2-design-rulings.md` O-C (Meta app under 香港大碗貿易有限公司), LQ1/LQ7 owner inputs.

**Goal:** public, static, 3-locale pages that satisfy Stripe's website requirements (L9) and Meta's
privacy/terms/data-deletion URLs, with every sentence that makes a legal or business commitment visibly
marked as owner text until the owner supplies or approves it. Engineering never invents policy terms.

## Read (by section)
PROCESS.md; the sections above only. Code by symbol: `apps/storefront/app/[locale]/layout.tsx` (locale
guard, metadata, design comment), `app/[locale]/claim/page.tsx` (header comment convention, `isLocale`,
`notFound`), `lib/history-copy.ts` (copy shape per locale), `app/globals.css` (tokens — read, do not edit),
`packages/i18n` (`locales`, `isLocale`), `apps/storefront/tests/purchase.test.mjs` (node:test style).

## Defaults adopted
- P1 Platform-level pages (one set, not per store): v1 sells only the owner's own store under
  香港大碗貿易有限公司 (LQ1 default, waiver W1). Per-store policy text is a later contract, needed before a
  second merchant onboards.
- P2 Routes: one dynamic route `/{locale}/legal/{slug}` with the closed slug set `privacy`, `terms`,
  `refunds` (refund, return, cancellation and dispute policy — L9 lists them together), `shipping`,
  `contact`; unknown slug → `notFound()`. `/{locale}/data-deletion` (Meta F-P2 instructions) and
  `/{locale}/privacy` (buyer self-service) are **customers-billing-ui's** (its frozen contract ships them,
  CB11; its brief already claims `app/[locale]/{privacy,data-deletion}/**`). This unit links to both by
  path: footer link "Data deletion", and a "Your data and deletion" section in the privacy policy.
- P3 Owner text markers: every owner-supplied value is `owner("<what is needed>")` in `legal-copy.ts`,
  rendered as a visible bordered block `<span data-owner-text="pending">待業主提供 · Owner text pending:
  <what></span>`; engineering drafts derived from contracts (data categories we store, Stripe as
  processor, CD7 erasure scope, 7–21 day dispute window L10) are `draft("…")` →
  `data-owner-text="draft"` with a "草稿 · Draft pending owner approval" label. Placeholder list, at
  least: legal entity name (default text 香港大碗貿易有限公司, still `pending` until LQ1 confirmed),
  registered address, support email, support phone, business description, refund window, return
  conditions, cancellation rule, shipping regions/carriers/lead times/fees, governing law, policy
  effective date. `LC_LEGAL_REQUIRE_FINAL=1` (LG01) fails while any marker renders.
- P4 Indexing: pages inherit the layout's `robots: noindex` (reviewers only need a reachable URL); switching
  to indexable is an owner choice, one metadata line.
- P5 Styling: a CSS module `app/[locale]/legal/legal.module.css` (native Next feature) using the existing
  `globals.css` custom properties; readable single column (max ~70ch), `h1` + `h2` sections, `<address>`
  for contact, "last updated" line. No new colour tokens, no cards/grids, no images. The footer is one row
  of text links, wraps at 390px.

## FROZEN interface
```ts
// apps/storefront/lib/legal-copy.ts
export const legalSlugs = ["privacy", "terms", "refunds", "shipping", "contact"] as const;
export type LegalSlug = (typeof legalSlugs)[number];
export type LegalText = { kind: "final" | "draft" | "pending"; text: string };
export function legalPage(locale: Locale, slug: LegalSlug):
  { title: string; updated: LegalText; sections: { heading: string; body: LegalText[] }[] };
export function legalFooterLinks(locale: Locale): { href: string; label: string }[]; // 5 legal slugs + /{locale}/data-deletion
export function pendingOwnerText(): { locale: Locale; page: string; what: string; kind: "draft" | "pending" }[];
// apps/storefront/components/LegalFooter.tsx
export default function LegalFooter({ locale }: { locale: Locale }): JSX.Element; // server component
```

## Build
1. `lib/legal-copy.ts` (P1–P3), zh-TW, zh-CN, en; same section structure in every locale.
2. `app/[locale]/legal/[slug]/page.tsx`: validate locale and slug, render `legalPage`, `generateMetadata` title per page, `<main lang>`; markers per P3.
3. `components/LegalFooter.tsx`: `<footer><nav aria-label=…>` with `legalFooterLinks`.
4. `app/[locale]/legal/legal.module.css` (P5), imported by the page and the footer.
5. `apps/storefront/tests/legal-copy.test.mjs`: every locale × page has title + ≥1 section; locales share
   section count; footer has 6 unique hrefs; `pendingOwnerText()` lists exactly the rendered markers and is
   printed to `output/legal-pages/owner-text-needed.txt` (the owner's to-do list).

Every new route/component file starts with the PROCESS §5 comment: owns which route, "calls no BFF / no Go
endpoint (static)", non-goals (no consent capture, no deletion execution, no per-store text).

## Write paths
`apps/storefront/app/[locale]/legal/**`, `apps/storefront/lib/legal-copy.ts`, `apps/storefront/components/LegalFooter.tsx`,
`apps/storefront/tests/legal-copy.test.mjs`, `output/legal-pages/**`.
Forbidden: `app/[locale]/layout.tsx`, `globals.css`, `next.config.ts`, `app/[locale]/{privacy,data-deletion}/**`
(customers-billing-ui), other storefront routes/components/lib, `tests/storefront/**` (LG01 is
stripe-live-tests'), Go, SQL, contracts, lockfiles, new packages.

## Gates
Implementer: typecheck, build, `legal-copy.test.mjs` (red: one locale missing a section → the
section-count assertion fails; log it, restore, then green).
Independent: **LG01** (stripe-live-tests). Owner: supplies/approves every marker; §9 `policy_pages`
attestation only after `LC_LEGAL_REQUIRE_FINAL=1` LG01 passes.

## Verify
```sh
pnpm typecheck:storefront && pnpm build:storefront
node --test apps/storefront/tests/legal-copy.test.mjs
```
Logs + 1586×992 and 390px screenshots × 3 locales (legal/privacy, legal/refunds, legal/contact) →
`/Volumes/data/live_commerce_architecture_v1/output/legal-pages/` for the independent visual review.

## Integrator hooks (only the integrator edits these)
- `apps/storefront/app/[locale]/layout.tsx`: `import LegalFooter from "../../components/LegalFooter";` and
  `<LegalFooter locale={locale as Locale} />` after `{children}` (one footer on every storefront page).
- Caddy: confirm the public storefront site serves `/{locale}/legal/*` and `/{locale}/data-deletion`
  (no change expected; no new host).
- Cross-unit: `/{locale}/data-deletion` stays customers-billing-ui's; until it merges the footer link 404s
  (LG01 accepts that only when the route file is absent). The Meta App Review URL set = legal/privacy,
  legal/terms, data-deletion; the integrator checks all three return 200 before the owner submits.
- Owner steps (not engineering): paste the final text, set the storefront URL as Stripe business website
  and as the Meta app's privacy/terms/data-deletion URLs.

## Non-goals / Return
No cookie banner (no non-essential cookies today), no consent capture, no deletion callback (Q9), no CMS,
no per-store text. Return commit SHA, model/reasoning, base, paths, commands + exits + counts, screenshot
paths, the owner-text list, P1–P5 as applied, risks, NOT_RUN (LG01 is not yours).
