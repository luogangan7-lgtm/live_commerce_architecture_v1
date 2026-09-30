# Unit design-after-payment — contract drafts: Stripe refund + manual fulfilment

Role: contract designer (read-only on code; writes two DRAFT contract files only).

## Goal
Draft two small, implementable contracts in the repo's contract style (see
`contracts/stripe-psp-v1.md` for structure: owner inputs, facts relied on with source URL +
retrieval date, flow, persistence, state machine, HTTP, idempotency, test gates table with
tiers, ownership, NOT_RUN/limits, open questions).

1. `contracts/stripe-refund-v1.md` — merchant-initiated full/partial refund of a CAPTURED
   Stripe payment (架构.md §12.3: REQUESTED→SUBMITTING→SUCCEEDED/FAILED/UNKNOWN; refund ≤
   captured − already refunded under concurrency; UNKNOWN never blind-retried, same
   Idempotency-Key replay only; webhook `charge.refunded`/`refund.updated` + poll). Reuse the
   Stripe stage-A adapter patterns, the webhook inbox of §9.1, River job style of §8. State
   precisely how stock/order status change (or don't) on refund, the merchant permission
   needed, audit rows, and the merchant admin UI + buyer view surface. Gates RF01..RFnn with
   tiers UNIT/REAL_PG/MOCK/SANDBOX/BROWSER. Stripe facts: use WebFetch on docs.stripe.com
   (Refunds API, refund object statuses, idempotency, `charge.refunded`, `refund.updated`,
   `refund.failed`), cite URL + date 2026-09-28.
2. `contracts/manual-fulfilment-v1.md` — 架构.md §13.4 manual path for R1: merchant marks an
   order (or its package) shipped with carrier name (free text + optional known-carrier enum incl.
   7-ELEVEN/FamilyMart CVS, SF Express, Chunghwa Post), tracking number, optional tracking URL
   template; corrections are new versions (audited, never overwrite); buyer order page shows
   status + tracking; CSV export of paid-unshipped orders for the merchant's external carrier
   tool. Must build on `internal/merchantorders` and existing order states — read their
   contracts (`contracts/merchant-orders-v1.md`, `buyer-order-history-v1.md`) by section.
   Gates MF01..MFnn.

## Read
`docs/delivery/PROCESS.md`; 架构.md §12.3, §13 (grep headings); `contracts/stripe-psp-v1.md`
§0.1, §6.2, §7, §8, §9.1, §16; `contracts/merchant-orders-v1.md`; `contracts/payment-capture-v1.md`
(state names); `internal/merchantorders` package comment and exported API (grep `^func [A-Z]`).

## Write paths
Only the two new contract files (status header `DRAFT`, date 2026-09-28).

## Constraints
- Smallest design that is correct; reuse existing tables/workers/roles; no new infrastructure.
- Migration numbers: refund = 0062, fulfilment = 0063 (post-River numbers 0013+ if needed).
- List every assumption as an explicit "Integrator ruling needed" item instead of guessing.

## Return
Paths, a 10-line summary of each design, and the ruling-needed list.
