# Unit claim-source — merchant binds a live session to a Facebook post / Instagram media

Ruling h (docs/delivery/units/meta-intake-rulings.md). Contract: contracts/meta-claims-intake-v1.md
§2 (`live.put_claim_source`, `live.claim_sources`). Two halves built in parallel against the
frozen HTTP interface below: **Go** (owned by meta-intake-reply phase B) and **UI** (unit
`claim-source-ui`, apps/admin only).

## Frozen HTTP interface (admin, merchant bearer via existing BFF)

Amended by wave-5 rulings o, p, s, t (meta-intake-rulings.md). Canonical path uses `live-sessions`
(matches the Studio/claims routes); there is no `/live/sessions/` alias.

`GET  /v1/admin/stores/{store_id}/live-sessions/{session_id}/claim-source`
→ 200 `{"source": null}` or `{"source": {"id","platform":"facebook"|"instagram","object":"page"|"instagram",
"asset_id","source_object_id","private_reply":bool,"reply_locale":"zh-TW"|"zh-CN"|"en","active":bool,
"version":int,"verified":bool,"intake_count":int,"intake_capped":int,"updated_at"}}`;
`verified=false` for `facebook` until probe U1 (UI shows "unverified" label).

`PUT  /v1/admin/stores/{store_id}/live-sessions/{session_id}/claim-source` (Idempotency-Key required)
body exactly `{"input":string,"private_reply":bool,"reply_locale":"zh-TW"|"zh-CN"|"en","active":bool,
"expected_version":int}` plus the optional `"platform":"facebook"|"instagram"` (ruling p; never null)
→ 200 same shape as GET's `source`. `platform` is required when the store has both an enabled Facebook
and an enabled Instagram binding and `input` is a bare numeric id (else `binding_ambiguous`); when given
it must agree with the parsed input (else `input_invalid`). A delivered `<page>_<post>` id is Facebook.
The UI shows a platform select only when both bindings exist and re-saves with the saved source's
platform (contracts/claim-source-openapi.json).
`input` is what the merchant pastes: a numeric id, a Facebook post/live-video URL
(`facebook.com/<page>/posts/<id>`, `/videos/<id>`, `fb.watch/...` is REJECTED — no redirect
following), or an Instagram URL (`instagram.com/p/<shortcode>`, `/reel/<shortcode>`) or numeric
media id. Server parses → (`object`, `source_object_id`); shortcodes that cannot be converted to a
media id without a Graph call are REJECTED with `input_unresolvable` (R1 has no Graph read path).
The page/IG `asset_id` comes from the store's single enabled Meta binding for that platform
(`integration.bindings`); zero or several → `binding_ambiguous` / `binding_missing`.

Errors (added to `internal/httperror` table): `input_invalid`, `input_unresolvable`,
`binding_missing`, `binding_ambiguous`, `page_token_missing` (ruling s: `private_reply=true` and no Page
token registered for the binding), `source_conflict` (object bound to another session),
`version_changed`, plus the standard `forbidden`, `not_found`. Permission: `live:manage` +
`integration:execute` (definer checks; 0065 grants both to the store creator, ruling 24).
UI defaults (ruling t): `private_reply=false`, `active=true`; `intake_capped` reads "Skipped: intake limit
reached"; the panel banner reads "Bind a Facebook or Instagram post to read comments automatically" with no
source and "Reading comments from the bound post" (+ "unverified" for facebook) when bound; three locales.

## Go half (meta-intake-reply phase B)
`internal/httpapi` route registration + input parser (pure function, unit-tested with the URL
table above incl. rejects), calling `live.put_claim_source` inside `command.Run`
(receipt + audit), OpenAPI entries, httperror codes. Real-PG gate: bind, rebind with CAS,
conflict across sessions, deactivate, permission denial, idempotent replay.

## UI half (`claim-source-ui`, apps/admin only)
In the approved Studio claims panel (`apps/admin/components/StudioClaims.tsx`, `claims.css`,
`claims-copy.ts`, `claims-client.ts`, `claims-request.ts`, BFF grammar): one section "Comment
source" with a single text input + private-reply checkbox + reply-locale select + active toggle,
a status line (bound object, platform, "unverified" label for facebook, intake counts), save with
Idempotency-Key reuse only for byte-identical retry after unknown outcome, re-read after save,
all error codes above in zh-CN/zh-TW/en, focus/aria like the existing panel. No new visual
decision (reuse panel styles); anything new → owner question with a default.
Verify: pnpm typecheck/build admin, the existing claims UI tests + new node tests for the request
grammar/validators; browser gate `claims-ui.spec.ts` extended (MOCK Go) — real chain runs after
phase B merges (`bash scripts/dev/test-local.sh --browser-live-claims`).
