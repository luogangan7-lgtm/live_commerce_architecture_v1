# Stripe buyer payment UI v1 (stage B2)

Status: **FROZEN 2026-09-29 (after adversarial review round 1; P1-1..3 and P2s applied)**. Evidence: DESIGN. Amends
[buyer-payment-public-v1](buyer-payment-public-v1.md) and [buyer-payment-ui-v1](buyer-payment-ui-v1.md) for
Stripe only; implements [stripe-psp-v1](stripe-psp-v1.md) §9.2, §9.3, SP14 (BFF half), SP18. PAYUNi
bytes, copy and layout unchanged. Go side: `docs/delivery/units/stripe-b1-{start-http,rulings}.md`.

## 1. Visual authority

Reuse the approved B payment section as built (`docs/implementation/2026-09-25-buyer-payment-ui-design-record.md`):
same section, lines, `payment-pay`, notes, live regions, Refresh order, `/payment/return` and CSP (Stripe
navigates, no `form-action`) unchanged. No new token/CSS/layout/component; only §7 copy and §10 Q1.

## 2. BFF routes (`apps/storefront/lib/buyer-server.ts`, `/api/buyer/` → `/v1/buyer/`)

| Public route | Request | 200 body validator |
| --- | --- | --- |
| GET `orders/{id}/payment` | unchanged | `validOrderPayment` (§3) |
| POST `.../payment/prepare` | JSON exactly `method_code` ∈ {`payuni_credit`,`stripe_checkout`}, `method_version`, `locale`; Idempotency-Key required | `validPaymentPrepared(v, id, method_code)` |
| POST `.../payment/handoff` | no body, no key | `validHostedHandoff` (union, §3) |
| POST `.../payment/refresh` (new) | no body, no key | `validPaymentSignal` = exactly `{order_id: id, scheduled: boolean}` |
| POST `.../payment/cancel` (new) | no body, no key | `validPaymentSignal` |

- Route regex becomes `^orders\/([^/]+)\/payment(?:\/(prepare|handoff|refresh|cancel))?$`. Refresh and
  cancel get the handoff keyless exception in both `buyer-server.ts` and `buyer-client.ts` (supplied
  body or key → reject), keep CSRF/Origin/context/journal checks, one fetch, `redirect:error`, no-store.
- Every handoff/refresh/cancel error is nonretryable and sanitized (disabled, 503, 429, deadline,
  network loss, malformed success). Invalid 200 bodies → existing sanitized 503.

## 3. Validator deltas (`apps/storefront/lib/payment-contract.ts`, dependency-free)

**OrderPayment**: nine fields plus optional `cancel_requested: boolean` (Go emits it only for Stripe
attempts). Enums add `CLOSED_UNPAID` and handoff `CREATING|READY|CLOSED`. Reject unless:
- `CREATING|READY|CLOSED` or `CLOSED_UNPAID` ⇒ `cancel_requested` present; those handoffs need a UTC
  `handoff_expires_at`. `cancel_requested` ⇒ handoff ∉ {`PREPARED`,`ISSUED`,`EXPIRED`}, `methods` empty.
- `methods` ≤ 2, distinct codes ∈ {`payuni_credit`,`stripe_checkout`}, existing field/name/state rules.
  A view is Stripe iff `cancel_requested` is present or it is fresh with `methods[0].code==="stripe_checkout"`.

**PaymentPrepared** gains a method parameter (the BFF knows it from the validated request, the client
from its marker). `payuni_credit`: unchanged (TWD 100..19999900, %100). `stripe_checkout`: currency and
bounds exactly the stripe-psp §0.2 corrected table — HKD 400..99999999; USD, SGD 50..99999999; MYR
200..99999999; TWD 2500..99999900 step 100 (TWD min 2500: Stripe SANDBOX rejected 100/1200, accepted 2500 (2026-09-29)). Anything else rejects.

**HostedHandoff** is a union keyed by disjoint dispositions. PAYUNi `ISSUED`/`ALREADY_ISSUED` unchanged.
Stripe: exact keys `order_id, disposition, expires_at` for `CREATING|CLOSED|UNAVAILABLE`; plus
`redirect_url` only for `REDIRECT`. `redirect_url` must match `^https://checkout\.stripe\.com/[!-~]{1,4000}$`
**and** `new URL(u).origin === "https://checkout.stripe.com"` with empty username/password.
Required negatives: `http:`, `checkout.stripe.com.evil`, `checkout.stripe.com@evil`, `:443`,
upper-case host, space/control/non-ASCII, 4001 path chars, missing path, URL on non-REDIRECT, form on Stripe,
and the Stripe path rejecting PAYUNi `ISSUED`/`ALREADY_ISSUED`.

## 4. Handoff mechanics — why a child window

Stripe `success_url`/`cancel_url` is the central neutral return (no tenant, no session id). A same-tab
redirect would unload the store tab — the only place that knows the tenant, polls and offers cancel —
and strand the buyer on a page that cannot find their store. So B2 keeps the approved child window:

1. Click (Pay or Continue) → synchronously `openPaymentDestination(locale)` (existing: `about:blank`,
   `opener=null`, no-referrer meta, wait text). Nothing awaits before this.
2. Inside the existing `commerce-purchase-write-v1` Web Lock with the existing fences: GET view, match
   order snapshot (order_id, currency, total). Stripe path: view NOT_STARTED/DRAFT/NONE with marker →
   replay prepare with the marker's key and body; without marker → persist a new marker, then prepare
   (`method_code:"stripe_checkout"`), validate prepared amount/currency = snapshot. View
   PENDING/AWAITING_PAYMENT/CREATING|READY with `cancel_requested=false` → skip prepare whether or not a
   marker exists and go to step 3. Any other view → render, no POST. A PAYUNi marker on a Stripe view,
   or the reverse, → `failed`. (Stripe never writes `handoff_started`, so the prepare marker stays.)
3. Release the lock after prepare. GET until `handoff_state` is `READY` (1,1,2,2,3,3,5,5,8 s; 30 s
   budget; child keeps its wait text; the lock is not held, so other tabs' purchase writes never stall;
   this poll pauses the §6 background poll). Budget exceeded → close the still-owned blank child, show
   `creating`, button stays.
4. Re-acquire the lock for the handoff POST; re-GET first and abort (close child, render) if
   `cancel_requested`. Exactly **one** POST handoff per click. `REDIRECT` → `destination.navigate(url)`:
   re-validate (§3), check `owned()`, set `used` before `child.location.replace(url)`.
   `CLOSED`/`UNAVAILABLE`/error → close owned blank child, GET, render. The URL lives only in that
   local variable.
5. Continue (cross-device or after closing the child) needs no marker: D16 makes a repeat REDIRECT to
   the authenticated owner harmless. This deliberately differs from PAYUNi's one-shot read-only rule.

`PaymentDestination` gains `navigate(url)`; `submit` unchanged.
**Popup blocked** (null/closed/unowned child): existing `blocked` alert, zero BFF calls; no same-tab or
link fallback (above); the next click retries safely. Mobile Chromium opens a tab; same code.

## 5. UI state machine (Stripe orders; PAYUNi table in buyer-payment-ui-v1 unchanged)

Status line = `copy[payment_state]`, order line = `orderCopy[commercial_state]`, both from one view.

| View (payment / commercial / handoff, cancel_requested) | Actions | Note / message |
| --- | --- | --- |
| NOT_STARTED / DRAFT / NONE, one stripe method | Pay (`pay`) | method name + `explain` |
| same, methods ≠ 1 | none | `unavailable` (see Q3) |
| PENDING / AWAITING_PAYMENT / CREATING or READY, false, now < cutoff | Continue (`recover`), Cancel | `explain`; `submitted` after a REDIRECT |
| PENDING / AWAITING_PAYMENT / UNAVAILABLE, false, now ≥ `handoff_expires_at` | Cancel | `cutoff` |
| same UNAVAILABLE view, otherwise | Cancel | `readOnly` |
| any non-terminal, true | none | `cancelling` |
| CAPTURED / CONFIRMED | none | — |
| CLOSED_UNPAID / CANCELLED / CLOSED | none | — (status says closed without charge) |
| REVIEW_REQUIRED or AUTHORIZED | none | existing read-only |

Cutoff = `handoff_expires_at` (hint; Go enforces; SQL returns UNAVAILABLE once now ≥ it, so the `cutoff`
row needs UNAVAILABLE and a client clock past it). Terminal = CAPTURED, CLOSED_UNPAID, REVIEW_REQUIRED.
Stripe errors, the CREATING budget and CLOSED/UNAVAILABLE show `failed` (or `creating` for the budget),
never `uncertain` (PAYUNi-only: a Stripe page may be reopened, D16).
**Cancel**: click → native `window.confirm(copy.cancelConfirm)` (Q1) → one POST cancel → GET;
`scheduled=false` is fine; errors → `failed`, never auto-retried. Cancel is disabled while Pay/Continue
is in flight (Go's `take_stripe_handoff` does not check `cancel_requested`, hence the step-4 re-GET).
When Cancel/Continue disappears, focus moves to the payment status line (§8).
**Refresh**: no new control. The existing **Refresh order** click (the return page already names it)
also POSTs `payment/refresh` while the last view is a non-terminal Stripe attempt, through a handler
`OrderPayment` registers on a parent ref only then; ≤1 POST/10 s/order, ≤30 per page life (mirrors
the SQL throttle), `scheduled=false` silent. No effect, mount, focus, poll or remount ever POSTs.

## 6. Polling and storage

- GET-only poll while a Stripe attempt is non-terminal, mounted and visible: 5, 10, 20, 30 s, then
  30 s; stops 10 min after the last Pay/Continue/Refresh/Cancel or focus/pageshow (these restart it),
  on terminal state or epoch change. Mount counts as a restart of the 10-minute window; the step-3
  READY poll pauses this background poll. Existing focus/pageshow/storage GETs stay.
- Storage: only the existing marker `commerce-order-payment-v1:<context>:<order>`, `method_code` widened
  to `stripe_checkout` (v=1; old readers fail closed); Stripe never writes `handoff_started`. No URL,
  session id, amount or PII in any storage, cookie, query, console or log; no Stripe URL in the store
  tab's URL or history API calls (the child tab's own history necessarily holds it).

## 7. Copy keys (`apps/storefront/lib/payment-copy.ts`; wording per Q2 ruling)

| Key | en | zh-CN | zh-TW |
| --- | --- | --- | --- |
| `CLOSED_UNPAID` | Payment closed without charge | 付款已关闭，未扣款 | 付款已關閉，未扣款 |
| `cancel` | Cancel payment | 取消付款 | 取消付款 |
| `cancelConfirm` | Cancel this payment? The order is cancelled once the payment provider confirms. If you already paid, the payment stands. | 确定取消付款？支付服务商确认后订单会取消。如已付款，以付款为准。 | 確定取消付款？支付服務商確認後訂單會取消。如已付款，以付款為準。 |
| `cancelling` | Cancellation requested. Waiting for the payment provider to confirm; refresh this order to check. | 已请求取消，正在等待支付服务商确认，请刷新此订单查询。 | 已請求取消，正在等待支付服務商確認，請重新整理此訂單查詢。 |
| `creating` | The secure payment page is still being prepared. Try Continue original payment again shortly. | 安全付款页仍在准备中，请稍后再点击“继续原付款请求”。 | 安全付款頁仍在準備中，請稍後再點選「繼續原付款請求」。 |
| `cutoff` | This payment page can no longer be opened. Refresh this order for the result. | 此付款页已无法再打开，请刷新此订单查询结果。 | 此付款頁已無法再開啟，請重新整理此訂單查詢結果。 |

`Copy` type keeps one key set for all locales (compile-time completeness). `packages/i18n` unchanged.
The existing `uncertain` copy ("a payment page that was already issued will not be issued again") is
PAYUNi-only; Stripe states never show it (§5).

## 8. Accessibility

Native buttons named by visible text; one `role=status` announcement per state change, errors
`role=alert`, same-state polls render nothing; `aria-busy` while reading/sending; keyboard-only flow;
existing 48px target and focus ring; state always in text, never colour alone. When Cancel/Continue
disappears, focus moves to the payment status line (`tabIndex=-1`).

## 9. Gates (test author ≠ implementer; red run before green per PROCESS §2.4)

| Gate | Tier | Required |
| --- | --- | --- |
| SU01 | Node | §3 validators: positives, every listed URL negative, union/consistency negatives, per-currency prepared bounds; all existing BPT03 vectors unchanged |
| SU02 | Node | BFF: 5 routes exact upstream path/headers/body/key, one fetch; refresh/cancel keyless+CSRF/Origin/context denials; every handoff/refresh/cancel error nonretryable; hostile bodies → 503; PAYUNi responses byte-identical |
| SU03 | Node | Controller: fresh, marker replay, CREATING wait + budget, one handoff POST per click, navigate validation, cross-device Continue, cutoff, cancel once, refresh throttle, blocked/closed/unowned child, no URL in storage |
| SU04 | build | `pnpm run typecheck:storefront`, `pnpm run build:storefront` exit 0 |
| SU05 | BROWSER (MOCK) | Existing `test-local.sh --browser-payment` passes unchanged; PAYUNi `order-payment` DOM (UUID/time-masked) and element screenshots equal to base-SHA captures |
| SU06 | BROWSER (SANDBOX) = **SP18** | Real Next→Go→PG→worker→`checkout.stripe.com`: 4242 → neutral return → Refresh order → CAPTURED/CONFIRMED; `4000 0000 0000 0002` decline → stays open → Cancel → CLOSED_UNPAID/CANCELLED, stock restored; 3DS `4000 0027 6000 3184` → complete → CAPTURED. Each scenario on desktop and mobile Chromium; every locale on both viewports |
| SU07 | BROWSER (local) | Popup blocked → zero calls; child closed → Continue reuses the same session (1 session row); double click/two tabs → 1 prepare, 1 session; reload during CREATING → no POST; return GET/POST cannot change state |
| SU08 | BROWSER | Storage/cookie/console/Next+Go log scan: no `checkout.stripe.com` URL, `cs_test_`, key or PII sentinel |
| SU09 | BROWSER | Keyboard-only flow, roles/names, one announcement per state; 3-locale screenshots of every §5 row, hashed. Rows SANDBOX cannot reach (REVIEW_REQUIRED, UNAVAILABLE, `cutoff`, CREATING-budget, CLOSED_UNPAID screenshots) run in a BROWSER(MOCK) assembly using `stripetest` plus a fixture clock, labelled MOCK. No AUTHORIZED row for Stripe |
| SU10 | REVIEW | Bounded visual audit (BPU04 style) of new states + independent security review; DESIGN.md/sidecar unchanged |

SU06 is SANDBOX, never LIVE. NOT_RUN: SP17 (capture proven by refresh signal + poller), non-Chromium, devices.

## 10. Owner questions (defaults ruled below; owner may still change before go-live)

- **Q1 Cancel control.** New button in the section. Default: base `button` style (as Refresh order),
  after Continue, native `window.confirm`. Approve or specify another treatment.
- **Q2 Copy.** Approve the six §7 strings in three locales. **Q3 Two methods.** A store with both PAYUNi
  and Stripe needs a method chooser (new visual). R1 default: operator enables one method per store;
  UI shows `unavailable` for methods ≠ 1.

## Integrator rulings (2026-09-29)

1. **Q1 Cancel payment button:** same plain style as "Refresh order", placed after Continue, confirmed with
   the browser's native `window.confirm`.
2. **Q2 Copy:** the six new §7 strings ship as drafted in zh-CN/zh-TW/en; owner may reword before go-live.
3. **Q3 Methods:** R1 enables one payment method per store (Stripe); a method chooser is R2. The
   "unavailable" note wording for a store with no method stays as is.

## Integrator rulings at merge (2026-09-29)

- UI implementer calls accepted: (1) CREATING/READY past the local clock's cutoff shows Cancel +
  `cutoff` note; (2) Stripe operation errors stay visible until the next buyer action; (3) the
  refresh throttle (10 s, 30 per page life) lives in `requestPaymentSignal`; (4)
  `validPaymentPrepared(body, request, method)` keeps the required `method` argument.
- F2 accepted: a revoked qualification / disabled binding / disabled method leaves the pinned view
  at READY with `methods []` and the handoff answers UNAVAILABLE (fail closed; nothing persists).
- F3 **changed**: real Stripe SANDBOX rejected TWD 100 and 1200 minor and accepted 2500. The TWD
  local minimum becomes **2500 minor (NT$25)** in every layer that encodes it (stripe-psp-v1 §4
  table, Go amount checks, SQL `stripe_amount_ok`/`stripe_unit_amount`, storefront
  `STRIPE_AMOUNT` table) — one follow-up unit, with SP02 parity vectors updated to match.
