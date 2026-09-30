# Merchant Studio v1 — one end-to-end product slice

Status: **FROZEN / ACCEPTED_LOCAL_MOCK_STU01_05** (2026-09-27).
Source baseline `320dad6`; LMW root full regression passed 645 tests, race/vet.
Independent preflight of draft `0af7e82` (Humaux
`33de752a-31c9-4697-991c-813bfb6e8910`) found no confirmed P0/P1; that preflight
alone did not establish STU01–05 acceptance. T08/T09 and G06/G07 remain incomplete.
Backend source/tests `2be9cd2` passed root focused eight-test PG/API/TLS/race gate
and full 652-test PG/race/vet regression. BFF `b18f977` passed real signed-login
transport, typecheck/build and existing order BFF regression. Integrated UI
`03928d5` passed root `05cb8ee` five-case actual browser gate, including native
calendar save/reopen and trusted native conceal. The orders regression passed
seven cases; root `b99a2d1` full PG/race/vet passed 696 tests. The later four-file
test-only diff leaves ordinary full/vet inputs unchanged; both affected browser
suites were rerun at `05cb8ee`. Independent final scope decision:
`f393fffb-1154-46c5-85b3-6a4f93bafd39`. Native BFCache restoration remains
unobserved, not a claimed success. See [backend evidence](../docs/implementation/2026-09-27-studio-backend-acceptance.md),
[BFF evidence](../docs/implementation/2026-09-27-studio-bff-acceptance.md) and
[UI/current regression evidence](../docs/implementation/2026-09-27-studio-ui-acceptance.md).
This is one local MOCK product slice; T08/T09, real input, Cloud and production
readiness remain incomplete.

## Product outcome and reuse

A merchant can find, create, edit and reopen a programme, inspect its durable
media state, and start/stop an explicitly labelled **MOCK rehearsal** when a
trusted internal fixture has prepared authority. Deliver domain reads, HTTP,
BFF and the approved Studio UI together; a green API-only test is not Studio
acceptance. Real room publishing, participant tokens, destination provisioning,
LiveKit Cloud and Meta audience visibility remain required downstream work.

Reuse `live.CreateDraft/UpdateDraft/GetDraft`, `MediaPlanner.PlanStart/RequestStop`,
the existing transaction and permission boundaries, native `river_media`,
pagination, OIDC BFF/CSRF/idempotency and `WorkspaceFrame`. No new scheduler,
transaction engine, credentials store or generic workflow abstraction.

## Domain and safe projection

Add `live.ListDrafts(ctx, tx, scope, token, pagination.Request)` returning
`pagination.Page[Draft]` and `live.GetStudio(ctx, tx, scope, token, sessionID)`
returning the fixed projection below. Both require `live:read`, scoped READ
COMMITTED and a final current-authorization check. Lists use descending
`(created_at,id)` keysets, the existing bounded two-key pagination pattern,
collection `live-sessions`, and an empty
array rather than null. Cursors bind tenant/store/collection; never OFFSET.

`Studio` has exactly `draft`, `prepared`, `attempt`, `can_manage`:

- `draft`: existing `Draft` JSON, unchanged.
- `prepared`: null or `{authorization_id, session_version, start_before,
  environment, destinations}`. At most one current, non-revoked, unexpired
  candidate for the exact session version/aspect and current binding versions;
  deterministic newest `(created_at,id)` wins. Require active tenant/store,
  programme DRAFT with no existing attempt, enabled media/destination bindings,
  matching provider/asset identity and current semantic versions, following
  the existing PlanStart predicates. It remains a candidate, not Start authority.
  `environment` is always `MOCK`.
  `destinations` is an ordinal-ordered array of `{ordinal, provider}` (1..2).
  It describes prepared targets, **not successful or publicly visible streams**.
- `attempt`: null or `{attempt_id, environment, operation_state, resource_state,
  transport_status, cleanup_required, stop_requested, stop_wire_count,
  escalated, updated_at, destinations}`. Destinations use the same safe ordinal/
  provider shape from the attempt's frozen authorization, not a newer candidate.
  These are persisted facts, not inferred success.
  No execution row yet means UNOBSERVED, empty transport status and no cleanup
  claim. A queued/UNKNOWN operation is never described as a live broadcast.
  Current one-attempt-per-session invariant remains unchanged.
- `can_manage`: current `live:manage` permission only, not provider readiness.
  Read-only merchants retain read access; missing manage is not an error for GET.

Use a narrow `live.read_studio_media(bytea,uuid,uuid)` SECURITY DEFINER projection
owned by the existing media writer. New migration `0038_studio_projection.sql`
must revoke PUBLIC and grant EXECUTE only to commerce_runtime. Validate token
hash, scope/principal/revision, READ COMMITTED and `identity.resolve_access`
before reading and again at the final database wall clock. Fail closed on an
incoherent attempt/operation/execution association. Scope every join explicitly.
No generic MEDIA_ATTEMPT operation RLS widening, no raw SELECT grant on private
media tables, no new function available to worker/executor/anonymous/buyer.

Never project ciphertext, nonce, key/project IDs, endpoint/stream URLs, binding
secrets, token hashes, lease tokens, provider IDs, raw errors or arbitrary
result_code. The Go decoder enforces the fixed typed projection and stable
error classes. The UI cannot select or mint an authorization from raw input.
No prepared authority means useful planning plus an explicit unavailable
rehearsal control; do not fabricate eligibility. PlanStart still rechecks all
authority, version, deadline, policy and transaction guards at mutation time.

## HTTP and process assembly

Base `/v1/admin/stores/{store_id}/live-sessions`:

| Method / suffix | Permission | Body / response |
| --- | --- | --- |
| GET base | live:read | only canonical `limit` and opaque `cursor`; Page[Draft] |
| POST base | live:manage | existing DraftInput; Draft |
| GET `/{session_id}` | live:read | no query; Studio |
| PATCH `/{session_id}` | live:manage | DraftInput plus expected_version; Draft |
| POST `/{session_id}/rehearsal/start` | live:manage | authorization_id, expected_session_version; safe receipt |
| POST `/{session_id}/rehearsal/stop` | live:manage | attempt_id; safe receipt |

Receipts contain only session_id, attempt_id and state; never expose internal
job/operation/room IDs from the planner result. Mutations use the existing
Idempotency-Key; no browser-selected tenant/principal/environment/provider URL.
Reject duplicate/unknown JSON fields, trailing values, invalid UTF-8, malformed
IDs, noncanonical query and extra query on non-list routes. Reuse bounded input
helpers where adequate; do not silently call the existing permissive decoder
"duplicate-safe". All responses, including errors, are private/no-store.

`httpapi.Options.Live *live.MediaPlanner` is nil by default and omits all these
routes. `cmd/api` owns this composition; `platform` must not import `live`.
`COMMERCE_STUDIO_ENABLED` accepts absent/empty/0 or 1 only. Enabled assembly
requires the existing enabled identity and literal loopback listener boundary.
Build one insert-only River client with schema river_media on the existing
runtime pool and inject its MediaPlanner; never start it in the API. Reuse
media_plan_ready preflight under the existing bounded startup context. No media
worker/executor pool, registrar role, provider client, keyring or credentials
are loaded by the API. Default-off construction must not touch this subsystem.

### Amendment G2 (2026-09-29, R1 release-gate ruling G2, `docs/delivery/units/r1-final-rulings.md`)

The flag is split. `COMMERCE_STUDIO_ENABLED=1` mounts only the planning routes (GET/POST base,
GET/PATCH `/{session_id}`) and enables keyword claims + claim-source; it builds no MediaPlanner and
does not query `live.media_plan_ready()`. `COMMERCE_STUDIO_MEDIA_ENABLED` (absent/empty/0 or 1;
1 requires Studio) additionally builds the planner and mounts the two rehearsal routes (and the
input routes when configured); with media off they are absent (404). `httpapi.Options.Studio`
carries the planning switch; `Live` non-nil implies it. The GET `/{session_id}` body gains one
read-only field, `media_enabled` (boolean: whether this API mounts the rehearsal routes); the admin
Studio hides the rehearsal column and controls when it is false. `Studio` above stays the domain
projection; `media_enabled` is added by the HTTP layer. R1 deploys STUDIO=1, CLAIMS=1, MEDIA=0
(preflight P06). Gates: `cmd/api` `TestStudioG2FlagMatrix`, `TestStudioPlanningOnlyG2APIProcess`
(real binary + PG), KC16/T12 browser gates in planning-only mode, smoke S45.

## BFF and browser

Only add the six exact method/path shapes to the existing scoped BFF allowlist.
Keep current-session/store binding, cookie-only browser auth, same-origin/CSRF,
request/response bounds, idempotency and private/no-store behaviour. No generic
path passthrough or fixture fallback. Route `/{locale}/studio` supports zh-CN,
zh-TW and en and the incumbent workspace shell. Visual composition must be
approved before UI authoring; the order page's C approval is not Studio approval.
On 2026-09-27 the user separately approved Studio B, `场次列表＋双区工作台`
(option `split`, seed `6373bb3f`). The binding comp is
`.impeccable/mocks/decision/studio-split.png`; the route brief is
`apps/admin/.impeccable/surfaces/app-locale-studio.md`. This satisfies the
composition-choice prerequisite only; STU04 implementation and acceptance remain
pending.

Expose planning, prepared rehearsal and actual media observation as distinct
states. There is no per-destination success evidence in the current storage;
display it as unverified, never copy one Egress status to Facebook/Instagram.
Do not add disabled pretend camera, recording, chat, product-pinning or audience
controls. Those capabilities remain real downstream requirements, not removed.

## Acceptance gates and delivery ownership

| Gate | Required evidence |
| --- | --- |
| STU01 | Independent PG18: draft list/create/edit/get, keyset/store isolation, permission changes and replay/version conflicts using existing business functions |
| STU02 | Runtime raw SQL and HTTP: safe projection exact fields, cross-store/token/expired/revoked denial, final wall-clock and association checks; no secret fields or widened private-table grants |
| STU03 | Real API + local TLS worker: current prepared authority → one Start → UNKNOWN/observed → authorized Stop → terminal or retained escalation; reload preserves facts, no duplicate Start/stop-budget reset or false destination success |
| STU04 | Real OIDC → production Next BFF → Go/PG browser workflow, desktop/mobile and three locales; save/reopen, read-only/no-prepared states, start/stop, CSRF and cross-store denial, no browser bearer or persistent media payload cache |
| STU05 | Default-off process, enabled startup admission, independent fixed-diff review, typecheck/build, existing regression suites and root full PG/race/vet; dependency/diagnosis docs plus fixture cleanup |

This interface is frozen after independent preflight. Integrator owns migration
number, contracts, API assembly and final merges; domain/HTTP author, BFF/UI
author and independent tests use separate worktrees and non-overlapping paths.
UI work follows approved composition B; backend and test preparation may proceed
after interface freeze. No unresolved P0/P1 is mergeable. Do not mark Studio,
T08/T09 or the SaaS complete until its entire required scope is proved.
