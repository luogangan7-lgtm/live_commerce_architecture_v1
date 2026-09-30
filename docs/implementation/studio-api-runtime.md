# Merchant Studio API: assembly and diagnosis

Scope: local MOCK planning/rehearsal API and merchant BFF. The six routes are defined in
[studio-v1](../../contracts/studio-v1.md); UI, real destination provisioning,
Cloud and production activation are separate gates. No new dependencies.

## Call and privilege boundaries

`cmd/api.loadStudioConfig` → existing identity configuration and literal loopback
listener validation → existing 10-second startup context → runtime pool
`live.media_plan_ready()` → insert-only River client (`river_media`) →
`live.NewMediaPlanner` → `httpapi.Options.Live`.

`COMMERCE_STUDIO_ENABLED` is absent/empty/0 by default; only 1 enables it. Enabled
configuration requires identity to be enabled. Since R1 ruling G2 it mounts the planning
routes only (`httpapi.Options.Studio`); the media planner above is built only with
`COMMERCE_STUDIO_MEDIA_ENABLED=1` (requires Studio), and a nil planner leaves the rehearsal
and input routes unmounted (404). See the G2 amendment in `contracts/studio-v1.md`. The API never starts River, loads media secrets or receives a media-worker,
executor or registrar pool. The separate media process retains execution ownership.
Do not enable this on customer systems as part of local verification.

- Reads: scoped HTTP → `platform.WithScope` → `ListDrafts` / `GetStudio`.
  Lists reuse tenant/store/collection-bound two-key pagination. `GetStudio`
  reuses `GetDraft`, pins the resolved authorization revision in a transaction-local
  GUC, reads the narrow SQL projection, then rereads the draft and checks current
  permissions. A draft change between reads fails closed, including a Start that
  changes programme state but does not increment session version.
- Private data: migration 0038 installs `live.read_studio_media`. The existing
  media writer owns it; only the runtime gets EXECUTE, not raw private-table SELECT.
  It validates current scope, token, revision, association and final DB wall clock.
  It returns a fixed safe shape, not keys, stream URLs, provider IDs or raw errors.
- Writes: existing `CreateDraft` / `UpdateDraft` / `PlanStart` / `RequestStop`.
  Idempotency, authority and version checks remain in those existing business
  functions. Safe HTTP receipts omit internal operation/job/room identifiers.
- Input: exact method/path shapes, canonical raw query, 64 KiB UTF-8 JSON,
  duplicate/unknown/case-alias keys and trailing values rejected. Studio responses
  and method errors are `private, no-store`.

Prepared authority is advisory and MOCK-only. Saving a draft does not create it.
The current implementation permits one attempt per session. Persisted UNKNOWN,
UNOBSERVED or OBSERVED states are not evidence of a public broadcast. Destination
names are frozen targets only; there is no per-destination success projection.

## Diagnosis and upgrade gates

The existing Next proxy rejects noncanonical Studio query/path before framework
normalization. The catch-all route allows six exact shapes, requires real
authConfig (never fixture fallback), and reuses current cookie/store authority,
Origin/CSRF, idempotency and callBackend. Shared readBody now accepts headers/body
from Request or Response, rejects invalid UTF-8 and preserves BOM for strict JSON
rejection. It bounds requests at 64 KiB and Studio responses at 256 KiB. Other
legacy proxy body handling is unchanged. safeError sanitizes bounded upstream
errors; every Studio response is private/no-store. No browser bearer is forwarded.

Changes here require `pnpm run typecheck:admin`,
`bash scripts/dev/test-local.sh --browser-studio-bff` and the existing
`--browser-merchant-orders-bff` regression. The selectors build the production
Next package and own local signed IdP/HTTP/PG fixtures. This is transport evidence,
not visual/Studio interaction acceptance. No new client/state/auth framework.

| Observation | Inspect without weakening the boundary |
| --- | --- |
| Routes absent | Explicit flag, enabled identity and planner injection; do not enable fixture fallback |
| `studio_invalid_config` | Exact 0/1 flag, identity and literal loopback listener |
| `studio_database_unavailable` | Correct runtime role, forward migrations and native River/post-River readiness; do not substitute a privileged pool |
| Scoped read forbidden | Session/grants/current revision and transaction-local GUC; never bypass the SQL scope check |
| Retriable projection failure during concurrent edit/start | Reread current state; do not combine old draft with new media facts or replay Start with a new key |
| No prepared candidate | DRAFT state, exact version/aspect, active bindings, deadline and revocation; do not mint browser-selected authority |
| UNKNOWN/cleanup liability | Existing media-worker evidence and bounded recovery, not duplicate Start or guessed destination success |

Changes to SQL/roles, pgx, River, permission checks, strict input or composition
require `bash scripts/dev/test-local.sh --studio-backend` and the complete
`bash scripts/dev/test-local.sh` (PG18/race/vet), plus fixed-diff independent review.
The focused gate includes a separately built actual API and media worker against
local TLS; the harness is race-enabled, those child binaries are not. BFF and UI
also require their own real signed-login/Next/Go/PG browser gates; backend success
does not establish STU04 or deployability.
