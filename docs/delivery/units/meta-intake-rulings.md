# Meta intake integrator rulings (2026-09-29) — binding for meta-intake-core, meta-intake-reply,
# meta-intake-tests, claim-source-ui

a. `claims.meta_intake.live_media boolean` column + trailing `p_live_media` parameter: ACCEPTED.
b. `origin` (storefront origin) added to the frozen reply request: ACCEPTED.
c. Instagram comment time = the inbox event's `occurred_at`; `p_occurred` NULL or equal: ACCEPTED.
d. KC03 edits: exactly the privilege rows of contract §4.3 (privilege matrix, denied list, schema
   ACL, writer EXECUTE) and the T06 allowlist additions — nothing broader: ACCEPTED.
e. The stricter lease inequality applies only to routes that set `LoadSecret`: ACCEPTED.
f. Graph: token in the JSON body (U6); API version required config, no default (U5); no 4xx
   treated as final until U3 is probed LIVE; Page-token env names `COMMERCE_META_PAGE_TOKEN_
   ACTIVE_KEY_ID` / `COMMERCE_META_PAGE_TOKEN_KEYS_JSON`: ACCEPTED.
g. Merge order for shared files (`tests/foundation/external_operation_authority_test.go`,
   `internal/platform/platform.go`): refund-core merges first, meta-intake-core rebases/merges
   after; the integrator resolves the allowlist union.
h. Gap closed: merchant binding of a live session to a Meta post (`live.put_claim_source`).
   R1 = merchant pastes a Facebook post/live-video URL or id, or an Instagram media URL or id;
   the server parses/validates it, checks it against `meta_inbox.routes` for the store's enabled
   binding, and calls `put_claim_source`. Go route owned by meta-intake-reply phase B
   (admin HTTP under the existing Studio/claims merchant routes); the Studio panel field is unit
   `claim-source-ui` (reuses the approved Studio claims panel; one text field + status line).
   No Graph listing of posts in R1 (needs a Page token read path; R2).

## Wave-3 rulings (2026-09-29)

i. §4.3 privilege table AMENDED (ratifies meta-intake-core round-2): `commerce_integration_writer`
   gets EXECUTE `identity.principal_holds` + USAGE on schema `identity` (needed so
   `register_meta_page_token` enforces §7 "owner membership + store validated");
   `commerce_meta_registrar` gets USAGE on schema `integration`; `meta_page_heads` UPDATE grant is
   `(current_version, updated_at)`. MCI02 compares against this amended table.
j. RATE_LIMITED stays out of the merchant claims-board `Stats.Rejected` (7 keys) until the
   claims-board UI adds it in three locales; the `internal/claims/merchant.go` edit is ratified.
k. §5.4 exact job link applies only to jobs inserted by the intake login (keeps T06's deliberate
   duplicate-job gate valid): ratified. The CHECK `events_source_platform` addition: ratified.
l. Facebook top-level comment rule `parent_id == post_id` treated as top-level (docs-based, LIVE
   unverified → probe U-list); late webhooks use the window's current match mode: ratified.
m. Reply origin: `plan_claim_reply` writes the stored `control.storefront_domains.origin` form
   (`https://host`); `RenderClaimLink` accepting a bare host too is fine; tests use the stored form.
n. metareply keyring env loader may not reuse the unexported `meta.parseStrict`; duplicate JSON member
   names collapsing is accepted for R1 (operator-controlled env; `ponytail:` marked). Upgrade path:
   export a strict parser from `internal/integrations/meta` in the maintainability unit.

## Wave-5 rulings (2026-09-29)

o. claim-source route spelling: canonical `/v1/admin/stores/{store_id}/live-sessions/{session_id}/claim-source`
   (matches existing Studio/claims routes); the `/live/sessions/` alias is removed; brief amended.
p. PUT body gains optional `"platform":"facebook"|"instagram"` (frozen-interface amendment by the
   integrator). Required when the store has both an enabled Facebook and Instagram binding and the
   input is a bare numeric id; otherwise optional and must agree with the parsed input. The UI shows a
   platform select only when both bindings exist; re-saving uses the saved source's platform.
q. `ensureWindow` default CLOSED/EXACT generation-0 `live.claim_windows` row: ratified.
r. Facebook `/videos/<id>` and bare ids stored as `<page_asset>_<id>`, `verified=false` until probe U1.
s. New error `page_token_missing` when `private_reply=true` and no Page token is registered for the
   binding (distinct from `binding_missing`); copy in three locales.
t. UI defaults `private_reply=false`, `active=true`; `intake_capped` copy "Skipped: intake limit reached".
   The stale "MOCK capture — comments are not read automatically yet" banner becomes: no source →
   "Bind a Facebook or Instagram post to read comments automatically"; bound → "Reading comments from
   the bound post" (+ "unverified" for facebook); three locales.

## Wave-5 integration rulings (2026-09-29, first MCI run against the merged code)

u. Contract conflict §5.3 step 3 ("§6.2 only for a source with `private_reply=true`") vs the MCI07 row
   ("source `private_reply` off before apply → one `claim_reply_skipped:source_off` audit"): §5.3 governs.
   A source with replies off writes no skip audit (no audit row per bundle for merchants who never enabled
   replies); the audited `source_off` skip is the race inside §6.2 (`claim_reply_plannable` sees the flag off
   under its lock). MCI07 asserts both.
v. MCI triage (product vs test, evidence output/r1-integration/wave5/pg-mci-*.log): product fixes — 0064
   column COMMENT ON for the five new tables (PROCESS §5), metareply doc.go Graph docs URL + date, claims-worker
   docs no longer spell forbidden env names (MCI10 greps them). Test fixes, each proven by contract text —
   ruling i rows in the MCI02 expected set (+ PUBLIC-inherited USAGE on schema public for the new role), token
   registrar principal holds `integration:manage` (ruling i), REPEATABLE READ cannot lease (§5.3), requeue
   clears `applied_event_id` (0064 CHECK), S03 offer on a second SKU (KC04 one active offer per SKU),
   RATE_LIMITED row keeps offer/quantity (§4.4 clause 3), consumer restarts close their pools
   (max_connections=30 gate), `link_system_issue` probe uses a principal GUC ≠ row principal (0060 `link_issue`
   is the merchant path), binding re-point drops the token rows first (0064 FK), dispatcher binding gate is
   STALE_BINDING (external-operation-v1 §5, §6.3).

## Integrator ratification after wave-5 integration (2026-09-29)

w. The claim-source GET response carries `platforms` (the store's enabled Meta platforms) so the UI knows
   when to show the platform select: ratified as an interface amendment (OpenAPI updated).
x. Rulings u and v recorded by the integration agent (§5.3 wins over the MCI07 row: no `source_off`
   audit when private replies are off; MCI triage list): ratified.
