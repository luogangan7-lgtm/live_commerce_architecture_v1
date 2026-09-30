# Claims retention purge and actor-level deletion v1 — U08 (R2)

Status: **v1 FROZEN 2026-09-30 (DESIGN; gates NOT_RUN)** — contract author, unit U08, ruling X4
(`docs/delivery/units/r2-design-rulings.md` Round-3). One adversarial review applied (§13 ledger), FROZEN per
X6 (PROCESS.md §2.2). Evidence label of this file: DESIGN. Every CRP gate is NOT_RUN. Nothing
here authorizes a production purge, a real actor erasure or a LIVE Meta call.

Closes the claims production-mount blocker of [live-keyword-claims-v1](live-keyword-claims-v1.md) §8/§11.4
and [meta-claims-intake-v1](meta-claims-intake-v1.md) §1/§8/§14 (lifting criteria §10). Until §10 holds,
waiver W1 applies (owner's own pilot store only). Reuses, without editing them: the definer/role pattern of
0060 (`commerce_claims_writer`), the lock-only UPDATE grant and pool-validator pattern of 0064/MCI §4.1,
§4.3, the operator-CLI pattern of `cmd/meta-admin` (0066), the relabel form and restore-replay model of
[customers-billing-v1](customers-billing-v1.md) CD7/CD8/CB05, and `meta_inbox.purge_expired` (0028: inbox
bodies already expire after 24 h / 7 d; not changed here). Authoritative: 架构 §14.4, §21.4 (U08 sets the
production retention configuration; restore replays deletion tombstones first); invariants I11, I22.
Where they conflict, this file is wrong except where §9 lists an amendment clause.

Migration: **`0071_claims_retention.sql`** (released to U08 by ruling R-6). No post-River file (IR-4).
Integrator-owned; renumbered at merge if needed.

## 0. Facts, decisions and rejected options

Facts read from the repo (2026-09-30), not assumed:
- Personal/pseudonymous claims data and where it lives: `claims.bundles.actor_key` (manual: random; Meta:
  `hex(HMAC(K_actor,"meta-claim-actor/v1",object,asset,from.id))`, `internal/integrations/meta/claim_intake.go`
  `ClaimActorKey`, deterministic), `claims.bundles.label` (merchant-typed name, manual only, CHECK
  `(platform='manual')=(label IS NOT NULL)`), `claims.bundles.owner_id/bound_at` (buyer binding),
  `claims.links.token_hash` (72 h TTL, CHECK), `claims.meta_intake.actor_key/comment_ref`
  (`UNIQUE(object,asset_id,comment_ref)` = per-comment dedup), `integration.operations.request.comment_ref`
  and `semantic_key = 'mpr:'||sha256(object|asset|comment_ref)[:48]` (0064 `plan_claim_reply`),
  `social.messages`/`social.comment_events` ciphertext and `social.conversations.peer_key` =
  unkeyed `tupleHash("meta-social-peer/v1",app,object,asset,sender.id)`; `social.comment_events.comment_key` =
  `tupleHash("meta-social-comment/v1",app,object,asset,comment_id)` (`projection.go`). The comment sender is
  only inside ciphertext; `social.comment_events.event_id` = the inbox event id = `claims.meta_intake.inbox_event_id`.
- None of 0029/0060/0064 has an expiry or a DELETE grant for these rows. `claims.events`, `claims.lines`
  and `live.*` hold no person identifier (events carry `bundle_id` only, claims §3.1).
- FKs into the purge targets: `claims.lines/events/links → claims.bundles`, `social.messages →
  social.conversations`; nothing references `claims.meta_intake`, `claims.links` or `social.*` rows.
- `ops.audit_events.principal_id` is NOT NULL + membership FK (0001), so a platform operator cannot write it.
- `commerce_worker` already holds full main-`river` table/sequence grants (`migrations/migrate.go:132-135`);
  River v0.40.0 `PeriodicJob` is available; no `cmd/*` registers a periodic job today.
- Meta data deletion (customers-billing F-P2): callback `signed_request` carries an **app-scoped user id**;
  v1 ships the instructions page (Q9). Whether webhook `from.id` equals that id is **UNKNOWN (U-D1 = MCI U8)**.

| # | Decision | Why / risk closed |
| --- | --- | --- |
| RD1 | **De-identify bundles in place; delete only leaf rows.** A purged bundle gets `actor_key` := fresh random 64 hex, manual `label` := `purged-`/`erased-`‖`replace(gen_random_uuid()::text,'-','')` (random, never derived from the bundle id; prefixes reserved by CHECK `bundle_label_reserved`), `owner_id/bound_at` := NULL, lines `applied_version` := NULL, new column `purged_at`. Its link is deleted. `claims.meta_intake`, `claims.links`, `social.*` rows are deleted. `claims.events/lines`, offers, windows stay. | Keeps FK graph, merchant claim statistics and dedup on `claims.events.source_event_id`; random (not NULL) key keeps 0060 NOT NULL/UNIQUE and guarantees a re-derived key of the same person never matches again. |
| RD2 | **Policy = one DB row** `claims.retention_policy` with defaults and CHECK bounds; `enforced=false` at migration. Unenforced runs are **report-only** (count, write nothing but the run log). | customers-billing Q8 "no automatic purge until periods set"; one audited source both the job and the CLI read. |
| RD3 | **River periodic job `claims_retention_v1`** hosted by `cmd/claims-worker` (IR-1), hourly + on start, one job per hour (unique by period); work = `claims.run_retention(500)` in its own pool, ≤20 batches per job, each batch its own tx. | Task requirement; no second scheduler; each batch is atomic, a crash loses nothing. |
| RD4 | **Actor-level deletion = operator CLI `cmd/retention-admin erase`**, one synchronous tx per request through one definer. Three selectors all resolve to one actor key: (a) Meta sender id as delivered in webhooks (+ object, asset, app ids); (b) a comment id of that person (via `claims.meta_intake`); (c) a bundle id given by the store (manual bundle = that bundle only). | PDPA 30-day erasure answer (F-P1) is met immediately; no request queue (CD6 precedent). Selector (b)/(c) work without solving U-D1. |
| RD5 | **8-day hold** (IR-6): erasure of a Meta actor is refused with `retry_after` (nothing written) while any affected window is OPEN, any intake of the actor is PENDING or received < 8 days ago, or any of its reply operations is non-terminal. Retention `intake_days` ≥ 8 for the same reason. | Deleting `meta_intake` removes per-comment dedup; inside Meta's 7-day private-reply window (F1) a re-delivered comment could re-claim and re-reply. After 8 days a re-staged comment cannot be replied to and is DROPPED/duplicate-safe. |
| RD6 | **Audit = `claims.retention_log`**, platform-level, append-only, counts only (CHECK: every JSON value is a number); erasure rows keep `actor_digest = sha256(actor_key)` as the restore-replay tombstone (§21.4, CD8 analogue). | No PII in audit; replay needs a handle that matches only the erased actor. |
| RD7 | **Secrets and selectors never in argv/logs**: sender id / comment id come from stdin JSON; K_actor is read from env only for selector (a); stdout = request id + numeric counts. | `ps`/shell history; claims §8, MCI §8. |
| RD8 | **A purged bundle can never get a link again**: trigger on `claims.links` raises PT404 for a bundle with `purged_at`. | A merchant `issue_link` on a de-identified bundle would re-open a binding path (claims R4). |

Rejected: DELETE whole bundles (cascades into append-only `claims.events`, loses statistics); NULL
`actor_key` (breaks 0060 NOT NULL + ingest upsert); rotating K_actor (splits every live bundle, erases nobody
— CD7); an async deletion-request queue (no deadline risk when done synchronously); a Meta deletion
callback in v1 (U-D1 unresolved; Q9 instructions page); storing `peer_key` or `comment_ref` in the tombstone
(linkable pseudonyms kept forever); env-only periods (no audit, job and CLI could disagree); redacting
non-terminal / UNKNOWN reply operations (reconcile still needs `comment_ref`, G07).

## 1. Retention classes and defaults (OQ1)

| Class | Rows | Eligible when (policy value = default) | Action |
| --- | --- | --- | --- |
| C1 links | `claims.links` | `expires_at < now() - link_days` (**7**) | DELETE |
| C2 claim identity | `claims.bundles` (actor_key, label, owner_id, bound_at) + lines.applied_version + the bundle's link | window of the bundle's session is CLOSED and `closed_at < now() - claims_days` (**90**), `purged_at IS NULL` | RD1 de-identify, label `purged-<32 random hex>` |
| C3 intake | `claims.meta_intake` (actor_key, comment_ref) | `state IN ('APPLIED','DROPPED','FAILED') AND received_at < now() - intake_days` (**30**, min 8) | DELETE |
| C4 reply ledger | `integration.operations` action `meta.private_reply` | state ∈ SUCCEEDED, FAILED_FINAL, CANCELLED, BLOCKED_POLICY, STALE_BINDING, `created_at < now() - intake_days`, `request ? 'comment_ref'` | `request := request - 'comment_ref' || '{"redacted":true}'`, `semantic_key := 'mpr-purged:'||id` (`request_hash` kept: hash of the original) |
| C5 social | `social.comment_events`, `social.messages` | `received_at < now() - social_days` (**30**, min 8, ≤ `intake_days`) AND `meta_inbox.lock_purgeable(event_id)` (the `meta_inbox.purgeable` predicate: event terminal, its River job terminal or pruned; holds the job row through the delete, 0028 pattern) | DELETE |
| C5b | `social.conversations` | no messages left and `created_at < now() - social_days` | DELETE |
| C6 run log | `claims.retention_log` kind `run` | `created_at < now() - 400 days` | DELETE (erasure/policy rows kept) |

Not purged here (stated, not forgotten): `meta_inbox.events` metadata (hashes/ids, no sender; inbox
contract), `meta_private.*` bodies (0028 already), `live.claim_sources.source_object_id` (merchant post id),
`claims.events`, operation `provider_reference` (Meta message id), `ops.command_results` receipts (keyed
`label_mac` only). Orders/snapshots: legal retention (customers-billing §9).

## 2. Data model — `0071_claims_retention.sql`

Preconditions (raise `55000` otherwise): 0029, 0060, 0064 applied (`to_regclass` of `social.comment_events`,
`claims.links`, `claims.meta_intake`, `live.claim_window_intervals`).

```sql
CREATE ROLE commerce_retention_writer   NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION; -- owns definers
CREATE ROLE commerce_retention_job      NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION; -- claims-worker pool
CREATE ROLE commerce_retention_operator NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION; -- retention-admin only

ALTER TABLE claims.bundles ADD COLUMN purged_at timestamptz;
ALTER TABLE claims.bundles ADD CONSTRAINT bundle_purged_unbound CHECK (purged_at IS NULL OR (owner_id IS NULL AND bound_at IS NULL));
-- RD1: `purged-`/`erased-`+32 hex is reserved for de-identified bundles; a merchant cannot pre-occupy it (§13 F1).
ALTER TABLE claims.bundles ADD CONSTRAINT bundle_label_reserved
 CHECK (purged_at IS NOT NULL OR label IS NULL OR label !~ '^(purged|erased)-[0-9a-f]{32}$') NOT VALID;
-- then, in a DO block: an existing row violating it → RAISE SQLSTATE '55000' (operator renames it; no auto-relabel
-- of merchant data); else ALTER TABLE claims.bundles VALIDATE CONSTRAINT bundle_label_reserved;
CREATE INDEX claims_bundle_actor ON claims.bundles(actor_key) WHERE platform<>'manual' AND purged_at IS NULL;
CREATE INDEX claims_bundle_purge ON claims.bundles(tenant_id,store_id,session_id) WHERE purged_at IS NULL;
CREATE INDEX meta_intake_actor ON claims.meta_intake(actor_key);
CREATE INDEX meta_intake_retention ON claims.meta_intake(received_at) WHERE state<>'PENDING';
CREATE INDEX claims_link_expiry ON claims.links(expires_at);
CREATE INDEX social_comment_retention ON social.comment_events(received_at);
CREATE INDEX social_message_retention ON social.messages(received_at);

CREATE TABLE claims.retention_policy (                 -- exactly one row
 id boolean PRIMARY KEY DEFAULT true CHECK (id),
 enforced boolean NOT NULL DEFAULT false,              -- false = report-only (Q8)
 link_days   integer NOT NULL DEFAULT 7  CHECK (link_days   BETWEEN 1 AND 365),
 intake_days integer NOT NULL DEFAULT 30 CHECK (intake_days BETWEEN 8 AND 3650),   -- RD5
 claims_days integer NOT NULL DEFAULT 90 CHECK (claims_days BETWEEN 8 AND 3650),
 social_days integer NOT NULL DEFAULT 30 CHECK (social_days BETWEEN 8 AND 3650),
 CHECK (social_days <= intake_days),   -- comment_events are reachable by erasure only via intake rows (§13 F4)
 version bigint NOT NULL DEFAULT 1 CHECK (version>0),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_by name NOT NULL DEFAULT session_user);
INSERT INTO claims.retention_policy DEFAULT VALUES;

CREATE TABLE claims.retention_log (                    -- platform-level, append-only except C6; never a person's id
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 kind text NOT NULL CHECK (kind IN ('run','policy_set','actor_erased','replay')),
 request_id uuid,                                       -- actor_erased: operator ticket id = confirmation code
 selector_digest bytea CHECK (octet_length(selector_digest)=32),
 actor_digest bytea CHECK (octet_length(actor_digest)=32),   -- sha256(convert_to(actor_key,'UTF8')); replay handle
 bundle_tenant uuid, bundle_store uuid, bundle_ref uuid,     -- manual-bundle erasure only
 counts jsonb NOT NULL CHECK (jsonb_typeof(counts)='object' AND octet_length(counts::text)<=1024
   AND NOT jsonb_path_exists(counts,'$.* ? (@.type() != "number")')),
 executed_by name NOT NULL DEFAULT session_user,        -- DB login, not a person
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK ((kind='actor_erased') = (request_id IS NOT NULL AND selector_digest IS NOT NULL)),
 CHECK (kind='actor_erased' OR (actor_digest IS NULL AND bundle_ref IS NULL)),
 CHECK (kind<>'actor_erased' OR ((actor_digest IS NOT NULL) <> (bundle_ref IS NOT NULL))),
 CHECK ((bundle_ref IS NULL) = (bundle_tenant IS NULL) AND (bundle_ref IS NULL) = (bundle_store IS NULL)));
CREATE UNIQUE INDEX retention_one_request ON claims.retention_log(request_id) WHERE kind='actor_erased';
CREATE INDEX retention_recent_runs ON claims.retention_log(created_at DESC) WHERE kind='run';
```

Both new tables: ENABLE + FORCE RLS, `REVOKE ALL FROM PUBLIC`, `COMMENT ON` every table/column/function/role
(PROCESS §5: owning package `internal/retention`, allowed roles, non-goals).

Trigger (RD8): `claims.links_not_purged()` BEFORE INSERT OR UPDATE ON `claims.links` FOR EACH ROW, SECURITY
DEFINER owner `commerce_retention_writer`: `purged_at IS NOT NULL` for `(NEW.tenant_id,NEW.store_id,
NEW.bundle_id)` → `RAISE SQLSTATE 'PT404'` (issue_link maps PT404 → not found, claims §3.3).

## 3. Definers

All: owner `commerce_retention_writer`, `SECURITY DEFINER`, `SET search_path=pg_catalog`,
`SET lock_timeout='2s'` (IR-5), fully qualified names, `REVOKE ALL FROM PUBLIC`, `COMMENT ON FUNCTION`
naming `internal/retention` and the caller roles. Platform-level (no scope GUC; they read none): every row
touched is chosen by the rules below, never by a caller-supplied tenant except selector (c). Global lock
order: advisory `hashtextextended('claims-retention',0)` (the only key form; both lock calls use it) → `live.claim_windows` FOR SHARE → `claims.bundles` FOR NO KEY UPDATE →
`claims.lines` → `claims.links` → `claims.meta_intake` → `integration.operations` → `social.conversations`
→ `social.messages`/`comment_events` → `claims.retention_log` (same relative order as claims §4.3 and
MCI §9; the job uses SKIP LOCKED on every row lock, so it never waits on a row).

| Function | EXECUTE to | Behavior |
| --- | --- | --- |
| `claims.run_retention(p_limit integer) → jsonb` | job, operator | `p_limit` 1..1000 else 22023. `pg_try_advisory_xact_lock(hashtextextended('claims-retention',0))` false → `{"busy":1}`, no write. Read policy. Per class C1→C2→C3→C4→C5→C5b→C6, oldest first, ≤ `p_limit` rows each, SKIP LOCKED. C2 locks the window `FOR SHARE SKIP LOCKED` and rechecks `state='CLOSED' AND closed_at < now()-claims_days` under the lock (a reopen in between skips it). Not enforced → counts eligible rows (capped at `p_limit`) and writes nothing but the log row. Inserts one `run` row `{enforced,links,bundles,intake,operations,comment_events,messages,conversations,more}` (`more`=1 when any class hit `p_limit`). Returns it. |
| `claims.erase_actor(p_request uuid, p_object text, p_asset text, p_actor_key text, p_comment_ref text, p_tenant uuid, p_store uuid, p_bundle uuid, p_peer_keys text[]) → jsonb` | operator | Exactly one selector: (a) object+asset+`p_actor_key ^[0-9a-f]{64}$` [+ `p_peer_keys` 0..8 × `^[0-9a-f]{64}$`]; (b) object+asset+`p_comment_ref ^[0-9_]{1,80}$`; (c) tenant+store+bundle; else 22023. Blocking `pg_advisory_xact_lock(hashtextextended('claims-retention',0))` (lock_timeout → 55P03). `selector_digest` = sha256 of the canonical selector (`a|object|asset|sha256(key)`, `b|object|asset|sha256(comment_ref)`, `c|bundle`). Request exists: same digest → stored counts + `replayed:1`; else `PT409`. Resolve: (b) intake row → its `actor_key`, none → PT404; (c) bundle row, none → PT404, manual → single-bundle mode, else its `actor_key`. (a)/(b)/(c-meta): bundles `platform=<object→platform> AND actor_key=k AND purged_at IS NULL` (any tenant/store: the key embeds object+asset). **Hold (RD5)** → returns `{"held":1,"retry_after":<unix s>}`, writes nothing. Else `claims.apply_actor_erasure(...)`, then `actor_erased` log row. Nothing found in any table → PT404. |
| `claims.apply_actor_erasure(p_platform text, p_actor_key text, p_tenant uuid, p_store uuid, p_bundle uuid, p_peer_keys text[]) → jsonb` | none (internal) | Idempotent. Lock windows FOR SHARE, bundles FOR NO KEY UPDATE; RD1 on each bundle with label `erased-<32 random hex>` for manual; DELETE their links; social: DELETE `social.comment_events` whose `comment_key` is in the keys of rows with `event_id` = the actor's intake `inbox_event_id`s (add/edit/remove of those comments); DELETE the actor's `claims.meta_intake`; C4 redaction of terminal `meta.private_reply` operations with `request->>'bundle_id'` in the bundles; DELETE `social.messages` then `social.conversations` whose `peer_key = ANY(p_peer_keys)`. Returns counts. |
| `claims.set_retention_policy(p_expected_version bigint, p_enforced boolean, p_link int, p_intake int, p_claims int, p_social int) → bigint` | operator | Row lock, CAS on `version` (PT409), NULL/out-of-range or `social_days > intake_days` → 22023 (the CHECKs are the bounds), `version+1`, `updated_by=session_user`; `policy_set` log row with all six values as numbers. |
| `claims.retention_status() → jsonb` | job, operator | STABLE. `{enforced, version, link_days, intake_days, claims_days, social_days, last_run_unix, last_run_more}` from policy + latest `run` row. |
| `claims.replay_actor_erasures(p_tombstones claims.erasure_tombstone[] DEFAULT NULL) → integer` | operator | Restore replay (§21.4). Composite `claims.erasure_tombstone(request_id uuid, selector_digest bytea, actor_digest bytea, bundle_tenant uuid, bundle_store uuid, bundle_ref uuid)` = the full `actor_erased` tuple (T20 exports whole tuples, never bare digests). NULL → every `actor_erased` log row; else each element is first inserted as `kind='actor_erased'` (counts `{}`) `ON CONFLICT (request_id) WHERE kind='actor_erased' DO NOTHING` (the §2 CHECKs apply), then applied; one `kind='replay'` row records the counts. For a digest, finds `actor_key`s where `sha256(convert_to(actor_key,'UTF8'))` matches in bundles/intake and runs `apply_actor_erasure` **without the RD5 hold** (services are not yet reopened). Peer-key social deletions are not replayable (keys not stored; C5 covers them within `social_days`). Social rows failing `meta_inbox.lock_purgeable` are skipped and counted `social_deferred` (C5 removes them later). |

Hold rule (RD5), evaluated under the locks: any affected bundle's window `state='OPEN'`; or any intake with
the actor key `state='PENDING'` or `received_at > now() - interval '8 days'`; or any `meta.private_reply`
operation of an affected bundle not terminal; or any social row to delete whose `event_id` fails
`meta_inbox.lock_purgeable` (a retrying consumer job would hit `social_terminal` XX000, §13 F5). `retry_after` = max(latest intake `received_at` + 8 d, now()
+ 1 h when a window is open or an operation is non-terminal).

## 4. Privileges (exact; CRP02 compares for equality)

"lock-only" = `UPDATE(<col>)` with a `FOR UPDATE` policy `USING (true) WITH CHECK (false)` (MCI §4.3
pattern: PostgreSQL needs an UPDATE privilege for `FOR SHARE/UPDATE`; any real update fails). Every policy
below is `TO commerce_retention_writer` only; bodies of the §3 definers are the control
(`commerce_retention_writer` has no login).

| Role | Object | Privilege | RLS policy |
| --- | --- | --- | --- |
| commerce_retention_writer | claims.bundles | SELECT(tenant_id,store_id,id,session_id,platform,actor_key,label,owner_id,purged_at); UPDATE(actor_key,label,owner_id,bound_at,purged_at,updated_at) | read `true`; update USING `purged_at IS NULL` WITH CHECK `purged_at IS NOT NULL AND owner_id IS NULL` |
| commerce_retention_writer | claims.lines | SELECT(tenant_id,store_id,bundle_id); UPDATE(applied_version) | read `true`; update WITH CHECK `applied_version IS NULL` |
| commerce_retention_writer | claims.links | SELECT(tenant_id,store_id,bundle_id,expires_at); DELETE; lock-only UPDATE(expires_at) | read/delete `true` |
| commerce_retention_writer | claims.meta_intake | SELECT(tenant_id,store_id,id,inbox_event_id,session_id,object,asset_id,comment_ref,actor_key,state,received_at); DELETE; lock-only UPDATE(updated_at) | read `true`; delete USING `state<>'PENDING'` |
| commerce_retention_writer | live.claim_windows | SELECT(tenant_id,store_id,session_id,state,closed_at); lock-only UPDATE(updated_at) | read `true` |
| commerce_retention_writer | integration.operations | SELECT(id,tenant_id,store_id,action,state,request,created_at); UPDATE(request,semantic_key,updated_at) | read USING `action='meta.private_reply'`; update USING `action='meta.private_reply' AND state IN (terminal five)` WITH CHECK same `AND NOT request ? 'comment_ref' AND semantic_key LIKE 'mpr-%'` |
| commerce_retention_writer | social.conversations, social.messages, social.comment_events | SELECT; DELETE; lock-only UPDATE(next_seq) / UPDATE(received_at) / UPDATE(received_at) | read/delete `true` |
| commerce_retention_writer | claims.retention_policy | SELECT; UPDATE(enforced,link_days,intake_days,claims_days,social_days,version,updated_at,updated_by) | `true` |
| commerce_retention_writer | claims.retention_log | SELECT, INSERT, DELETE | read/insert `true`; delete USING `kind='run'` |
| commerce_retention_writer | schemas | USAGE `claims`, `live`, `integration`, `social`, `meta_inbox` | — |
| commerce_retention_writer | functions | EXECUTE `meta_inbox.lock_purgeable(uuid)` only (definer; the role has no login, so 0028's "no execution grant for any login role" still holds) | — |
| commerce_retention_job | claims schema; functions | USAGE `claims`; EXECUTE `run_retention(integer)`, `retention_status()` only | — |
| commerce_retention_operator | claims schema; functions | USAGE `claims`; EXECUTE `run_retention`, `retention_status`, `erase_actor`, `set_retention_policy`, `replay_actor_erasures` | — |

No privilege for PUBLIC, `commerce_runtime`, `commerce_buyer_runtime`, `commerce_claims_intake`,
`commerce_claims_writer`, any `commerce_meta_*`, `commerce_integration_writer`, `commerce_worker`,
`commerce_privacy_writer` on the two new tables or the new functions. `apply_actor_erasure` and
`links_not_purged` have no EXECUTE grant. Logins: exactly one dedicated login per role
(`lc_retention_job`, `lc_retention_operator`), validated by new `platform.ValidateRetentionJobPool` /
`ValidateRetentionOperatorPool` modelled on `ValidateClaimsIntakePool` (mixed membership, SET ROLE, owner
reachability rejected both ways); `validatePoolAuthority` rejects any runtime login that can reach
`commerce_retention_writer`, `_job` or `_operator`. Meta `require_authority` (0029) stays true: no
`commerce_meta_*` login is a member of a retention role.

## 5. Go surface

- `internal/retention` (commerce_worker): package doc per PROCESS §5. `JobArgs{}` kind
  `claims_retention_v1`; `Worker.Work` runs `claims.run_retention(500)` on the retention-job pool, one tx per
  batch, until `more=0`, `busy=1` or 20 batches; logs one fixed line with the numeric counts. Operator calls:
  `Erase(ctx, pool, Selector) (Counts, Held, error)`, `SetPolicy`, `Status`, `Replay`. `Selector` and
  `Counts` implement redacted `String/GoString/Format/MarshalJSON` (no sender id, comment id, actor key).
  Error mapping: 22023 → usage, PT404 → not_found, PT409 → conflict, 55P03 → busy.
- `cmd/claims-worker` (integration_worker): new env `COMMERCE_RETENTION_JOB_DATABASE_URL` (required when the
  worker is enabled; same-database check as the existing pools); registers
  `river.NewPeriodicJob(river.PeriodicInterval(time.Hour), ctor, &river.PeriodicJobOpts{RunOnStart: true})`
  with `InsertOpts{UniqueOpts: river.UniqueOpts{ByPeriod: time.Hour}}` on its existing main-`river` client
  (inserted by `commerce_worker`, IR-4) and adds the worker.
- `cmd/retention-admin` (commerce_worker), operator-only, no service starts it (meta-admin pattern). Env
  `COMMERCE_RETENTION_OPERATOR_DATABASE_URL`; `status` also accepts `COMMERCE_RETENTION_JOB_DATABASE_URL`
  (validated by `ValidateRetentionJobPool`) and is the only subcommand that does; every other subcommand
  refuses the job DSN (usage exit 2); `erase` with a sender id also `COMMERCE_CLAIMS_ACTOR_KEY`
  (IR-2). Subcommands: `status`; `policy-set --expected-version --enforced --link-days --intake-days
  --claims-days --social-days`; `run --limit` (manual batch); `erase --request <uuid> [--object --asset]
  [--app <id> …] [--tenant --store --bundle]` with **stdin** one strict JSON object `{"sender_id":"…"}` or
  `{"comment_ref":"…"}` (unknown fields, both, or trailing data rejected; empty stdin allowed only with
  --bundle); `replay [--tombstones-file]` (JSON array of full tombstone tuples, strict). Selector (a): Go derives `actor_key =
  meta.ClaimActorKey(K, object, asset, sender)` and one peer key per `--app` via new exported
  `meta.SocialPeerKey(app, object, asset, sender)` (wraps the existing `tupleHash`; integration_worker).
  Output: request id + numeric counts only. Exit codes: 0 done/replayed, 2 usage, 3 held (prints
  `retry_after` RFC 3339), 4 not_found, 5 conflict/busy, 1 other (one fixed stderr code, never a driver
  message, DSN, key or selector).
- Runbook `docs/runbooks/claims-data-deletion.md` (integrator): intake via the `/data-deletion`
  instructions page (customers-billing Q9); identity verification (OQ3); owner approval per production
  erasure (OQ2, AGENTS.md); reply to the requester with the request id as confirmation code within 30 days
  (F-P1); `status` check; restore procedure = `replay` before reopening marketing (§21.4).

## 6. Amendment clauses (recorded by the integrator at freeze, never by an implementer)

1. live-keyword-claims-v1 §3 "no DELETE grant" and §3.2 matrix (KC03 equality) gain the §4
   `commerce_retention_writer` rows and the `purged_at` column/CHECK/trigger; KC03 subtests unchanged
   otherwise.
2. live-keyword-claims-v1 §3.3 R4 sentence "no other function or grant can clear `owner_id`" gains: "except
   the U08 definers, which clear it only together with deleting the bundle's link and setting `purged_at`
   (RD8: no link can be issued afterwards)". KC03's "no function EXECUTE-able by `commerce_runtime` or
   `commerce_buyer_runtime` can clear `owner_id` without replacing `token_hash`" stays true unchanged.
3. live-keyword-claims-v1 §8/§11.4/§12 and meta-claims-intake-v1 §1/§8/§14 "production mount blocked by
   T14/U08": replaced by "lifted per claims-retention-purge-v1 §10".
4. meta-claims-intake-v1 §3 "`K_actor` … loaded only by the meta-worker consumer" gains "and by the operator
   CLI `cmd/retention-admin` (never a service)"; MCI10 source guard updated to that pair.
5. meta-claims-intake-v1 §4.3 (MCI02 equality) gains the §4 rows for `claims.meta_intake`,
   `integration.operations` and `live.claim_windows`; meta-consumer-v1 social grants gain the §4 social rows
   (MIso/MC gates must still pass); meta-inbox-v1 function grants gain EXECUTE `meta_inbox.lock_purgeable(uuid)`
   to the NOLOGIN `commerce_retention_writer` only.
6. external-operation-v1: a terminal `meta.private_reply` operation's `request.comment_ref` and
   `semantic_key` may be redacted by U08 only; `request_hash` stays the hash of the original request.
7. customers-billing-v1 §9 U08 bullets point here; CD7 unchanged (owner erasure still never touches actor
   data); note that C2 clears bindings after `claims_days`, so the CB03 projection loses old claims.

## 7. Gates (all NOT_RUN)

Real-PG tests in `tests/foundation/claims_retention_test.go`, one `TestClaimsRetentionCRPnn…` per gate;
pure tests in `internal/retention`, `cmd/retention-admin`. Every sentinel synthetic. Each gate records one
red run before green (PROCESS §2.4). Test author ≠ implementer.

| Gate | Evidence | Required |
| --- | --- | --- |
| CRP01 | UNIT | Selector parsing (exactly one; stdin strict JSON; argv never carries sender/comment id); `%v/%+v/%#v/json` of Selector/Counts/config emit no sentinel; `SocialPeerKey` equals the key `projection.go` writes and `ClaimActorKey` vectors unchanged; exit-code mapping; job loop stops on `more=0`/`busy`/20 batches; `status` runs on the job DSN, every other subcommand given only the job DSN → exit 2. |
| CRP02 | REAL_PG | 0071 fresh + populated-0066 upgrade, migrate twice; preconditions fail without 0064; role attributes; **§4 matrix equality** (column/table privileges, schema USAGE, EXECUTE, direct and inherited); FORCE RLS + policies; definer owner/`prosecdef`/`proconfig` (search_path, lock_timeout); PUBLIC/runtime/buyer/intake/meta/worker/privacy roles 42501 on new tables and functions; lock-only grants cannot change a value; pool validators reject mixed/SET ROLE/owner-reachable logins; KC03 and MCI02 pass with §6 clauses 1, 2, 5; `retention_log` rejects a string count and two selectors; `retention_policy` rejects `social_days > intake_days` (23514, `set_retention_policy` 22023); 0071 on a DB holding a non-purged `purged-<32 hex>` label → 55000. |
| CRP03 | REAL_PG | Report mode: `enforced=false`, one eligible row per class → counts equal expected, every purge-target table checksum identical before/after, exactly one `run` row. |
| CRP04 | REAL_PG | Enforced purge per class at threshold −1 s / +1 s (rows seeded with past timestamps); OPEN window, recently CLOSED window, reopened window, PENDING intake, non-terminal and UNKNOWN operations untouched; after C1/C2 old token preview/redeem → not found, `issue_link` on a purged bundle → PT404 and HTTP 404; same actor commenting in a new session → new bundle; C2 with a pre-existing merchant label `purged-<8 hex>` (outside the reserved pattern) in the session completes; `claims.events`/`lines.quantity` identical; second run changes nothing; `more=1` at `p_limit`. |
| CRP05 | REAL_PG | Concurrency (`pg_stat_activity` interleaves, no sleeps): run vs manual ingest, intake apply, issue_link, redeem, mark_applied, window reopen, consumer inserting a message into a conversation being purged → zero 40P01, no lost claim, message committed exactly once (consumer retry path); two runs → one `busy`; `erase_actor` blocks on a running `run_retention` (and vice versa `busy`) — both use `hashtextextended('claims-retention',0)` (asserted from `pg_locks.objid`); a held row lock → skipped, next run purges it; a social row whose inbox job is non-terminal is not deleted and its consumer retry returns `ALREADY`, not XX000. |
| CRP06 | REAL_PG | Erasure: selectors (a)/(b)/(c-meta)/(c-manual); one actor across two stores and sessions → all bundles, links, intake, social comment rows (incl. edit/remove of those comments), peer conversations/messages, operation redaction; another actor and another peer byte-identical; hold cases (OPEN window, PENDING intake, intake 7 d 23 h old, non-terminal op) → `held` + `retry_after`, nothing written; replay same request → same counts `replayed:1`; same request other selector → PT409; unknown → PT404; manual label `erased-`+32 random hex (≠ hex of the bundle id) with a pre-existing merchant label `erased-<8 hex>` in the session; a merchant INSERT/UPDATE with label `purged-<32 hex>` or `erased-<32 hex>` → 23514; a merchant label `purged-<hex of bundle X>` created while the window is OPEN cannot exist, and C2 + erasure of X complete. |
| CRP07 | REAL_PG | Privacy: DB-wide scan (every text/jsonb/bytea column incl. River args) + captured api/worker/CLI stdout/stderr find no sender-id, comment-id, label or Page-token sentinel after erasure/purge; the erased `actor_key` appears nowhere, its sha256 only in `retention_log.actor_digest`; no counts row contains a string. |
| CRP08 | REAL_PG (MODEL of restore until T20) | Dump before erasure, restore to a fresh DB: red = no replay → bundles keep the key; green = `replay` from log rows and from an external tombstone-tuple list (missing tombstone inserted as `actor_erased`, existing one not duplicated) → de-identified, `replay` row written, hold ignored. |
| CRP09 | MOCK (River) | claims-worker with retention DSN registers the periodic job; RunOnStart inserts one `claims_retention_v1` job; restart within the hour inserts none; job runs `run_retention` on the retention-job login (not `commerce_worker`); DB error → River retry, earlier batches committed; missing/invalid DSN or a login reaching another role → worker refuses to start with a fixed code; the deploy smoke config carries the job DSN only (no operator DSN). |
| CRP10 | REVIEW + regression | Source guards: only `internal/retention` calls the §3 definers; only `cmd/meta-worker` and `cmd/retention-admin` load `COMMERCE_CLAIMS_ACTOR_KEY`; `deploy/scripts/smoke.sh`, `deploy/**` and `.github/workflows/*` never reference `COMMERCE_RETENTION_OPERATOR_DATABASE_URL`; no network and no new dependency in retention code; KC01–15, MCI01–10, MC/MIso, CB05 (when 0078 present) unchanged; full `go test -race ./...`, `go vet ./...`, `python3 scripts/check_packet.py`; independent test_worker + security_reviewer verdict. |
| CRP11 | LIVE read-only (owner-approved) | U-D1: for an app-role test user, compare the app-scoped id (Graph `/me` with the app 4291253377792879) with the `from.id` of that user's FB and IG test comments captured by MCI11. Records whether selector (a) accepts callback ids. No send. |

Commands (record exit codes): `go test -count=1 ./internal/retention/... ./cmd/retention-admin/...
./cmd/claims-worker/...`; `bash scripts/dev/test-focused.sh '^TestClaimsRetentionCRP'`; full suite per
PROCESS §2.4.

## 8. Ownership and sequencing

| Artifact | Owner |
| --- | --- |
| `migrations/0071_claims_retention.sql` | commerce_worker; integrator merges |
| `internal/retention/**`, `cmd/retention-admin/**` | commerce_worker |
| `cmd/claims-worker` wiring, `meta.SocialPeerKey` | integration_worker |
| `internal/platform/retention.go`, `validatePoolAuthority` role list, deploy smoke step, runbook, §6 clauses | integrator |
| `tests/foundation/claims_retention_test.go`, package tests | test_worker |
| Review | security_reviewer (definers, grants, stdin/log handling) |

Order: 0071 + definers → CRP02–CRP08 → Go job/CLI → CRP01, CRP09 → CRP10 → runbook + smoke. Depends on
0060/0064 only; independent of 0070/0072–0079.

## 9. Known limits / NOT_RUN

- All gates NOT_RUN; this file is DESIGN. No performance claim: one job = ≤ 20 × 500 rows per class per hour
  (ponytail ceiling: raise batches or cadence when `last_run_more=1` persists > 24 h, visible in `status`).
- Comments on unbound objects, reply comments and DMs of a person whose sender id is unknown can be found only
  by age (C5); actor erasure covers them only through selector (a) with the right `--app` ids (peer keys) or
  through intake rows.
- U-D1 unresolved: a Meta callback app-scoped id is usable in selector (a) only if CRP11 shows equality; no
  callback endpoint in v1 (Q9).
- A new comment after erasure re-derives the same deterministic `actor_key` (new data, new bundle); an
  UNKNOWN reply operation blocks that actor's erasure until resolved; operation `request_hash` and
  `provider_reference` (message id) are kept as ledger evidence.
- Purged bundles stay visible to the merchant with `purged-`/`erased-` labels and no link; `live:manage`
  cannot re-issue a link (404). Retention clock of C2 = last window close of the session.
- Peer-key social deletions are not replayable after a restore (C5 removes them within `social_days`);
  a copy of the erasure tombstones outside PG backups is T20.
- The job lives in `cmd/claims-worker`; a deployment without that worker never purges — the §10 smoke check
  makes that a failed deploy once claims are mounted.

## 10. Lifting the production-mount blocker

The blocker of claims §8/§11.4 and MCI §1/§8/§14 is lifted, and waiver W1 lapses, only when all hold:
(1) 0071 applied; (2) CRP01–CRP10 PASS with evidence under `output/u08-claims-retention/`; (3)
`retention-admin policy-set --enforced=true` executed on production with the OQ1 periods, after owner
approval in chat (AGENTS.md: deleting user data); (4) claims-worker running with the retention-job login;
(5) the deploy smoke step runs `retention-admin status` with the retention-job login (never the operator login)
and asserts `enforced=1` and `now - last_run_unix < 26 h`; the operator DSN is never present on the deploy host
or in CI;
(6) the runbook exists. Must hold before a second merchant is onboarded (X4).

## 11. Owner questions (each has a default; none blocks implementation)

- **OQ1 Retention periods** (MCI O7, customers-billing Q8). Default: links 7 d after expiry, intake +
  reply-ledger comment ids 30 d, claim identity (actor key, label, binding) 90 d after the session's claim
  window closed, social comment/message ciphertext 30 d; enforced at the latest when the second merchant is
  onboarded. Taiwan legal retention for these classes is UNKNOWN (no legal review); the defaults are short,
  purpose-bound periods (PDPA Art 11, F-P1).
- **OQ2 Production erasure approval.** Default: each production `erase` needs your approval in chat
  (AGENTS.md). Alternative: a standing approval for requests verified per OQ3.
- **OQ3 Requester verification.** Default: through the store — the merchant confirms the person in their own
  Messenger/IG thread and passes the bundle id (selector c), or the requester supplies a link to their own
  comment (selector b); the operator never accepts a bare name.

## 12. Integrator rulings requested

- IR-1 Host the periodic job in `cmd/claims-worker` (claims domain host) rather than `cmd/expiry-worker`.
- IR-2 `cmd/retention-admin` may load `COMMERCE_CLAIMS_ACTOR_KEY` (clause 4).
- IR-3 Terminal reply-operation redaction (clause 6).
- IR-4 No post-River file: `commerce_worker` already inserts main-`river` jobs (migrate.go:132-135); the
  job's DB work runs on the separate retention-job login.
- IR-5 `SET lock_timeout='2s'` in definer `proconfig` (new; existing definers set only `search_path`).
- IR-6 8-day hold = Meta 7-day private-reply window (MCI F1) + 1 day for webhook redelivery.
- IR-7 Gate prefix `CRP` (no collision in contracts/ or docs/, grep 2026-09-30).

## 13. Review findings ledger (adversarial round 1, 2026-09-30; applied at freeze)

Each finding was verified against the repo before applying; none rebutted.

| # | Sev | Finding | Verified against | Resolution |
| --- | --- | --- | --- | --- |
| F1 | P1 | Label `purged-`/`erased-`‖bundle-id hex is predictable; a merchant can pre-create it in the same session while OPEN → C2/erasure 23505 on the same oldest row every hour, retention stops for every tenant. | 0060:99 label is merchant text 1..60 chars, no prefix rule; 0060:112 `UNIQUE(tenant,store,session,label)`; 0060:255 runtime SELECT `id`. | RD1/C2/§3: random label from `gen_random_uuid()`; §2 `bundle_label_reserved` CHECK (NOT VALID + VALIDATE, 55000 on existing violators — operator renames, no auto-relabel of merchant data); CRP02/CRP04/CRP06 cases. The reviewer's "pre-existing `purged-`-looking label still completes" is tested with labels outside the reserved pattern (`<8 hex>`), since a reserved-pattern row blocks the migration itself. |
| F2 | P1 | Smoke `retention-admin status` needs the operator DSN on the deploy host/CI = standing authority to erase data and disable the purge. | §5 env (operator DSN only); §4 job role already has EXECUTE `retention_status()`. | §5: `status` alone accepts the job DSN; §10(5) job login only, operator DSN never on deploy host/CI; CRP01/CRP09/CRP10 guards. |
| F3 | P2 | Replay from an external digest/bundle list cannot insert a tombstone: `actor_erased` needs `request_id`+`selector_digest`, `replay` rows must have `actor_digest`/`bundle_ref` NULL. | §2 retention_log CHECKs (lines 123–125). | §3: `claims.erasure_tombstone[]` full tuples, inserted as `actor_erased` `ON CONFLICT DO NOTHING`, then applied; §5 `--tombstones-file`; CRP08. |
| F4 | P2 | `intake_days < social_days` leaves comment ciphertext unreachable by erasure (lookup via `meta_intake.inbox_event_id`). | §3 apply_actor_erasure social step. | §2 CHECK `social_days <= intake_days`; `set_retention_policy` 22023; CRP02. |
| F5 | P2 | Deleting social rows of a processed event whose River job is non-terminal makes `meta_inbox.social_terminal` raise XX000 on retry. | 0029:122-137 (count of social facts must be 1); 0028:372-392 `purgeable`/`lock_purgeable`. | C5 eligibility + RD5 hold + replay skip use `meta_inbox.lock_purgeable`; writer gets USAGE `meta_inbox` + EXECUTE on it only; §6 clause 5; CRP05. |
| F6 | P2 | Advisory key written two ways; `pg_advisory_xact_lock(text)` does not exist. | PostgreSQL has only `(bigint)` / `(int,int)` overloads. | One key `hashtextextended('claims-retention',0)` in §3; CRP05 asserts it. |

## 14. Amendments (integrator lane close, 2026-09-30, r2-close-retention)

Each row is the smallest change that makes the contract say what the reviewed and tested code must do; the
migration `0071` (unreleased, edited in place), CRP02/CRP03/CRP08 and the unit tests were changed in the same commit.

| # | Source | Amendment |
| --- | --- | --- |
| A1 | Test finding (CRP08 residue, P2) | §3 `replay_actor_erasures` ignores the RD5 hold **completely**: while it runs, a transaction-local flag `lc.retention_replay='on'` (set and reset by the definer) lets `apply_actor_erasure` delete the erased actor's PENDING `claims.meta_intake` rows and redact `comment_ref` of its non-terminal `meta.private_reply` operations (state unchanged; the adapter then fails pre-send with zero calls, so no reply is ever sent to an erased actor). §4 policies `intake_retention_delete` and `operation_retention_update` gain `OR current_setting('lc.retention_replay',true)='on'`. `erase_actor` resets the flag first (the hold applies; the flag is settable by any session but only writer-owned code evaluates the policies). `run_retention` never sets it and keeps its explicit state filters. |
| A2 | Review P2-2 | §3 `run_retention`: `more` is 1 only when `enforced` and some class hit `p_limit`. Report-only removes nothing, so its backlog never shrinks; `more=1` there made the job run 20 batches per hour and set `last_run_more=1` permanently. Counts stay capped at `p_limit`. CRP03 (capped report-only run) asserts `more=0`, one run row and `last_run_more=false`. |
| A3 | Review P2-1 | §5 job bound: `Worker.Timeout` = `retention.RescueWindow` (one minute, = `RescueStuckJobsAfter` of `cmd/claims-worker`) − 5 s; no new batch starts after 45 s; the remainder waits for the next hourly run (`more` stays 1, the job succeeds). A run can therefore never be rescued into a second concurrent runner. |
| A4 | Review P2-3 | §3 lock order sentence "the job uses SKIP LOCKED on every row lock, so it never waits on a row" is replaced by: the job chooses rows with SKIP LOCKED; C2's UPDATE of `claims.lines` and DELETE of `claims.links` of a bundle it already holds `FOR NO KEY UPDATE` do not use SKIP LOCKED, because every claims writer (`issue_link`, `redeem_link`, `mark_applied`, `issue_system_link`) locks the bundle first; a stuck row costs `55P03` after 2 s and a River retry. |
| A5 | Test ambiguity | §3 `erase_actor`: in `selector_digest` the inner `sha256(...)` is lower-case hex (as implemented). |
| A6 | Test finding (DDL looseness) | §2 `retention_log`: `request_id` and `selector_digest` are NULL unless `kind='actor_erased'` (two-sided CHECK), and the counts check uses a `strict` jsonpath so an array of numbers is rejected (lax mode unwrapped it). CRP02 asserts both. |
