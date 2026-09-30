# Studio › Claims and buyer claim page — composition recorded, owner review PENDING

Scope: `apps/admin/app/[locale]/studio/claims/page.tsx` (`StudioClaims.tsx`,
`claims.css`) and `apps/storefront/app/[locale]/claim/page.tsx` (`ClaimLink.tsx`,
claim rules at the end of `apps/storefront/app/globals.css`).
Contract: `contracts/live-keyword-claims-v1.md` §11.1 (T10b), unblocked by §0.1 on
2026-09-28 with the instruction to reuse the approved Studio/admin and storefront
worlds. No new visual world, token, raster or icon was introduced. This composition
is NOT owner-approved; it is recorded here for review with the KC16 screenshots.

## Merchant: Studio › Claims (Operate)

Entry: the Studio heading shows "Keyword claims" once a scene is selected; the
page keeps the workspace rail/topbar, a `Live Studio › Keyword claims` breadcrumb,
the scene title and one "Refresh facts". A blue note (Studio notice style) states
"MOCK capture — comments are not read automatically yet" on the page and inside the
link dialog. One joined white surface (Studio workbench language): a 340px left
rail for the claim window (Open/Closed badge with words, round, quantity rule
selectable only while CLOSED, one primary Open/Close action), this round's counters
(recorded + seven rejection reasons, with "N not understood — pin the host prompt"
when NO_MATCH > 0) and the frozen host prompt (language + keyword selectors, copy);
a broad right column with the offers table and add-offer form (keyword, product,
SKU, max per claim), then manual comment entry (buyer picker or new label with a
no-phone/address hint, comment text, result line in words). Below the surface, the
buyers table (ref, label + opened state, claimed lines with in-cart state, link
state, Create/Replace/Release-and-replace). Labels sit beside their controls.

One-time link: a native modal dialog masks the token by default
(`https://<origin>/<locale>/claim#t=••••••••`), with Reveal, Copy link, Copy message
(buyer language selectable) and "Close and discard"; hiding the page re-masks it and
closing or leaving drops it. A replay shows that the token was not received and asks
for Replace. At ≤980px the rail stacks above the work column; at ≤680px tables become
labelled two-column rows and forms one column, with no page horizontal overflow.

## Buyer: /{locale}/claim (product-detail world)

Compact language header (path-only switch, token stays in memory), navy title,
claimed lines (name, SKU code, keyword, quantity, current line amount, "Already in
your cart"/"Not available right now" chips), notes "current price, final at checkout"
and "claims do not reserve stock", link expiry, one full-width teal "Add to cart"
(explicit click, disabled when nothing is pending), then "Your cart" with remove
buttons. 404/409 use the contract copy in the existing error panel; 409 links to
`#claim-cart` and offers "Reload claim" (no automatic retry).

## Evidence and open review

KC16 (`scripts/dev/test-local.sh --browser-live-claims`) captures desktop 1586×992
and 390×844 merchant views and 390px buyer views in en, zh-CN and zh-TW under
`output/playwright/live-claims-*` (not committed). Owner visual review, an
independent finish review and a design-comp comparison are NOT_RUN.
