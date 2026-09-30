# Meta claims intake v1 — T10c comment → keyword claim → first private reply

Status: **FROZEN 2026-09-29 (review rounds 1–3; last two P1 adjudicated by integrator)** (contract author, unit `design-meta-intake`; frozen by the integrator under PROCESS.md §2.2).
Round-1 adversarial review `output/contract-review/meta-intake-round1.json` (verdict BLOCK, 8 P1
/ 9 P2) and the round-2 fresh review (3 P1: privilege matrix not executable, per-app comment
keys, reply precondition destroying the claim) are answered in this revision; each finding was
checked against migrations/ and internal/ before amending. Round 3 left two P1 (River job
grants, §4.4 vs frozen claims §3.2/KC03); the integrator adjudicated both at freeze (§4.3 River
rows, §5.4, §4.4 clause 10) and ruled IR-12..IR-17 (§15). Implementation may start from this
file (PROCESS.md §2.2). Evidence label of this file: DESIGN.
Every gate in §12 is NOT_RUN.

Amends, without editing them (the amendment text lives here and is recorded by the
**integrator at freeze**, never by an implementer):
- `live-keyword-claims-v1.md` (FROZEN) — exact clause list in §4.4; §11.2 H1–H7 are the entry
  conditions this file closes.
- `meta-consumer-v1.md` (FROZEN/PASS MC01–07) — the "MC01 amendment" H4 asks for: step 5's
  "no cross-domain rows" gains exactly one sanctioned edge, `meta_inbox.stage_claim_intake`
  (§5.1). No extra River job is created by the consumer.
- `external-operation-v1.md` (accepted) — one system producer (`integration.plan_claim_reply`)
  and one INSERT policy for `commerce_integration_writer` (§4.3).
- `external-dispatcher-v1.md` (INTERNAL_DISPATCHER_ACCEPTED) — the per-route secret loader
  hook of §6.4; an "Amendment proposal" section pointing here is appended to that file for
  integrator approval.

Unchanged: `meta-runtime-isolation-v1.md` (the consumer never touches the main `river`
schema and gets no River grant; meta-worker keeps exactly its two pools), `meta-inbox-v1.md`,
`meta-webhook-protocol-v1.md`, `meta-runtime-v1.md`, `legacy-runtime-isolation-v1.md`.
`stripe-psp-v1.md` §0.2 is used only as the pattern for per-store credential custody and the
lease-fenced loader (§7); nothing here touches Stripe objects or migrations 0061–0063 /
post-River 0012–0013.

Migration numbers: **0064** (`0064_meta_claims_intake.sql`), post-River **0014**
(`post_river/0014_meta_claims_intake_river.sql`, main-`river` guards of §5.4 only).
Integrator-owned; renumbered at merge if needed.

## 0. Facts (retrieved 2026-09-28 from developers.facebook.com)

| # | Fact | Source |
| --- | --- | --- |
| F1 | Page private reply: `POST /{PAGE-ID}/messages`, body `recipient.comment_id`. Requires `pages_messaging` and a Page access token of a person with the `MESSAGING` task. "Only one message can be sent to the person who commented." Must be sent "within 7 days from when the post or comment was created." Further messages only after the person responds (24 h window). "Cannot send private reply message to another facebook page." Standard Access apps only reach people with a role on the app. | <https://developers.facebook.com/docs/messenger-platform/discovery/private-replies/> |
| F2 | IG private reply: `POST /<IG_ID>/messages`, body `recipient.comment_id` + `message.text`. Facebook Login host `graph.facebook.com` (Instagram Login host `graph.instagram.com`). Permissions listed: FB Login `instagram_manage_comments`, `pages_read_engagement`; IG Login `instagram_business_manage_comments`. "within 7 days of the comment"; Live: "private replies can only be sent during the live broadcast"; "Only one message can be sent to the commenter". The page does **not** mention `instagram_manage_messages`. | <https://developers.facebook.com/docs/instagram-platform/private-replies/> |
| F3 | Page `feed` webhook, `item=comment`: value fields `from{id,name}`, `post_id`, `comment_id`, `parent_id`, `message`, `created_time`, `verb`, `post{status_type,…}`. Verb list includes `add`, `edit`, `edited`, `remove`, … . Feed webhooks require `pages_manage_metadata` and `pages_show_list`; the Page must install the app via `POST /{page-id}/subscribed_apps`. | <https://developers.facebook.com/docs/graph-api/webhooks/reference/page/>, <https://developers.facebook.com/docs/graph-api/webhooks/getting-started/webhooks-for-pages/> |
| F4 | IG `comments` / `live_comments` value: `id`, `text`, `from{id,username,self_ig_scoped_id}`, `media{id,media_product_type}`, `parent_id`, `ad_id`, `ad_title`, `original_media_id`. FB-Login permissions: `instagram_basic`, `instagram_manage_comments`, `pages_manage_metadata`, `pages_read_engagement`, `pages_show_list`. "Advanced Access is required to receive `comments` and `live_comments` webhook notifications." Account must be public. "Notifications for Comments on Live media are only sent during the live broadcast." | <https://developers.facebook.com/docs/graph-api/webhooks/reference/instagram/>, <https://developers.facebook.com/docs/instagram-platform/webhooks/> |

UNKNOWN (not retrievable or not stated; never assumed): U1 whether Facebook **live video**
comments arrive through the Page `feed` webhook and which `post_id` form they carry (live-video
comments guide fetch timed out); U2 whether IG private replies additionally need
`instagram_manage_messages` (F2 does not list it; the brief expected it); U3 the Graph error
code for "already replied / window closed"; U4 whether any read field (e.g. comment
`can_reply_privately`) proves a private reply was already sent; U5 current Graph API version to
pin; U6 whether Graph accepts the Page token in an `Authorization: Bearer` header. Each has a
LIVE probe in §12; until closed, the affected branch fails closed.
U7 (documented limit, not a probe): the IG comment value carries no creation time, so
`normalize.go` uses `entry.time` (delivery time) as `occurred_at`; every IG window and
deadline decision is therefore delivery-time based (MCI06 case).
U8 whether `from.id` is app-scoped (§3); relevant only when two apps route one asset.

Observation for T07 (not changed here): F3 lists verb `edited`, while `normalize.go` accepts
only `add|edit|remove`; an `edited` delivery is quarantined. Harmless for claims (only `add`
qualifies) but a T07 fixture should record it.

Code facts this revision relies on (checked 2026-09-29):
- `protocol.go` `emit()` keys comment events on `("meta-event-v1", app, object, asset, kind,
  externalID, payload_hash)`; `social.comment_events.comment_key` is non-unique (0029). Inbox
  dedup is therefore byte-level, not per comment → §4 adds a per-comment unique key.
- Meta route bindings use `provider='facebook'` (page) / `'instagram'` (0028 `register_route`);
  the dispatcher gate joins `b.provider=o.provider` and reads only `actor_kind='MERCHANT'`
  (`internal/integrations/core/dispatcher.go` `finalDispatchGate`).
- No `cmd/*` hosts `core.NewDispatcher` today; §5.3 adds the host.
- `DispatchRoute` callbacks get no lease token and no DB handle (external-dispatcher-v1
  "Frozen Go surface"); a non-`ErrPolicyDenied` Check error becomes UNKNOWN
  `policy_check_failed` (dispatcher.go, `runOperation`), never a retry of Check.
- No store default locale exists in any migration (grep `locale` in migrations/ hits only
  payment tables); storefront locales are `zh-TW`, `zh-CN`, `en` (`locale-routing-v1.md`).

## 1. Scope

In: FB Page feed comment `add` and IG `comments`/`live_comments` on an object bound to a live
session → text-free staged intake → `claims` ingest (source_kind `meta`) → optionally one
automated private reply carrying the claim link, through the external-operation ledger.

Out (unchanged NOT_RUN elsewhere): OAuth / Facebook Login issuer and token refresh (T07), social
read UI, human takeover UI, DM conversations after the reply, public comment replies, comment
snapshot reconciler, marketing, consent, identity edges (arch §14.1), inventory holds (arch
§11.2), T14 purge/DSAR. The production-mount blocker of claims §8 stays in force and now also
covers `claims.meta_intake` and Meta actor keys.

## 2. Session ↔ source binding (closes H1)

`live.claim_sources` (0064). commerce_runtime reads it (`S`) and writes it only through
`live.put_claim_source(p_session uuid, p_object text, p_asset text, p_object_id text,
p_private_reply boolean, p_locale text, p_active boolean, p_expected_version bigint) RETURNS
uuid` (SECURITY DEFINER, owner `commerce_claims_writer`, EXECUTE `commerce_runtime` only),
called inside `command.Run` (receipt + audit written by Go as commerce_runtime, as today). The
definer requires a merchant transaction (`M`, §4.3), `identity.principal_holds(…,
ARRAY['live:manage','integration:execute'])`, CAS on `version`, and sets `principal_id` =
`app.principal_id`:

```
tenant_id, store_id, id uuid, session_id uuid → live.claim_windows,
platform text CHECK IN ('facebook','instagram'),
binding_id uuid, binding_version bigint → integration.bindings, where
  bindings.provider = CASE object WHEN 'page' THEN 'facebook' ELSE 'instagram' END
  AND bindings.external_asset_id = asset_id AND enabled (at create)
object text CHECK IN ('page','instagram'), asset_id text ^[0-9]{1,40}$  -- = route asset
source_object_id text ^[0-9_]{1,80}$     -- FB: feed post_id as delivered; IG: media.id
private_reply boolean NOT NULL DEFAULT false,
reply_locale text NOT NULL DEFAULT 'zh-TW' CHECK IN ('zh-TW','zh-CN','en'),  -- locale-routing-v1 labels
intake_count bigint NOT NULL DEFAULT 0, intake_capped bigint NOT NULL DEFAULT 0,  -- §4.2
active boolean, version bigint, principal_id uuid (last changer), created_at, updated_at
UNIQUE (object, asset_id, source_object_id) WHERE active   -- one live object → one session, globally
CHECK (platform='facebook') = (object='page')
```

- `(object, asset_id)` must equal an enabled `meta_inbox.routes` row of the same tenant/store
  (checked in `put_claim_source`; mismatch → `ErrConflict`). An object never maps to two sessions.
- Unbound objects never claim (no intake row). Deactivation is the only removal.
- FB live video mapping depends on U1: until the LIVE probe records the delivered `post_id`
  form for a live video, a `facebook` source is MOCK-only and the UI labels it "unverified".
- `private_reply=true` requires the binding to hold a current Page-token credential (§7) and is
  the merchant's explicit opt-in; its `principal_id` becomes the operation principal (§6.2).
- `reply_locale` is chosen per source by the merchant (O6 amended, §15): no store default
  locale exists in the schema. It selects the fixed template and the URL locale segment.

## 3. Qualification and mapping (closes H2, H3)

Inside the existing consumer transaction (§5.1), after `finish_social_event` has written the
comment fact, the consumer decides eligibility from the **already-authenticated decrypted
unit** only:

| Kind | Qualifies | source_object_id | actor platform id | comment_ref | occurred_at |
| --- | --- | --- | --- | --- | --- |
| `page_comment_add` | yes | `value.post_id` | `value.from.id` | `value.comment_id` | event `occurred_at` (`created_time`) |
| `instagram_comment`, `instagram_live_comment` | yes | `value.media.id` | `value.from.id` | `value.id` | event `occurred_at` (= `entry.time`, U7) |
| `page_comment_edit/remove` | **never** (no retract either) | — | — | — | — |

Fail closed (no intake row, social fact still commits as today) when: `from.id` or object id
missing/malformed; **comment id missing or not `^[0-9_]{1,80}$`**; `from.id` = `asset_id`
(seller's own comment); `parent_id` present (replies to comments do not claim in v1 — IR-7);
no active `claim_sources` row; `occurred_at` NULL; text over 256 bytes (claims parser
short-circuit). Comment text and `from.name/username` stay only inside the encrypted
inbox/social ciphertext.

`grammar.Parse(text)` (pure, frozen kw-v1) runs in memory in the consumer. The staging function
resolves `p.Keyword` against `live.offers` of the bound session (keywords are immutable, so the
resolution is stable): the intake row stores `offer_id` when the head names an offer, otherwise
only `unknown_keyword=true` — **an unresolved keyword is never persisted** (claims R5/§8).

Actor key (H3): `actor_key = hex(HMAC-SHA256(K_actor, tupleHash-encoding("meta-claim-actor/v1",
object, asset_id, from.id)))`, 64 lowercase hex. `K_actor` =
`COMMERCE_CLAIMS_ACTOR_KEY` (32 bytes, base64; all-zero rejected), loaded only by the
meta-worker consumer. Keyed, because sender ids are enumerable; domain-separated from the
unkeyed social `peer_key` (`meta-social-peer/v1`), from the label MAC and from the reply-link
derivation (`meta-claim-link/v1`). The three keys live in three different processes (meta-worker,
api, claims-worker), so no process can compare them; **domain separation is the guarantee**,
and MCI01 proves that equal key bytes under the different domains give unrelated outputs.
Deployment docs require distinct secrets. Same person on FB and IG, or on two assets,
yields different bundles (actor ≠ person, claims §1). `app_id` is in no key (actor, intake,
reply): `meta_inbox.routes` is `UNIQUE(app_id,object,asset_id)` and `asset_owners` is keyed
`(object,asset_id)` (0028), so several apps may route one asset of one store and deliver the
same global comment id; the first delivery wins, later ones are duplicates (§5.1). U8
(UNKNOWN): whether `from.id` differs per app; if it does, comments of one person delivered
through different apps split bundles (claims stay correct, dedup per comment is unaffected). No key ring in v1: rotation splits
bundles of an in-flight session (documented limit, same as label key).

## 4. Data model — migration 0064

- `claims.bundles.platform` CHECK widens to `('manual','facebook','instagram')`;
  `claims.events.source_kind` to `('manual','meta')`, `platform` likewise; existing CHECKs
  `(platform='manual')=(label IS NOT NULL)` and `(source_kind='manual')=(principal_id IS NOT
  NULL)` stay and now carry meaning. `claims.events.reason` adds `RATE_LIMITED` with
  `CHECK (reason IS DISTINCT FROM 'RATE_LIMITED' OR source_kind='meta')` (§4.2).
- `claims.events.source_event_id` for meta = `claims.meta_intake.inbox_event_id` of the one
  intake row of that comment (UUID; unique per store across kinds). No FK (inbox retention
  must not block claims retention).
- `live.claim_window_intervals` (H5): `(tenant,store,session,generation, opened_at,
  closed_at NULL)`, PK `(tenant,store,session,generation)`. Maintained **only** by an
  `AFTER INSERT OR UPDATE OF state` trigger on `live.claim_windows` (function
  `live.track_claim_window_interval()`, SECURITY DEFINER, owner `commerce_claims_writer`): an
  insert or transition into OPEN inserts `(generation, opened_at)`; OPEN→CLOSED sets
  `closed_at` of `(…, OLD.generation)` exactly once (`closed_at IS NULL` → value; ≠1 row
  updated → raise 23514). No INSERT/UPDATE/DELETE grant to any login role; no Go helper.
  0064 backfills one interval per existing `generation>0` row from its current
  `opened_at`/`closed_at` (an OPEN window gets `closed_at NULL`). Earlier closed generations
  of a window are unknown and never matched (documented; MCI06).
- `claims.meta_intake` (text-free staging, FORCE RLS):

```
tenant_id, store_id, id uuid, inbox_event_id uuid UNIQUE, source_id uuid → live.claim_sources,
session_id uuid, platform text,
app_id text NOT NULL, object text NOT NULL, asset_id text NOT NULL,
comment_ref text NOT NULL CHECK (comment_ref ~ '^[0-9_]{1,80}$'),                  -- IR-4
UNIQUE (object, asset_id, comment_ref),        -- one intake per Meta comment, globally (any app)
actor_key text ^[0-9a-f]{64}$,
occurred_at timestamptz NOT NULL, received_at timestamptz NOT NULL,             -- inbox created_at
grammar_version 'kw-v1', grammar_kind, offer_id uuid NULL, unknown_keyword boolean,
quantity int NULL, explicit_quantity boolean NULL,
state text CHECK IN ('PENDING','APPLIED','DROPPED','FAILED'), drop_reason text NULL,
fail_code text NULL CHECK (fail_code ~ '^[a-z0-9_]{1,40}$'),
attempts int NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 10), not_before timestamptz NOT NULL,
applied_event_id uuid NULL, lease_xid xid8 NULL, created_at, updated_at
CHECK (offer_id IS NULL OR NOT unknown_keyword), CHECK (grammar_kind<>'NO_MATCH' OR offer_id IS NULL)
INDEX (not_before, created_at) WHERE state='PENDING'
```

No `job_id`: the intake is polled, not a River job (IR-5 superseded, §15).

### 4.1 Roles

- `commerce_claims_intake` NOLOGIN, exactly one dedicated login (validated by a new
  `platform.ValidateClaimsIntakePool` modelled on `ValidateMetaConsumerPool`; mixed/owner/
  SET ROLE rejected in both directions; `validatePoolAuthority` must reject any runtime login
  that can reach it). It applies intake rows and produces reply operations. Table access is
  only through RLS policies whose scope is `claims.intake_scope()` — a STABLE SECURITY DEFINER
  (owner `commerce_claims_writer`) returning `(tenant_id, store_id, session_id)` of the single
  `claims.meta_intake` row with `lease_xid = pg_current_xact_id()`, else zero rows.
  **No GUC-only authority**: setting `app.tenant_id` by hand grants nothing without a leased
  row. The worker still sets `app.tenant_id/app.store_id` to the leased scope because the
  shared ingest core checks GUC equality (claims §4.3 step 1); `app.principal_id` and
  `app.buyer_id` stay unset in every intake transaction.
- `commerce_meta_consumer` gains EXECUTE on `meta_inbox.stage_claim_intake` only (an
  in-domain Meta function). No claims/live/river privilege; claims §3.2 row "any
  `commerce_meta_*` role: none" stays true for tables.
- `commerce_meta_writer` (NOLOGIN, owner of `stage_claim_intake`) gains EXECUTE on
  `claims.insert_meta_intake` only. This is the single sanctioned Meta→claims edge (H4).
- Definers: fixed `search_path=pg_catalog`, `REVOKE ALL FROM PUBLIC`, `COMMENT ON` naming the
  owning package and the only caller role; every statement filters tenant/store explicitly.

### 4.2 Abuse bounds (H6)

- Per `(session, actor_key)`: ≤10 ACCEPTED commands whose `occurred_at` lies in
  `(occurred_at − 60 s, occurred_at]` of the new command (event time, so a River/poll backlog
  replay of legitimately spaced comments is not rate-limited). Per session ≤5000 bundles with
  platform ≠ manual. Over either bound → `REJECTED/RATE_LIMITED` event (no bundle/line write,
  no reply).
- Serialization: inside `IngestMetaIntake`, right after the claim-source advisory lock (claims
  §4.3 step 2), take `pg_advisory_xact_lock('claims-actor|t|s|session|actor_key')` then
  `pg_advisory_xact_lock('claims-session-cap|t|s|session')`, then count. Manual ingest never
  takes these keys.
- Staging bound (I23, viral post): `claims.insert_meta_intake` locks the source row
  `FOR NO KEY UPDATE`; when `intake_count >= 50000` it increments `intake_capped` and stages
  nothing (returns NULL), else increments `intake_count`. NO_MATCH comments count.
- Values are the IR-6 defaults.

### 4.3 Exact privilege delta (KC03 and MCI02 compare for equality)

Shorthand as claims §3.2. `I` = `(tenant_id,store_id) = (SELECT tenant_id,store_id FROM
claims.intake_scope())`; `IS` = `I AND session_id = (SELECT session_id FROM
claims.intake_scope())`; `LEASED` = `lease_xid = pg_current_xact_id()`; `SYSTEM` =
`app.principal_id IS NULL AND app.buyer_id IS NULL`; `G` = `tenant_id=app.tenant_id AND
store_id=app.store_id` (GUC scope, as 0060); `M` = `G AND app.principal_id IS NOT NULL AND
app.buyer_id IS NULL` (merchant tx); `NOGUC` = `app.tenant_id`, `app.store_id`,
`app.principal_id` and `app.buyer_id` all unset/''. `NOGUC` policies exist only for
`commerce_claims_writer` (NOLOGIN, reachable only through definers) and only for the two
definers that run without any scope GUC (§4.4 clause 9); they never widen a buyer or merchant
transaction, which always sets `app.tenant_id`, so the 0060 GUC-scoped policies stay the only
ones that apply there. No policy on a claims table references
another claims table directly; `intake_scope()` reads `claims.meta_intake` as its owner
through policy `intake_owner_read` (`LEASED`), which does not call `intake_scope()`, so RLS
recursion (42P17) is impossible. All rows below are additions; every existing claims §3.2 row
is unchanged.

| Role | Object | Privilege | RLS policy |
| --- | --- | --- | --- |
| commerce_claims_intake | claims.meta_intake | SELECT; UPDATE(state,drop_reason,applied_event_id,lease_xid,updated_at) | read/update USING `LEASED`, WITH CHECK `LEASED OR lease_xid IS NULL` |
| commerce_claims_intake | live.claim_windows | SELECT; UPDATE(updated_at) (row lock for the `FOR SHARE` fence only) | read `IS`; update USING `IS` WITH CHECK `false` |
| commerce_claims_intake | live.claim_window_intervals | SELECT | read `IS` |
| commerce_claims_intake | live.claim_sources | SELECT(tenant_id,store_id,id,session_id,active,private_reply) | read `IS` |
| commerce_claims_intake | live.offers | SELECT; UPDATE(updated_at) (row lock only) | read `IS`; update USING `IS` WITH CHECK `false` |
| commerce_claims_intake | claims.bundles | SELECT(all except owner_id); INSERT(tenant_id,store_id,session_id,platform,actor_key,label); UPDATE(line_count,version,updated_at) | read/update `IS`; insert `IS AND platform IN ('facebook','instagram') AND label IS NULL` |
| commerce_claims_intake | claims.lines | SELECT; INSERT(all except applied_version); UPDATE(quantity,version,updated_at) | read/update/insert `IS` |
| commerce_claims_intake | claims.events | SELECT, INSERT | read `IS`; insert `IS AND source_kind='meta' AND principal_id IS NULL` |
| commerce_claims_intake | river.river_job (post-River 0014) | SELECT, INSERT, UPDATE(kind) (River `InsertTx` always emits `ON CONFLICT (unique_key) DO UPDATE SET kind = EXCLUDED.kind`, and PostgreSQL requires UPDATE(kind) for that statement even without a conflict; same as `commerce_runtime` in river `migrate.go` and `meta_ingress` in 0003) | — (guards §5.4: BEFORE INSERT OR UPDATE rejects any change of kind/args/queue/unique_key by this login) |
| commerce_claims_intake | river.river_job_id_seq | USAGE | — |
| commerce_claims_intake | functions | EXECUTE `claims.lease_meta_intake()`, `claims.fail_meta_intake(uuid,text,boolean)`, `claims.intake_scope()`, `integration.claim_reply_plannable(uuid)`, `integration.plan_claim_reply(uuid,uuid,bytea,text,bigint)` only | — |
| commerce_claims_writer | claims.meta_intake | SELECT, INSERT, UPDATE(state,fail_code,attempts,not_before,lease_xid,updated_at) | `intake_owner_read` SELECT `LEASED`; `intake_stage` INSERT/SELECT/UPDATE `true` (bodies of `insert_meta_intake`/`lease_meta_intake`/`fail_meta_intake` are the control; `commerce_claims_writer` has no login) |
| commerce_claims_writer | live.claim_sources | SELECT; UPDATE(intake_count,intake_capped) | `true` (definer bodies filter tenant/store) |
| commerce_claims_writer | live.claim_window_intervals | SELECT, INSERT, UPDATE(closed_at) | `true` (trigger body only) |
| commerce_claims_writer | claims.links | (existing) + policy `link_system_issue` | `link_system_issue` FOR INSERT WITH CHECK `I AND SYSTEM AND generation=1` (no subquery; `issue_system_link` sets `principal_id` = source principal and `bundle_id` = the bundle of the leased intake's ACCEPTED event); **no UPDATE policy added** — `link_rotate`/`link_issue` require `app.principal_id IS NOT NULL`, which `SYSTEM` excludes |
| commerce_claims_writer | integration.operations | SELECT(id,tenant_id,store_id,action,state,request) | USING `action='meta.private_reply'` |
| commerce_claims_writer | functions | EXECUTE `identity.principal_holds(uuid,uuid,uuid,text[])` | — |
| commerce_claims_writer | live.offers | (existing 0060 column SELECT) + policy `offer_system_read` | FOR SELECT USING `NOGUC` (`insert_meta_intake`, consumer tx) |
| commerce_claims_writer | claims.links | (existing) + policy `link_system_read` | FOR SELECT USING `NOGUC` (`check_meta_reply`, dispatcher Check pool) |
| commerce_claims_writer | live.claim_windows | SELECT(tenant_id,store_id,session_id,state,generation) | `window_system_read` FOR SELECT USING `NOGUC` (`check_meta_reply` `live_closed`) |
| commerce_claims_writer | claims.events | SELECT(tenant_id,store_id,id,session_id,source_kind,source_event_id,outcome,bundle_id) | `event_system_read` FOR SELECT USING `G AND SYSTEM` (`issue_system_link`; the intake tx sets the GUCs to the leased scope, §4.1) |
| commerce_claims_writer | live.claim_sources | + INSERT(tenant_id,store_id,session_id,platform,binding_id,binding_version,object,asset_id,source_object_id,private_reply,reply_locale,active,version,principal_id); + UPDATE(binding_id,binding_version,private_reply,reply_locale,active,version,principal_id,updated_at) | `claim_source_put` FOR INSERT WITH CHECK `M`; reads/updates use the `true` policy above (`put_claim_source` body filters `G`) |
| commerce_claims_writer | meta_inbox.routes | SELECT(tenant_id,store_id,object,asset_id,binding_id,enabled) | — (no RLS on routes, 0028) |
| commerce_claims_writer | integration.bindings | SELECT(id,tenant_id,store_id,provider,external_asset_id,semantic_version,enabled) | `binding_claims_read` FOR SELECT USING `M` (`put_claim_source`) |
| commerce_claims_writer | integration.meta_page_heads | SELECT(tenant_id,store_id,binding_id,current_version) | USING `M` (`put_claim_source` private_reply check) |
| commerce_claims_writer | schemas | USAGE `integration`, `meta_inbox` (0060 already grants claims, live, identity) | — |
| commerce_claims_writer | functions | owns `live.put_claim_source(...)`; EXECUTE `commerce_runtime` | — |
| commerce_meta_consumer | functions | EXECUTE `meta_inbox.stage_claim_intake(...)` only | — |
| commerce_meta_writer | functions | EXECUTE `claims.insert_meta_intake(...)` only | — |
| commerce_meta_writer | schema | USAGE `claims` (0028 grants meta_private, control, integration, river only) | — |
| commerce_integration_writer | integration.operations | INSERT | `claim_reply_insert` FOR INSERT WITH CHECK `state='READY' AND generation=0 AND actor_kind='MERCHANT' AND action='meta.private_reply' AND purpose='service' AND provider IN ('facebook','instagram')` |
| commerce_integration_writer | claims.meta_intake, claims.events, live.claim_sources | SELECT (fixed columns used by §6.2) | `LEASED` / `IS` |
| commerce_integration_writer | river.river_job | SELECT (table level, because `plan_claim_reply` reads `xmin`, §5.4; it has no other river_job privilege: post-River 0005 revoked its UPDATE(queue)) | — |
| commerce_integration_writer | functions | EXECUTE `claims.issue_system_link(uuid,bytea)`, `claims.intake_scope()`; owns `integration.plan_claim_reply(...)`, `integration.claim_reply_plannable(uuid)` and the §5.4 deferred trigger function (SECURITY DEFINER; it reads `integration.operations` through `worker_operation_read`) | — |
| commerce_integration_writer | live.claim_sources | UPDATE(updated_at) (row lock for `FOR SHARE` only) | update USING `IS` WITH CHECK `false` |
| commerce_integration_writer | control.storefront_publications, control.storefront_domains | SELECT(tenant_id,store_id,published) / SELECT(id,tenant_id,store_id,origin,state) | `I` (`origin_ref` = the store's `ACTIVE` domain id of a published storefront; none → skip `no_storefront`, §6.2) |
| commerce_integration_writer | ops.audit_events | INSERT | `claim_reply_audit` FOR INSERT WITH CHECK `I AND action IN ('meta.private_reply.planned','claim_reply_skipped:source_off','claim_reply_skipped:binding_disabled','claim_reply_skipped:binding_changed','claim_reply_skipped:no_storefront')` |
| commerce_integration_writer | schemas | USAGE `claims`, `live`, `control`, `ops` (0008/0018 grant integration, river) | — |
| commerce_claims_intake | schemas | USAGE `claims`, `live`, `integration`, `river` | — |
| commerce_integration_writer | integration.meta_page_credentials, integration.meta_page_heads | owner-side SELECT/INSERT/UPDATE(current_version) for §7 definers | `true` (bodies are the control) |
| commerce_runtime | integration.operations | (existing) + RESTRICTIVE policy `no_merchant_claim_reply` | FOR INSERT WITH CHECK `action <> 'meta.private_reply'` |
| commerce_runtime | live.claim_sources | SELECT (no write grant) | read `G` |
| commerce_runtime | functions | EXECUTE `live.put_claim_source(...)` | — |
| commerce_worker | functions | EXECUTE `claims.check_meta_reply(uuid,bytea)`, `integration.load_meta_page_token(uuid,bigint,bytea)` | — |
| commerce_worker | schema | USAGE `claims` (0008 grants integration) | — |
| commerce_meta_registrar | functions | EXECUTE `integration.register_meta_page_token(...)` | — |
| commerce_auth | function | owns `identity.principal_holds(p_tenant,p_store,p_principal,p_permissions text[]) RETURNS boolean` (same joins as `identity.resolve_access` minus the session row: principal active, tenant active, membership active, `store:read` + every listed permission in `identity.store_grants`) | — |

No privilege for PUBLIC, `commerce_buyer_runtime`, `commerce_buyer_issuer`, `commerce_identity`,
checkout, hosted, media, the Meta ingress/lifecycle/worker roles or `commerce_meta_consumer`
beyond the one EXECUTE above. `commerce_worker` gets no claims/live table privilege.
`intake_scope()` is `REVOKE ALL FROM PUBLIC`; every role whose policies use `I`/`IS` holds
EXECUTE on it (above), because a policy expression runs with the querying role's privileges.

### 4.4 Frozen-claims amendment (integrator-frozen with this file, not by the implementer)

Each clause of `live-keyword-claims-v1.md` changed, with its replacement:

1. §1 "matched only if this session's window is OPEN at ingest and `opened_at <= occurred_at
   <= clock_timestamp()+120s`" → append: "For `source_kind='meta'` the command is matched if
   an interval of `live.claim_window_intervals` covers it (meta-claims-intake-v1 §5.3 step 2)
   and `occurred_at <= clock_timestamp()+120s`."
2. §3 "no River job kind" → unchanged for claims; append "claims owns no River kind; the
   intake worker inserts `external_operation_v1` jobs through the integration producer only
   (meta-claims-intake-v1 §6.2)."
3. §3.1 add row: `RATE_LIMITED | MATCH | set | N, flag | NULL` (bundle NULL, no line write),
   with `CHECK (reason IS DISTINCT FROM 'RATE_LIMITED' OR source_kind='meta')`.
4. §3.2 KC03 matrix: add every row of §4.3 above.
5. §3.3 "Release and hash replacement happen only here" → "Release and hash replacement
   happen only in `issue_link`. `claims.issue_system_link(p_intake uuid, p_hash bytea)`
   (owner `commerce_claims_writer`, EXECUTE `commerce_integration_writer` only) is INSERT-only
   (no `ON CONFLICT`): it inserts generation 1 for the bundle the leased intake's ACCEPTED
   event created, `principal_id` = source principal, `issued_at/expires_at` from one
   `clock_timestamp()` read (+72 h). It can never update, rotate or release."
6. §4.3 `IngestInput.SourceKind/Platform` accept `meta`/`facebook|instagram` only via
   `IngestMetaIntake`; `IngestResult` gains `BundleCreated bool json:"bundle_created"` (true
   only when this call inserted the bundle; false on `Duplicate`).
7. §4.3 add frozen entry `func IngestMetaIntake(ctx context.Context, tx pgx.Tx, intakeID
   string) (IngestResult, error)`, sharing the unexported core of `IngestParsed`
   (meta-claims-intake-v1 §5.3). KC15: `Ingest/IngestParsed` only from `RecordManualClaim`;
   `IngestMetaIntake` only from the intake worker.
8. §6 table add row: `ACTIVE (system, generation 1) | entered by plan_claim_reply for a new
   meta bundle with private_reply | 200 bound=false | binds caller`. Token bullet: "meta
   system links: `token = base64url_raw(HMAC-SHA256(K_link, …))` per meta-claims-intake-v1
   §6.2 instead of `crypto/rand`; same length, same hash-only storage." Merchant IssueLink on
   such a bundle uses `expected_generation=1` (the merchant list already shows generation);
   `expected_generation=0` gets PT409. Merchant rotation kills the sent link (buyer must get
   the new link from the merchant).
9. §3.3 "Every statement inside filters `tenant_id=app.tenant_id AND store_id=app.store_id`"
   → append: "For `insert_meta_intake`, `check_meta_reply`, `issue_system_link`,
   `intake_scope`, `lease_meta_intake`, `fail_meta_intake` and the interval trigger, the filter
   is the tenant/store passed in or derived from the locked row (intake, operation, window),
   because they run without merchant or buyer GUCs (`insert_meta_intake`, `check_meta_reply`
   and `lease_meta_intake` assert `NOGUC` and raise 22023 otherwise). The `NOGUC`/`true`
   system policies of meta-claims-intake-v1 §4.3 apply only to `commerce_claims_writer`, which
   has no login. `live.put_claim_source` uses the merchant guard (`M`) like `issue_link`."
10. §3.2 closing paragraph: the sentences "No policy references another claims table (no
    subquery), so RLS recursion (42P17) is impossible." and "No privilege for PUBLIC,
    `commerce_buyer_issuer`, `commerce_identity`, checkout, hosted, worker, any `commerce_meta_*`
    or `commerce_media_*` role." are replaced by: "No privilege for PUBLIC,
    commerce_buyer_issuer, commerce_identity, checkout, hosted, commerce_media_* roles;
    commerce_worker, commerce_meta_writer, commerce_meta_consumer and commerce_integration_writer
    hold exactly the meta-claims-intake-v1 §4.3 rows; claims-table policies may call
    claims.intake_scope() (definer, reads only claims.meta_intake through a non-recursive LEASED
    policy); no other cross-table reference." (RLS recursion stays impossible because
    `intake_owner_read` does not call `intake_scope()`, §4.3.) The KC03 subtest
    `foreign-roles-denied` (`tests/foundation/live_claims_schema_test.go`) drops
    `commerce_integration_writer` for `SELECT` on `claims.events` (its §4.3 column SELECT makes
    `SELECT 1 FROM claims.events` succeed); every other role/table/statement pair stays 42501.
    This gate change is **recorded by the integrator at freeze as part of this amendment**, not
    by the implementer; the implementer applies exactly this one exemption and may not widen it.

## 5. Staging and apply (closes H4, H5)

### 5.1 Stage (consumer transaction, meta-consumer step 5–6 extension)

After `meta_inbox.finish_social_event` succeeds for a qualifying comment and before COMMIT,
the consumer calls:

`meta_inbox.stage_claim_intake(p_event uuid, p_job bigint, p_attempt integer, p_object_id
text, p_comment_ref text, p_actor_key text, p_occurred timestamptz, p_kind text, p_keyword
text, p_quantity integer, p_explicit boolean) RETURNS uuid` (NULL = not staged). Owner
`commerce_meta_writer`, EXECUTE `commerce_meta_consumer`. It authenticates `session_user`
and re-derives tenant/store/app/object/asset/route from the **locked** inbox event and running
`river_meta` job with the same shared validator as `finish_social_event` (caller cannot supply
scope), requires this event's comment fact in `social.comment_events` in this tx, then calls
`claims.insert_meta_intake(p_tenant, p_store, p_event, p_received, p_app, p_object, p_asset,
p_object_id, p_comment_ref, p_actor_key, p_occurred, p_kind, p_keyword, p_quantity,
p_explicit) RETURNS uuid` (owner `commerce_claims_writer`, EXECUTE `commerce_meta_writer`
only). That claims function locks the active source by `(object, asset_id, p_object_id)`
`FOR NO KEY UPDATE` and requires its tenant/store to equal the passed scope, applies the §4.2
staging bound, resolves the offer by keyword, and `INSERT … ON CONFLICT (object, asset_id,
comment_ref) DO NOTHING` with `state PENDING, attempts 0, not_before now,
lease_xid NULL`. **If an intake row for that comment already exists, it returns NULL: no
second intake and no second claims event; the social fact still commits.** This covers a
redelivery with a different payload hash, one IG comment delivered as both
`instagram_comment` and `instagram_live_comment`, and one comment delivered through two apps
routed to the same asset (`app_id` is stored as "first delivering app" only). `p_keyword` is used only for resolution and
never stored.

As with the social `subject_key`, SQL validates shapes, not that the Go consumer derived them
from the ciphertext; a fully compromised consumer holding the keyring is out of scope.

### 5.2 Atomicity

Social fact, intake row and the `processed` mark commit in the one consumer transaction; a
rollback anywhere leaves none of them (MCI04). The consumer inserts no River job and gets no
`river`/`river_meta` privilege beyond its existing ones, so `meta-runtime-isolation-v1` is
unchanged. No ACK path changes: the webhook was acknowledged at inbox admission.

### 5.3 Apply (claims intake worker, new `cmd/claims-worker`)

Host: new command `cmd/claims-worker` with two pools, both validated at startup:
(a) `COMMERCE_CLAIMS_INTAKE_DATABASE_URL` (the `commerce_claims_intake` login;
`ValidateClaimsIntakePool`) for the intake poller plus an insert-only River client on the main
`river` schema; (b) `COMMERCE_WORKER_DATABASE_URL` (`commerce_worker`) for the
`external_operation_v1` River worker running `core.NewDispatcher` with the two routes of §6.3.
It loads `COMMERCE_CLAIMS_REPLY_LINK_KEY` and the Page-token keyring
(`COMMERCE_META_PAGE_TOKEN_KEYS`); it never loads the Meta payload keyring or `K_actor`.
meta-worker is unchanged.

Poll loop (N=2 goroutines default; 1 s idle sleep; one row per transaction). One bounded READ
COMMITTED tx per row:

1. `claims.lease_meta_intake() RETURNS SETOF claims.meta_intake` (owner
   `commerce_claims_writer`): `SELECT … WHERE state='PENDING' AND not_before <=
   clock_timestamp() ORDER BY not_before, created_at LIMIT 1 FOR UPDATE SKIP LOCKED`, sets
   `lease_xid = pg_current_xact_id()`, returns the row (zero rows → sleep).
2. `claims.IngestMetaIntake(ctx, tx, intakeID)`. Core differences from the manual path only:
   after the claim-source and §4.2 advisory locks, lock `live.claim_windows` of the session
   `FOR SHARE` (any state), **then** select the interval with `opened_at <= occurred_at` and
   (`closed_at IS NULL` or (`occurred_at < closed_at` and `received_at <= closed_at + 60 s`));
   no match, source inactive, or `occurred_at > clock_timestamp()+120s` → mark
   `DROPPED(window_closed)` (claims §4.3 step 3 semantics: WINDOW_CLOSED is not persisted as a
   claims event). Window fence = the matched interval's generation; unknown keyword =
   `UNKNOWN_KEYWORD` without a keyword; `ActorLabel` empty (label NULL); `principal_id NULL`;
   §4.2 bounds before the bundle step. Idempotency = claims §4.3 step 2 on `source_event_id`
   (immutable-fact compare).
3. On ACCEPTED with `BundleCreated` and source `private_reply=true`: §6.2 in the same tx
   (`claim_reply_plannable` first; only on `OK` the River job and `plan_claim_reply`).
4. Mark intake `APPLIED` + `applied_event_id`, clear `lease_xid`, COMMIT.

Errors (the tx rolls back; then a separate short tx calls `claims.fail_meta_intake(intake,
code, final)` which increments `attempts`, sets `not_before = now + least(2^attempts s,
5 min)`, and at `attempts = 10` or `final` sets `FAILED` with `fail_code`):
22023/invalid, 23514 (CHECK, e.g. future `occurred_at`), 23503 (FK), 42501 (privilege), 23505
on the reply semantic key (§6.1: impossible by construction, so an invariant breach) →
`final=true`, operator-visible, never auto-retried; 40P01/55P03/timeouts/connection loss →
`final=false`; any other SQLSTATE → `final=false` (bounded by the 10-attempt cap). A reply
precondition is never an error here: it is a skip (§6.2), so the claim itself commits. A process crash
between the two txs leaves the row PENDING without an attempt increment (documented limit: a
row that crashes the process is re-polled; MCI04 bounds it by a crash test).

### 5.4 Main-`river` guards (post-River 0014)

- BEFORE INSERT **OR UPDATE** on `river.river_job`: when `pg_has_role(session_user,
  'commerce_claims_intake','MEMBER')`, on INSERT require `kind='external_operation_v1'`,
  default queue, `unique_key IS NULL`, args exactly `{"operation_id":<uuid>,"version":1}`; on
  UPDATE reject (22023) any change of `kind`, `args`, `queue` or `unique_key` (River's
  `ON CONFLICT … DO UPDATE SET kind = EXCLUDED.kind` writes the same value, so the real
  `InsertTx` passes and any other UPDATE from the intake login fails). Else 22023.
- DEFERRABLE INITIALLY DEFERRED constraint trigger AFTER INSERT on `river.river_job` WHEN
  `NEW.kind='external_operation_v1'`: at COMMIT an `integration.operations` row with
  `id = args.operation_id` and `job_id = NEW.id` must exist (holds for the existing merchant
  `Plan` too, which inserts both in one tx; MCI10 re-runs dispatcher gates). The trigger
  function is owned by `commerce_integration_writer`, SECURITY DEFINER, and reads
  `integration.operations` through `worker_operation_read`.
- `plan_claim_reply` requires the passed job to be that exact row, inserted in this tx
  (`xmin = pg_current_xact_id()`); it reads `river.river_job` with the table-level SELECT of
  §4.3.

## 6. First private reply (closes H7)

### 6.1 Budget rules (binding)

- At most **one** private reply per Meta comment, forever: operation `semantic_key =
  "mpr:" + hex(sha256(object|asset_id|comment_ref))[:48]`, UNIQUE per tenant/store
  (existing) **and** a 0064 partial global unique index `ON integration.operations
  (semantic_key) WHERE action='meta.private_reply'`. The key never includes template, policy,
  source version, link generation or token version. A conflict cannot arise from this
  producer (one intake per comment, globally, applied once under `FOR UPDATE`) and
  `commerce_runtime` cannot insert this action (§4.3 restrictive policy); a 23505 is therefore
  an invariant breach → intake `FAILED(reply_key_conflict)`, not a retry loop.
- At most one automated reply per bundle in v1: only the ACCEPTED event that created the bundle
  plans a reply (IR-3). Later comments update the same bundle; the buyer reopens the same link.
- Never fall back to another channel (DM, public reply, other asset, other platform, email).
  Denial or failure is final for that comment.
- UNKNOWN is never blind-retried (external-operation v1). Reconcile is query-only; until U4 is
  proven it returns UNKNOWN and the budget exhaustion path marks manual-required.

### 6.2 Producer bridge

`integration.plan_claim_reply(p_intake uuid, p_operation uuid, p_link_hash bytea,
p_link_key_id text, p_job bigint) RETURNS uuid`, owner `commerce_integration_writer`, EXECUTE
`commerce_claims_intake` only. Derives everything from the `LEASED` intake row, its ACCEPTED
claims event (bundle created in this tx) and the source: `actor_kind='MERCHANT'`,
`principal_id = live.claim_sources.principal_id`, binding = source binding (version: see below),
**provider = the source binding's provider** (`facebook` | `instagram`),
`external_asset_id = asset_id`, action `meta.private_reply`, purpose `service` (IR-2); the
operation's binding version is the **current** enabled version read under the §6.2 lock (IR-17),
not the version stored on the source. In the
same tx it calls `claims.issue_system_link(p_intake, p_link_hash)` (§4.4 clause 5), inserts the
operation (plain INSERT, `job_id = p_job`), its READY event and audit. The operation UUID and
the River job (`external_operation_v1`, args `{operation_id, version:1}`) are created by Go
before the call.

Preconditions never roll back the claim. Before inserting the River job, Go calls
`integration.claim_reply_plannable(p_intake uuid) RETURNS text` (SECURITY DEFINER, owner
`commerce_integration_writer`, EXECUTE `commerce_claims_intake` only). It locks the source's
binding, then the source row, `FOR SHARE` (lock order §9; a source whose locked `binding_id`
differs from the one first read → `binding_changed`) and returns `OK` or one fixed skip code
(IR-17): `source_off` (source inactive or `private_reply=false`), `binding_disabled` (the
binding is disabled), `binding_changed` (the binding now targets a different Page/IG object id
than `claim_sources.asset_id`, or its `binding_id` differs from the first read), `no_storefront`
(no published store with an `ACTIVE` domain for `origin_ref`). A bumped
`bindings.semantic_version` (e.g. `SetBindingEnabled` off then on) is **not** a skip: while the
binding is enabled and still targets the same object id, the plan uses the current enabled
version and the source needs no re-save. On a skip code it writes audit
`claim_reply_skipped:<code>` (principal = source principal) and nothing else: no job, link or
operation; the claim and intake commit as ACCEPTED/APPLIED. `plan_claim_reply` is called only
after `OK`; the locks held since then keep the checked rows unchanged, so any precondition
failure inside `plan_claim_reply` is an invariant breach (22023 → intake `FAILED`, §5.3), never
a normal path. Principal authority and credentials are not plan preconditions: an inactive
principal or missing Page token yields a visible operation denied at dispatch (Check
`principal_revoked`, LoadSecret `credential_unavailable`); memberships have no DELETE grant in
any migration, so the principal FK (23503) cannot fail for a stored source principal.

Link token: `token = base64url_raw(HMAC-SHA256(K_link, tupleHash-encoding("meta-claim-link/v1",
tenant, store, bundle, operation_id)))` (43 chars, same format as `LinkToken`). Only
`sha256(token)` reaches SQL. The token is never stored, logged or put in the operation request;
the adapter re-derives it. `K_link` = `COMMERCE_CLAIMS_REPLY_LINK_KEY` (32 bytes), loaded only
by `cmd/claims-worker`. `link_key_id = hex(HMAC-SHA256(K_link, "meta-claim-link-key-id/v1"))[:16]`
(a fingerprint; no key ring). Key rotation between plan and dispatch → the id no longer matches
→ Check denies.

Frozen request (≤ 2 KiB, no token/text/name): `{v:1, platform, source_id, asset_id,
comment_ref, bundle_id, session_id, link_generation:1, link_key_id, locale, template:
"claim-link/v1", policy:"mpr-policy/v1", message_type:"first_private_reply",
takeover_generation:0, origin_ref (published storefront id), deadline_at, live_media: bool}`
(arch §10.2 MessageIntent: purpose, target, trigger comment, policy version, message type,
deadline, takeover version). `deadline_at`, DB-computed, = `least(occurred_at + 7 d − 1 h,
link expires_at − 10 min, CASE WHEN live_media THEN received_at + 15 min END)`.

### 6.3 Adapter routes `(facebook, meta.private_reply, service)` and `(instagram, meta.private_reply, service)`

- **Check** (every attempt; arch §10.2 "re-read at execution"): Go verifies `link_key_id`
  equals the loaded key's id (else `ErrPolicyDenied`), re-derives the token, then calls
  `claims.check_meta_reply(p_operation uuid, p_link_hash bytea) RETURNS text` (owner
  `commerce_claims_writer`, EXECUTE `commerce_worker`, STABLE, one statement on the route's
  captured `commerce_worker` pool; no transaction held across I/O). It reads the frozen
  operation row and returns `OK` or one fixed deny code: `deadline` (`clock_timestamp() >=
  deadline_at`); `source_off` (source inactive or `private_reply=false`); `principal_revoked`
  (`identity.principal_holds(…, ARRAY['live:manage','integration:execute'])` false);
  `link_invalid` (link row not `generation=1 AND token_hash=p_link_hash AND expires_at >
  clock_timestamp() + 10 min`, i.e. rotated, released or expiring); `live_closed`
  (`live_media` and the source session's claim window is not OPEN). Takeover is always 0 in
  v1. **Only a returned deny code maps to `ErrPolicyDenied` → BLOCKED_POLICY with zero HTTP
  calls.** An infrastructure error (timeout, connection, key-load failure) returns a
  non-policy error; per the frozen dispatcher that becomes UNKNOWN `policy_check_failed` and
  only Reconcile follows (the reply is not sent and the comment's budget is spent as
  manual-required, never mislabelled a policy denial; documented limit, §14). Binding
  enabled/version is re-checked by the dispatcher's own final gate.
- **LoadSecret** (§6.4; dispatch mode only): `integration.load_meta_page_token(operation,
  generation, lease_token)`. No current credential, or credential without the attested scope
  (§7) → `ErrPolicyDenied` → BLOCKED_POLICY (capability evidence, arch §10.2/I07).
- **Dispatch**: re-derive token, render the fixed template for `locale` (`claim-link/v1`: one
  sentence + URL `https://<origin>/<locale>/claim#t=<token>`; no merchant free text, price,
  discount or delivery promise in v1), `POST https://graph.facebook.com/<pinned U5>/{asset_id}/
  messages` with `{"recipient":{"comment_id":…},"message":{"text":…}}`, Page token in
  `Authorization` header if U6 holds, otherwise form body — never URL/query/log. `asset_id` =
  Page id (FB) or IG professional account id (IG, Facebook Login path). 2xx with `message_id` →
  SUCCEEDED (`provider_reference` = message id ≤200). A documented permanent 4xx (policy,
  window, permission; codes per U3 once probed) → FAILED_FINAL with a fixed code; until U3 is
  closed, any other 4xx → UNKNOWN. Timeout, 5xx, transport error, unparsable body, 429 →
  UNKNOWN.
- **Reconcile**: query-only. v1 returns UNKNOWN (U4). After U4 is proven LIVE, a reviewed
  amendment may map a positive read to SUCCEEDED. Never Dispatch again.
- Race accepted: a merchant rotation that commits after Check and before the HTTP call sends a
  dead link (claims §6 semantics: rotation kills the old link); MCI09 covers the lock order.
- Callback errors, request/response bodies, tokens and comment ids never reach logs or River.

### 6.4 Dispatcher amendment (external-dispatcher-v1; appended there as a proposal)

`DispatchRoute` gains two optional fields, both set or both nil:
`LoadSecret func(context.Context, pgx.Tx, SecretClaim) (Secret, error)` and
`DispatchWithSecret func(context.Context, DispatchRequest, Secret) (Outcome, error)` (a route
sets either `Dispatch` or this pair). `SecretClaim{OperationID string; Generation int64;
LeaseToken []byte}` is given **only** to `LoadSecret`, whose sole allowed body is one call to a
lease-fenced SQL loader. In `dispatch` mode only, after `finalDispatchGate` passes, the
dispatcher opens one bounded (DBTimeout) transaction, calls `LoadSecret`, and ends the
transaction before calling `DispatchWithSecret`. `Secret` is an opaque type whose
`String/GoString/Format/MarshalJSON/MarshalText` return `[redacted]`; the dispatcher zeroes it
after the callback returns. It is never passed to Check or Reconcile. `ErrPolicyDenied` from
`LoadSecret` → BLOCKED_POLICY `credential_unavailable` (pre-dispatch, allowed); any other error
→ UNKNOWN `secret_load_failed` with zero calls. Timing: the lease inequality becomes
`CallTimeout + 3*DBTimeout + 1s < lease`. Existing routes are unaffected. Route callbacks may
capture a read-only pool of the dispatcher's own role for Check; "all DB transactions have
ended before callbacks" continues to mean dispatcher-owned transactions.

## 7. Page-token custody (stripe §0.2 pattern, adapted)

- Page access tokens are **per store per binding**, never environment-global. 0064 adds
  `integration.meta_page_credentials(tenant, store, binding_id, version, key_id, nonce,
  ciphertext 17..8192, scopes_attested text[] NOT NULL, principal_id, created_at)` + head
  `current_version` on a `integration.meta_page_heads` row, deferred FK as in 0014. FORCE
  RLS; no runtime SELECT of ciphertext.
- Payload `meta-page-token-v1` = `{page_access_token}` only; AES-256-GCM with a distinct AAD
  `["livecommerce/meta-page-token/v1", tenant, store, binding, provider, asset_id, version,
  key_id]` and a keyring (`COMMERCE_META_PAGE_TOKEN_KEYS`) separate from the Meta payload
  keyring and from payment keys. Only `cmd/claims-worker` loads it.
- Provisioning in R1: operator registrar CLI calls `integration.register_meta_page_token`
  (owner `commerce_integration_writer`, EXECUTE `commerce_meta_registrar` only; owner
  membership + store validated; audited; version CAS). `scopes_attested` is the operator's
  attestation from the token debug output (FB: `pages_messaging`; IG: `instagram_manage_comments`
  + `pages_read_engagement`, and `instagram_manage_messages` if U2 says so); LIVE verification
  is MCI11. Token plaintext is an owner-supplied registrar input, never in PG/logs. OAuth
  issuance and refresh remain T07.
- Loader `integration.load_meta_page_token(p_operation uuid, p_generation bigint, p_lease_token
  bytea)` (owner `commerce_integration_writer`, EXECUTE `commerce_worker`): lease-fenced like
  `integration.load_stripe_credential` (0061), called only from `LoadSecret` (§6.4). Returns
  the **current head for the frozen binding** (a Page token refresh is not a semantic change,
  external-operation v1; an asset change is a new binding and therefore never used) plus its
  `scopes_attested`; no head → zero rows (→ `ErrPolicyDenied`). Observations record the
  credential version used.
- Webhook signing stays per app+object (`COMMERCE_META_APPS_JSON`, meta-runtime v1): Meta signs
  with the platform app secret, and tenant routing is the server-owned asset→route map. No
  per-store Meta webhook secret is introduced.

## 8. Privacy

Nothing in `claims.*`, `live.claim_sources`, `claims.meta_intake`, `integration.operations`,
audit, River args or logs holds comment text, unresolved keywords, `from.name`, username,
`from.id` in clear, or link tokens. `actor_key` (keyed), `comment_ref` (IR-4) and
`source_object_id` are pseudonymous personal/platform data with retention classes added to the
claims §8 list; purge/DSAR stays T14 and blocks production mount. Sentinel scans in MCI08.

## 9. Lock order

Consumer: meta-consumer order (tenant → store → binding → route → event → `river_meta` job) →
social fact → `live.claim_sources` (FOR NO KEY UPDATE) → `live.offers` (plain read) → intake
insert. Intake worker: intake (FOR UPDATE SKIP LOCKED) → claims §4.3 order from the
`claim-source` advisory onwards, with `claims-actor` then `claims-session-cap` advisories
right after `claim-source` → `live.claim_windows` FOR SHARE → `live.claim_window_intervals` →
offers → bundle → lines → events → `integration.bindings` FOR SHARE → `live.claim_sources`
FOR SHARE (`claim_reply_plannable`; binding before source, as the consumer) → River job →
`claims.links` insert → operation. Never takes a `meta_inbox` lock. Dispatcher: unchanged (binding →
operation); `check_meta_reply` is a lock-free read. Window close takes `live.claim_windows`
then (trigger) `live.claim_window_intervals`, the same relative order.

## 10. What each evidence class can prove

| Class | Proves | Cannot prove |
| --- | --- | --- |
| MOCK (UNIT/REAL_PG) | Signed synthetic webhook replay through the real API handler → inbox → consumer → staging → claims → operation → fake Graph `httptest` server; idempotency, atomicity, privacy, lock order, UNKNOWN handling | Real payload shape for FB live video (U1), permissions, App Review, window/limit enforcement by Meta |
| SANDBOX | Not applicable: Meta has no sandbox for Page messaging; a Meta test app with app-role users is LIVE with Standard Access | — |
| LIVE read-only probes | Token debug/permissions list, `subscribed_apps`, a real webhook delivery captured into the inbox (read path), comment field reads (U1, U4, U5, U6) | Sending |
| LIVE send | One private reply to an app-role test user on an owner-approved test Page | Anything for non-role users before Advanced Access/App Review |

## 11. Owner inputs (engineering cannot close)

O1 Meta app (id) in Live mode with Webhooks product; Page + IG professional (public) test assets.
O2 Permissions via Facebook Login for Business: `pages_manage_metadata`, `pages_show_list`,
`pages_read_engagement`, `pages_messaging` (+ person with `MESSAGING` task),
`instagram_basic`, `instagram_manage_comments`; `instagram_manage_messages` only if U2 says so.
O3 Advanced Access / App Review for the above (IG comment webhooks require Advanced Access, F4;
private replies to non-role users need it, F1). O4 A Page access token supplied to the registrar
for the test Page (until T07 OAuth). O5 Explicit approval in chat for each LIVE send probe (a
message to a real person, AGENTS.md). O6 Approval of the reply template wording and locales
(customer-visible copy, SHOPLINE parity). O7 Retention days for the new classes (U08).

## 12. Gates (all NOT_RUN)

Real-PG tests in `tests/foundation/meta_claims_intake_test.go`, one `TestMetaClaimsMCIxx…` per
gate; pure tests in `internal/claims` / `internal/integrations/meta`. Every sentinel synthetic.

| Gate | Evidence | Required |
| --- | --- | --- |
| MCI01 | MOCK (UNIT) | Qualification table §3 for all 7 kinds incl. edit/remove/reply/own-comment/missing `from.id`/missing object/missing or malformed comment id/NULL occurred_at; actor key vectors (domain separation vs peer_key, label MAC and link derivation with **equal key bytes** → unrelated outputs; per asset/platform differ, same across `app_id`; all-zero key rejected); link key id vectors; redacted formatting of every new input type and of `Secret` |
| MCI02 | MOCK (REAL_PG) | 0064 fresh + populated-0063 upgrade, migrate twice; FORCE RLS; **§4.3 privilege delta equality** (direct and inherited, including schema USAGE and function EXECUTE); each definer exercised from its real caller role with the real GUC state (consumer tx without GUCs resolves an offer; Check pool without GUCs sees the link and the window; `NOGUC` policies return zero rows inside a buyer or merchant tx); `commerce_meta_consumer` has no claims/live/river table privilege and meta-isolation MIso gates still pass; intake role without a leased row sees/writes nothing even with forged GUCs; `link_system_issue` cannot insert generation ≠1 or with `app.principal_id` set, and no intake-context UPDATE of `claims.links` succeeds; `commerce_runtime` cannot insert `meta.private_reply`; pool validator rejects mixed roles; CHECK widening both directions; interval backfill for an OPEN window |
| MCI03 | MOCK (REAL_PG) | `claim_sources`: route mismatch 409; provider/binding mapping facebook/instagram; one object → one session under concurrency; permissions `live:manage`+`integration:execute`; `private_reply` requires credential |
| MCI04 | MOCK (REAL_PG + River) | Signed webhook replay end-to-end to ACCEPTED claim; forced failure at social fact / stage / processed mark / COMMIT → zero intake and fact; unbound object → social fact only; intake apply failure paths: 22023/23514 → FAILED once, transient → backoff then APPLIED, attempts cap 10 → FAILED; child-process kill mid-apply → row re-polled and applied once; job without operation and job of another kind from the intake login rejected (§5.4); a real River `InsertTx` of `external_operation_v1` from the intake login succeeds (no 42501, `ON CONFLICT … DO UPDATE SET kind` path included); a `kind`/`args`/`queue`/`unique_key` UPDATE of `river.river_job` from that login is rejected 22023; `plan_claim_reply` reads `xmin` of the inserted job with `commerce_integration_writer`'s table-level SELECT (no 42501) |
| MCI05 | MOCK (REAL_PG) | Same inbox event ×20 concurrent → one intake, one claims event; redelivered webhook → duplicate; **same comment id with a different payload hash, the same IG comment id as `instagram_comment` and `instagram_live_comment`, and the same comment id delivered through two apps routed to one asset → one intake, one claims event, one operation, final quantity unchanged (late `A1` after `A1+3` does not reset)**; immutable-fact mismatch 409; unknown keyword stores no keyword (DB-wide sentinel scan); S03 unknown→create offer→redeliver = duplicate UNKNOWN_KEYWORD; staging cap 50 000 → no intake, `intake_capped` counts |
| MCI06 | MOCK (REAL_PG) | Window intervals: late webhook inside interval + grace accepted with that generation; beyond grace DROPPED; comment before open DROPPED; close/reopen does not merge generations; close committing while an apply waits on the window lock is observed (no acceptance after close); future `occurred_at` > +120 s → DROPPED; window OPEN at migration has an interval; IG delivery-time case (U7); rate bound under 10 concurrent applies of one actor and of one session → exactly the bound ACCEPTED, rest RATE_LIMITED, no bundle; backlog replay of spaced comments not rate-limited |
| MCI07 | MOCK (REAL_PG + fake Graph) | Binding version bump with the binding enabled and the same object id → reply planned with the **current** version (no skip; dispatcher gate passes); binding disabled (`binding_disabled`) / binding re-pointed to a different Page or IG object id (`binding_changed`) / source `private_reply` off / no active storefront domain before apply → claim ACCEPTED, intake APPLIED, no job/link/operation, one `claim_reply_skipped:<code>` audit row; One reply per new bundle; second comment same actor no reply; semantic key per comment survives template/source/key-version change (no second operation); operation provider = binding provider and dispatcher gate passes; link generation 1 hash = sha256(re-derived token); Check denials (deadline, source off, principal revoked, IG live window closed, merchant rotate/release before dispatch, link expiring within 10 min, key id change, binding changed, no credential via LoadSecret) → BLOCKED_POLICY with zero HTTP calls; Check infra error → UNKNOWN `policy_check_failed`, zero HTTP; Page token never visible to Check or Reconcile (callback instrumentation); fake 2xx → SUCCEEDED; 5xx/timeout/429/garbled/unclassified 4xx → UNKNOWN then query-only, never a second POST (child-process kill after send) |
| MCI08 | MOCK (REAL_PG) | Sentinel scan of every column in claims/live/integration/audit/River/logs: no comment text, name, username, raw `from.id`, token, Page token; Page token ciphertext AAD swap/tamper fails; meta-worker and API processes cannot load the page-token keyring or `K_link` |
| MCI09 | MOCK (REAL_PG) | Lock-order workload: consumer staging vs window close vs offer deactivate vs redeem vs issue_link vs intake apply vs dispatcher → zero 40P01; `pg_stat_activity` interleaves, no sleeps |
| MCI10 | REVIEW + regression | Source guards (callers of each ingest entry, no new dependency, no network in consumer/intake poller, only `LoadSecret` sees `SecretClaim`); MC01–07, MI01–07, MIso, KC01–15 (with §4.4 matrix), dispatcher gates unchanged plus the §6.4 hook tests; full `go test -race ./...`, `go vet ./...`, `python3 scripts/check_packet.py`; independent test_worker + security_reviewer |
| MCI11 | LIVE read-only | U1, U4, U5, U6 probes and permission listing on O1 assets; captured webhook shape stored as a redacted fixture; no send |
| MCI12 | LIVE (owner-approved) | One private reply to an app-role test user on FB and one on IG (live comment during broadcast, within 15 min of receipt); readback in Page inbox; U2/U3 recorded |

Commands (record exit codes): `go test -count=1 ./internal/claims/... ./internal/integrations/...`;
`bash scripts/dev/test-focused.sh '^TestMetaClaimsMCI'`; full suite per PROCESS §2.4.

## 13. Integrator rulings requested

IR-1 Gate prefix: brief asked `MI01..`; that collides with `meta-inbox-v1` MI01–07 (PASS), so
this draft uses `MCI`. Confirm or rename.
IR-2 Operation purpose `service` (reply to the buyer's own request) vs `transactional`; never
`marketing`.
IR-3 One automated reply per bundle (default) vs one per ACCEPTED comment (still ≤1 per comment).
IR-4 `comment_ref` storage: plain Meta comment id in `claims.meta_intake` and the frozen
operation request (needed as `recipient.comment_id`; retention class added) vs re-reading it
from the encrypted social copy at Dispatch (needs the Meta payload keyring in the dispatcher).
IR-5 River job (default, reciprocal deferred guards §5.2, grants the consumer one guarded job
kind) vs fenced poll by the intake worker (no River grant to the consumer).
IR-6 Late-arrival grace 60 s and rate bounds 10/min/actor, 5000 bundles/session.
IR-7 Replies to comments (`parent_id` set) excluded in v1.
IR-8 New frozen entry `IngestMetaIntake` + KC15 guard widening (touches FROZEN claims §4.3/§9).
IR-9 System link issuance `claims.issue_system_link` (generation 0 only) beside the merchant-
guarded `issue_link`; confirm no path lets it rotate or release.
IR-10 Page-token custody tables in 0064 vs reusing `integration.merchant_accounts` (would widen
its provider CHECK, which Stripe B1's 0061 also edits — ordering conflict).
IR-11 FB live-video claims stay MOCK-only until U1 is closed by MCI11.
New in round 1: IR-12 approve the §6.4 dispatcher hook (or require a dedicated Meta reply
worker instead); IR-13 `cmd/claims-worker` as the first production host of the main-`river`
`external_operation_v1` worker — confirm no other main-`river` consumer is planned, since a
missing route leaves foreign operations READY (dispatcher step 1).

## 14. Known limits

No claim retract on edit/remove; no DM follow-up or conversation takeover; no merchant template
text; IG Live replies only within 15 min of delivery and while the claim window is OPEN (a proxy
for "during broadcast"; Meta's own refusal is then FAILED_FINAL once U3 is known, UNKNOWN before);
IG windows use delivery time (U7); earlier closed generations before 0064 have no interval;
actor key rotation splits bundles; a binding disabled or re-pointed to another object id silently
disarms replies of existing sources until the merchant re-enables it or re-saves the source (audited skip `binding_disabled` / `binding_changed`, no retry; a mere version bump does not disarm, IR-17); operations without a registered route stay READY by design (IR-13); a Check infrastructure error spends the comment's reply as
UNKNOWN/manual-required (frozen dispatcher); a merchant rotation racing the send sends a dead
link; Reconcile cannot prove delivery until U4; Standard Access reaches only app-role users; no
performance or capacity claim; production mount blocked by T14 retention/DSAR.

## 15. Integrator rulings (2026-09-29, binding)

- IR-1 Gate prefix `MCI` confirmed. IR-2 Purpose `service`. IR-3 One automated reply per claim
  bundle (each comment still at most one, forever).
- IR-4 `comment_id` stored plainly (platform object id, not PII); the dispatcher never needs the
  Meta payload keyring.
- IR-5 River job with commit-time reciprocal checks. IR-6 60 s grace, 10/min per commenter,
  5000 bundles per session accepted. IR-7 Replies to comments excluded in v1.
  **Superseded by amendment (IR-5 only):** the River-job option contradicts FROZEN
  `meta-runtime-isolation-v1` (consumer jobs live in `river_meta`, whose guard admits only the
  Meta family; Meta roles may not hold main-`river` or cross-domain authority; no third pool in
  meta-worker). The intake is now the fenced poll of §5.3 in `cmd/claims-worker`; the consumer
  gets no River grant.
- IR-8 Approved: `IngestMetaIntake` and the KC15 caller-guard widening amend the frozen claims
  contract; the amendment is recorded in `live-keyword-claims-v1.md` §0.1 by the implementing unit.
  **Superseded by amendment:** PROCESS.md §2.2 makes the contract integrator-frozen before
  implementation, and the change set is wider than IR-8 lists. The full clause list is §4.4
  and is recorded by the integrator at freeze, not by the implementer.
- IR-9 Approved with a gate proving `claims.issue_system_link` cannot rotate or release a link
  (MCI02; policy `link_system_issue`, INSERT-only).
- IR-10 New 0064 tables for Page tokens (do not widen the 0061 provider CHECK).
- IR-11 Facebook live-video claims stay MOCK-only until probe U1.
- O6 (reply wording): ship default templates in zh-TW, zh-CN and en, chosen by the store's
  default locale; per-store editing is R2. Owner may replace the wording before go-live.
  **Superseded by amendment (locale selection only):** no store default locale exists in the
  schema; the locale is chosen per claim source (`live.claim_sources.reply_locale`, default
  `zh-TW`, labels as `locale-routing-v1`). Templates and wording rule unchanged.
- O1–O5, O7 remain owner inputs; LIVE sends need explicit owner approval in chat.
- Freeze rulings (2026-09-29, integrator; answer to §13 round-1 questions and round-3 review):
- IR-12 Approved: the §6.4 `LoadSecret`/`DispatchWithSecret` hook amends
  `external-dispatcher-v1` (its "Amendment proposal" section is marked APPROVED). No dedicated
  Meta reply worker.
- IR-13 `cmd/claims-worker` is the first host of the main-`river` `external_operation_v1`
  dispatcher. Operations without a registered route stay READY by design (dispatcher step 1);
  this is a documented known limit (§14). `cmd/claims-worker` logs one startup line listing the
  registered routes.
- IR-14 Reply locale is per claim source (`live.claim_sources.reply_locale`), default `zh-TW`;
  no store locale column in R1.
- IR-15 The `NOGUC`-scoped system policies of §4.3 are accepted (stricter than `USING(true)`).
- IR-16 `live.put_claim_source` is owned by `commerce_claims_writer` as drafted. Each
  cross-domain SELECT it needs (`meta_inbox.routes`, `integration.bindings`, the page-token
  heads `integration.meta_page_heads`) is a named column grant of §4.3, and each grant carries a
  `COMMENT ON` stating why (which function needs it, that no other column is readable).
- IR-17 `binding_changed`: at plan time use the CURRENT enabled binding version if it still
  targets the same Page/IG object id as the claim source; skip only if the binding is disabled
  (`binding_disabled`) or points at a different object id (`binding_changed`). §6.2 and MCI07
  are updated accordingly; a version bump alone never skips.
- Round-3 P1 adjudication: River grants (§4.3 rows, §5.4 guard, MCI04) and §4.4 clause 10 with
  the KC03 `foreign-roles-denied` change are integrator amendments recorded at freeze.

Integrator note at freeze (2026-09-29): the two skip codes `binding_disabled` / `binding_changed`
are accepted (clearer diagnosis than one code). Round-3 P2s (column lists, `principal_holds`
definer, `origin_ref`, `issue_system_link` return type, stale "generation 0 only" wording in IR-9)
are fixed by the implementing unit, which must list how each was handled.

### Wave-3 amendment (2026-09-29, integrator ruling i — binding; MCI02/KC03 compare against §4.3 plus these rows)

These rows amend the §4.3 table (ratifying meta-intake-core round 2). They are additions or
replacements of the named §4.3 rows only; every other §4.3 row is unchanged.

| Role | Object | Privilege | RLS policy |
| --- | --- | --- | --- |
| commerce_integration_writer | functions | + EXECUTE `identity.principal_holds(uuid,uuid,uuid,text[])` (so `integration.register_meta_page_token` enforces §7 "owner membership + store validated": principal, tenant, store and membership active and `integration:manage` held; raises 42501 otherwise) | — |
| commerce_integration_writer | schema | + USAGE `identity` (needed to call `identity.principal_holds`) | — |
| commerce_meta_registrar | schema | + USAGE `integration` (needed to call `integration.register_meta_page_token`) | — |
| commerce_integration_writer | integration.meta_page_heads | UPDATE`(current_version, updated_at)` — replaces UPDATE`(current_version)` in the §4.3 row "integration.meta_page_credentials, integration.meta_page_heads" | `true` (bodies are the control) |

Related wave-3 rulings recorded in `docs/delivery/units/meta-intake-rulings.md`: j (RATE_LIMITED
stays out of the merchant `Stats.Rejected` 7-key board), k (§5.4 exact job link only for jobs
inserted by the intake login; CHECK `events_source_platform`), l (§3: a Facebook comment with
`parent_id == post_id` is top-level; late webhooks use the window's current match mode).

R1 final-wave ruling F2 (2026-09-29, `docs/delivery/units/r1-final-rulings.md`): the operator path to a
routable store is `meta-admin route` (registrar login): migration 0066 `integration.register_meta_binding(tenant,
store, principal, provider facebook|instagram, asset) -> (binding_id, binding_version)` (definer
commerce_integration_writer, EXECUTE commerce_meta_registrar only, principal must hold `integration:manage`,
reuses the enabled binding, audit `meta.binding_registered`) followed in the same transaction by 0028
`meta_inbox.activate_route`; `route-disable` calls `disable_route`. §7 order for operators: route → page-token →
claim source. Gate `TestMetaRouteRegistrarF2` (REAL_PG).

- 2026-09-30 (ruling B27): claims-retention-purge-v1 grants EXECUTE on `meta_inbox.lock_purgeable` to commerce_retention_writer.

## Amendment by claims-retention-purge-v1 (integrator, 2026-09-30, U08 merge)

Recorded from `contracts/claims-retention-purge-v1.md` §6 (FROZEN 2026-09-30); that file is the source of the rows.

- Clause 3: §1/§8/§14 "production mount blocked by T14/U08" is replaced by "lifted per claims-retention-purge-v1 §10".
- Clause 4 (IR-2): §3 "`K_actor` … loaded only by the meta-worker consumer" gains "and by the operator CLI
  `cmd/retention-admin` (never a service)"; the MCI10 source guard allows exactly that pair.
- Clause 5: §4.3 (MCI02 equality) gains the §4 `commerce_retention_writer` rows on `claims.meta_intake`
  (column SELECT, DELETE, lock-only UPDATE(updated_at)), `integration.operations` (column SELECT,
  UPDATE(request,semantic_key,updated_at)) and `live.claim_windows` (column SELECT, lock-only UPDATE(updated_at)).
  MCI02 holds back the dependent 0071 with 0064 (ledger +3) and leaves every other 0071 privilege to CRP02.
  It also records the grant `EXECUTE meta_inbox.lock_purgeable(uuid)` to the NOLOGIN `commerce_retention_writer` only (B27; meta-inbox-v1 amendment).
