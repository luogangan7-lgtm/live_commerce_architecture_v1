# Unit ads-ui — admin Ads page: connect Meta, pick account, drafts, approve/publish/pause, report, CAPI setting

Role: ui_worker (mid tier). Base SHA `00c1d94`. Worktree `.worktrees/ads-ui`, branch `unit/ads-ui`.
No delegation, no new dependency, no lockfile change. **Wave 1**, parallel with ads-core against the
**Frozen HTTP** table and JSON shapes in `docs/delivery/units/ads-core.md` (do not read ads-core's branch).
Contract: `contracts/meta-ads-v1.md` (FROZEN) §2, §5.1, §5.3 (X7), §7, §12, rulings O4/O6/O8/Q6.
Buyer consent control is **not** this unit's (customers-billing UI owns it; its copy must follow O6).

**Goal:** a merchant can, on one admin page, connect a Meta ad account, create and approve a draft,
publish, pause/end, copy a paused draft into a new one, read the three-block report and toggle CAPI —
never seeing a token and never being told spend is a hard stop.

## Read (by section)
PROCESS.md; contract sections above; ads-core Defaults D1, D5, D6, D9 and Frozen HTTP. Code by symbol
(pattern, not to edit): `apps/admin/app/[locale]/orders/page.tsx` (server auth/session/store resolution),
`apps/admin/components/{MerchantOrders,WorkspaceFrame,Icon}.tsx`, `apps/admin/lib/{backend.ts (callBackend,
merchantBackend), auth.ts, claims-request.ts (exported route regex pattern `claimsRoutes`), orders-model.ts,
orders-client.ts, orders-copy.ts}`, `apps/admin/app/api/stores/[store]/[...resource]/route.ts` (grammar,
Idempotency-Key handling, method table), `apps/admin/app/api/auth/callback/route.ts` (redirect handling).

## Defaults adopted
- U1 **Connect BFF** `apps/admin/app/api/ads/meta/connect/route.ts` (POST, same-origin, session): calls Go
  `POST ads/meta/connect` via `callBackend`, sets cookie `lc_ads_connect` = store UUID (httpOnly, Secure,
  SameSite=Lax, Path=`/api/ads/meta/callback`, Max-Age=600), returns `{dialog_url}`; the page does
  `location.assign(dialog_url)` only if its origin is exactly `https://www.facebook.com`.
- U2 **Callback BFF** `apps/admin/app/api/ads/meta/callback/route.ts` (GET; the registered redirect URI):
  reads `code`,`state` (length/charset-bounded), store from `lc_ads_connect` (else `state_mismatch`), calls Go
  `GET ads/meta/callback`, clears the cookie, 303 → `/<locale>/ads?store=<store>&connect=<state_id>` or
  `&connect_error=<fixed code>`. Never logs the URL, never echoes `code`/`state`, `Cache-Control: no-store`,
  `Referrer-Policy: no-referrer`. Locale from the existing locale cookie, default `zh-TW`.
- U3 Pick step: `GET ads/meta/states/{id}` → radio list of ad accounts (name, id, currency, status) and
  optional dataset select; POST `ads/meta/bindings`; expired state → "start again".
- U4 Draft form: template radio (Boost post / Product traffic), source picker = typed id (post id /
  IG media id / product) validated by pattern only (server decides), budget in whole currency units (TWD
  whole NT$, converted to minor ×100 client-side with integer math only), start ≥ now+10 min, end ≤ start+30 d,
  countries (default TW), age 18–65. No placement picker (O8). Client validation is a hint; server codes rule.
- U5 Status badges from `Draft.status` (§5.1): DRAFT, APPROVED, SUBMITTING, REMOTE_PAUSED, ACTIVE, PAUSED,
  UNKNOWN ("check Ads Manager" + Ads Manager link + pause button), FAILED, ENDED, REJECTED. Pause button
  visible whenever any remote id exists and status ≠ ENDED; after pause: "Copy to new draft" (X7), never resume.
- U6 Allowance: when `max_active_budget_minor == 0` show "Ads are off for this store until the platform
  enables them" (O4) and disable approve; environment SANDBOX shows a persistent "Sandbox — no delivery" banner.
- U7 Report: three separately titled blocks (orders net of refunds / Meta delivery / Meta-reported
  purchases), each with window, timezone, `fetched_at`; `orders == null` → "not available"; copy states
  "Meta budgets are not a real-time hard stop; figures refresh with a delay" (§12). No combined ROAS number.
- U8 Copy zh-TW (default), zh-CN, en for every string and every Frozen HTTP error code.

## Build
`apps/admin/app/[locale]/ads/page.tsx` (server: auth + store, same pattern as orders), components
`Ads.tsx` (sections: Connection, Drafts list + detail, Draft form, Report, CAPI), `lib/ads-{model,client,
request,copy}.ts` — `ads-request.ts` exports `adsRoutes: {GET, POST, PUT}` regex fragments + validators for
exactly the Frozen HTTP resources except `meta/connect` and `meta/callback` (those use U1/U2); `ads-model.ts`
strict parsers of the frozen JSON (unknown enum → error state, never guessed). One Idempotency-Key per
dialog open, reused on retry; `If-Match` = last GET revision; re-GET after every mutation (no optimistic state).

## PROCESS §5 (binding)
Every new route/component file starts with a comment naming the BFF route(s) it calls and the Go endpoint
behind them (e.g. `// BFF POST /api/stores/{store}/ads/drafts → Go POST /v1/admin/stores/{store_id}/ads/drafts`).

## Write paths
`apps/admin/app/[locale]/ads/page.tsx`, `apps/admin/app/api/ads/meta/{connect,callback}/route.ts`,
`apps/admin/components/{Ads.tsx,ads.css}`, `apps/admin/lib/ads-{model,client,request,copy}.ts`,
`tests/admin/{ads-model,ads-request}.test.ts`, `output/ads-ui/**`.
Forbidden: `WorkspaceFrame.tsx`, `Icon.tsx`, `[...resource]/route.ts`, `globals.css`, `next.config.ts`,
other components/libs, Go, SQL, contracts, `*.spec.ts`, `tests/foundation/**`, lockfiles, storefront.

## Gates
Implementer: node unit tests for model/request (every frozen code + enum, money conversion vectors incl.
TWD non-whole rejected, callback param bounds). Independent: **MA09** browser (ads-tests) — do not claim it.

## Verify
```sh
pnpm typecheck:admin && pnpm build:admin
node --test --experimental-strip-types tests/admin/ads-model.test.ts tests/admin/ads-request.test.ts
```
Screenshots 1586×992 + 390px × 3 locales (fixture JSON through the BFF fixture session) →
`/Volumes/data/live_commerce_architecture_v1/output/ads-ui/` for independent visual review.

## Integrator hooks (integrator only)
`WorkspaceFrame.tsx`: nav item `["ads", "<existing icon>", c.ads]` + router branch to `/${locale}/ads`
(+ `c.ads` copy in the frame copy); `[...resource]/route.ts`: import `adsRoutes` and splice
`${adsRoutes.GET|POST|PUT}` into the method table (same as `claimsRoutes`); Meta app dashboard redirect URI
= `https://<admin host>/api/ads/meta/callback` (owner step §11); Caddy log exclusion for that path.

## NOT_RUN / Order / Return
Real-chain check after F2 + integrator mount; MA09 is ads-tests'. Author at dispatch. Return SHA,
model/reasoning, base, paths, commands + exits + counts, screenshots path, U1–U8 as applied, risks, NOT_RUN.
