# Refund / fulfilment integrator rulings (2026-09-29) — binding for refund-core, fulfilment-core,
# owner-provisioning, refund-fulfilment-ui, refund-fulfilment-tests

All defaults proposed with the briefs are ACCEPTED:
1. D6 `identity.read_merchant_orders` replacement (with refund fields) moves to 0063; fulfilment-core
   alone owns the merchant order read (SQL + Go).
2. D4 new `identity.read_merchant_refunds` in 0062 with the round-2 reviewer's state rules.
3. D1 refund operation created like the checkout operation: UNKNOWN, `generation=1`, no lease.
4. D2 webhook prepare gains payment-intent and refund-metadata parameters; the 64-signal cap is
   enforced in prepare; ingress function allowlist updated accordingly.
5. D3 zero-match refund list within 15 min of the last send → snooze and list again.
6. D5/E5 policy names as written in the briefs; 0063 reuses them.
7. D7 `charge.refunded` reads keep the B1 loader's key version after rotation (documented limit).
8. D8 buyer `refund` field omitted for PAYUNi and when there is no refund activity (PAYUNi bytes
   unchanged).
9. E2 new read-only `GET /order-actions` returning the three permission booleans.
10. E3 buyer role gets SELECT on the shipment heads table.
11. E4 export sets tenant/store/principal GUCs itself before its audit row.
12. RF01/RF02 gate tests belong to the independent test unit.
13. RF11 split: BROWSER(MOCK) without keys + BROWSER(SANDBOX) with keys.
14. UI Q1–Q6: the UI brief's defaults (no new comp; native `<dialog>` for refund; three-language
    carrier names; existing badge palette; export button in the list toolbar; tracking link as a
    plain external link with `rel="noopener noreferrer"`). Owner may revise before go-live.

## Wave-3 rulings (2026-09-29)

15. Refund/fulfilment error codes (refundable_changed, exceeds_refundable, amount_step,
    not_refundable, refund_blocked_review, refund_limit, version_changed, not_shippable,
    invalid_carrier, invalid_tracking, invalid_url, void_requires_shipped, invalid_void) are added to
    `internal/httperror`'s code table centrally (root cause: unknown codes were rewritten to
    `internal`), with an httptest asserting each code reaches the JSON body.
16. Extra grant `SELECT(tenant_id,store_id,attempt_id)` on `payments.stripe_sessions` + policy
    `auth_refund_session_read` for `commerce_auth` (Stripe/PAYUNi discriminator): ratified.
17. Partial refund on a full-remaining-only attempt → 422 `refund_blocked_review`: ratified.
18. Extra export `stripe.EncodeRefundBody` (pins body_sha256 before POST, like EncodeCreateBody):
    ratified; add to the frozen interface list.
19. `validateRuntimeRiverPrivileges` (runtime login: exactly SELECT/INSERT/UPDATE(kind) on
    river_job + sequence USAGE in river, river_media, river_payment): ratified; the deploy
    provisioning script must match it (checked in the deploy unit).
20. Carrier name: NFC-normalize (not refuse) via golang.org/x/text/unicode/norm; go.mod tidied to a
    direct dependency and a row added to docs/engineering/dependencies.md.
21. RF04 (over-refund concurrency witness) may never be waived as "baseline"; every red RF/MF gate
    is triaged into product / test / dependency with evidence before merge.
22. Admin UI: new `apps/admin/components/order-actions.css` accepted; after a refund/shipment change
    that drops the selected order out of the current filter, keep the list and refresh only the
    detail; refund section visible for CAPTURED / PARTIALLY_REFUNDED / REFUNDED / REVIEW_REQUIRED
    (REVIEW_REQUIRED without capture shows "unavailable").
23. RF07 contract conflict (§4.4 currency change = identity mismatch → job ends, vs §6 step 4 →
    REFUND_AMOUNT_MISMATCH review): BOTH. A Stripe refund report whose currency differs from the
    request ends the job as `stripe_refund_mismatch` (no resend, no release) AND opens a
    `REFUND_AMOUNT_MISMATCH` review for a human; refundable capacity stays reserved. RF07 asserts
    all three. (Stripe refunds are always in the charge currency, so a mismatch means a wrong match
    or corrupted data — a human must look.)

## Wave-5 rulings (2026-09-29)

24. Owner provisioning grants SIX permissions to the store creator: live:read, live:manage,
    payments:refund, fulfillment:write, orders:export, integration:execute. Reason: the claim-source
    definer requires live:manage + integration:execute, without which the owner cannot bind their own
    posts. Contract stripe-refund-v1 §12 / OP01 / account_onboarding expectations amended (spec change).
25. Buyer tracking link: `rel="noopener noreferrer nofollow"` per manual-fulfilment-v1 §3.2 (ruling 14
    was incomplete; the contract text governs).
