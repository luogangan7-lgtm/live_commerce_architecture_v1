# Meta inbox v1 — trusted routing and durable admission

Status: **CONTRACT_FROZEN / DATABASE_LOCAL_GATES_PASSED_NOT_PUBLIC**. Actual local PG gates
are recorded in [durability acceptance](../docs/implementation/2026-09-26-meta-inbox-durability-acceptance.md), not provider acceptance. Builds on
[MWP01–05](meta-webhook-protocol-v1.md). No public route or provider activation
until this contract's real PostgreSQL gates and later operational gates pass.

## Decisions and trust boundary

- Reuse pgx, PostgreSQL, River `InsertTx`, standard-library AES-256-GCM. No
  second queue system/broker, generic conversation table or provider SDK;
  the dedicated Meta queue is a name inside the existing River system.
- `integration.bindings` is merchant self-assertion, not asset ownership.
  Add a trusted route keyed by configured `(app_id, object, asset_id)` plus
  global `(object, asset_id)` ownership. The first accepted tenant/store is
  immutable in v1, even across apps and revocations; an explicit future audited
  transfer protocol is required before cross-tenant/store reassignment.
- A trusted control-plane operation, unavailable to merchant/buyer/worker/ingress
  DB roles, activates a route only against a matching enabled Page/IG binding,
  its exact semantic version, active tenant/store and verified authorization
  evidence digest + expiry. The database cannot verify Meta OAuth evidence;
  only a verified OAuth/control-plane caller may assert it. Local fixtures use
  synthetic proof and cannot establish production qualification.
- Route epoch increments on disable/re-authorization. Old event scope, route
  epoch and binding version never change. No automatic rerouting or release of
  quarantined events after a merchant registers/reconnects an asset.
- No sending, LLM call, identity merge, message-window renewal or commerce
  state mutation while receiving a webhook. Site chat remains a separate domain.

## Authenticated bytes and Go API

Preserve `NewHandler(v, func(context.Context, Batch) error)` for existing callers
and tests. Internally it wraps a private raw-aware callback; raw bytes are the
owned buffer read and verified in the handler, never reconstructed JSON. The
new production-capable constructor is:

```go
func NewInbox(ctx context.Context, pool *pgxpool.Pool, keys *PayloadKeyring) (*Inbox, error)
func NewInboxHandler(v *Verifier, inbox *Inbox) (http.Handler, error)
func NewPayloadKeyring(activeID string, keys map[string][]byte) (*PayloadKeyring, error)
```

`Inbox` has no exported method accepting arbitrary `Batch`/raw pairs. Its private
commit is reachable from `NewInboxHandler` only after the existing verifier.
Constructor rejects nil/zero keyring, unsafe/closed pool, privileged/mixed login,
SET ROLE masquerade and DSN/current/session-user mismatch. It borrows the pool;
caller owns Close. It creates a producer-only River client (no worker startup).
Invalid config/storage errors are fixed safe errors, never connection details,
provider content, SQL parameter text or key material. Formatting/JSON of Inbox,
keyring and payload envelopes is redacted.

## Encryption sub-contract (independent of SQL interface)

Independently frozen against `183417f`; no open P0/P1/P2 in this subsection.
UUID validation reuses `command.ValidID` (lowercase 8-4-4-4-12 form, not a new
version/variant policy). This freeze is not an encryption implementation gate.

`PayloadKeyring` owns copies of 1..16 distinct nonzero 32-byte keys indexed by
`[A-Za-z0-9_-]{1,64}`; active ID must exist. No reused key under two IDs. Old keys
are retained for decryption after normal rotation; deleting a key is an explicit
operator retention decision, not an implicit fallback. No payment-keyring reuse.

Private `payloadContext` has fields:
`Class, ID, AppID, Object, BodyHash, EventKey, PayloadHash, TenantID, StoreID,
RouteID string; RouteEpoch int64`. Private `sealedPayload` has
`KeyID string; Nonce, Ciphertext []byte`. Methods:
`seal(payloadContext, []byte) (sealedPayload,error)` and
`open(payloadContext, sealedPayload) ([]byte,error)`.

- Class is exactly `raw`, `event` or `quarantine`. ID is canonical lowercase UUID;
  AppID is protocol digits; Object is page/instagram. SHA fields are lowercase
  64-hex; scope IDs use the existing canonical UUID rule.
- `raw`: BodyHash required, all event/scope/route fields empty/zero.
- `event`: EventKey and PayloadHash required, tenant/store/route IDs and positive
  RouteEpoch required; BodyHash empty. `quarantine`: EventKey/PayloadHash required,
  no tenant/store/route context, BodyHash empty. This includes conflicts.
- AEAD AAD is exactly the compact JSON array in this order:
  `["livecommerce/meta-payload/v1",Class,ID,AppID,Object,BodyHash,EventKey,
  PayloadHash,TenantID,StoreID,RouteID,RouteEpoch,KeyID]`.
  Fields are JSON strings except RouteEpoch, a JSON integer; include empty
  strings and zero fields (no omission), with no whitespace/newline. KeyID is
  the envelope key ID. Never delimiter-concatenate attacker-controlled values. Random 12-byte
  nonce for each seal, independent of body hashes, IDs and retries. Same plaintext
  must produce different ciphertext. Tag size 16. Seal plaintext bounds 1..4 MiB;
  raw additionally <=1 MiB. Open validates all context/key/nonce/cipher bounds
  before AEAD; plaintext cannot be returned on tag failure.
- Check `sha256(plaintext)` against BodyHash for raw, PayloadHash for event or
  quarantine before seal and after open. No empty-key fallback, nonce reuse,
  deterministic encryption, disk plaintext, SQL plaintext or stdout dumps.
- Actual encrypted normalized bytes are `Event.Payload`; quarantine preserves
  the complete protocol-designated context. The whole raw batch is not a tenant
  payload: it can contain different tenants. Persist it only in restricted
  short-retention storage inaccessible to tenant workers.

## Database layout and authority

Integrator owns migration number allocation (next currently 0028) and post-River
migration/ACL changes; never edit an applied migration. Proposed schemas:

| Storage | Permanent metadata / private body | Authority |
| --- | --- | --- |
| `meta_inbox.asset_owners`, `routes` | External asset -> immutable tenant/store; app route, binding/version, epoch, proof hash/expiry, enabled | Trusted control-plane mutation only; no merchant mutation |
| `meta_inbox.batches` | UUID, app/object/raw hash, exact expected unit count, finalized, timestamps; unique app/object/body hash | Ingress definer functions only |
| `meta_inbox.events` | UUID, app/object/event Key + PayloadHash, asset/kind, frozen route/scope/binding, disposition and job reference | No body; persistent event uniqueness independent of River retention |
| `meta_inbox.batch_events` | Every original batch ordinal -> event/version UUID; repeated units are still accounted | Ingress only; exact finalized count |
| `meta_private.raw_bodies` | Batch ID, key/nonce/cipher, expiry | Restricted, no generic runtime/worker SELECT |
| `meta_private.event_bodies` | Event ID + tenant/store composite FK, key/nonce/cipher, expiry | FORCE RLS; no generic runtime/worker ciphertext SELECT |
| `meta_private.quarantine_bodies` | Unknown/conflict/unsupported event ID, key/nonce/cipher, expiry | Restricted, no guessed tenant |

No external message/comment IDs or payload text in global routing/job metadata;
use protocol hashes/internal IDs. Raw/event timestamps are evidence only, never
ownership or consent proof. Retention eligibility defaults to 24h raw/quarantine
and 7d scoped event body; permanent minimal receipts survive body expiry. **Age
alone never permits purge**: a pending processing job or unresolved quarantine
retains its recoverable payload until consumed/reviewed, or a specifically
authorized audited terminal-retention outcome is recorded. Raw batch purge also
requires every linked event/version to have reached such a terminal state.
Overdue pending payloads require operations alerts and resolution, not silent
expiry. A bounded purge removes only eligible ciphertext, not dedupe/job metadata.
Retention scheduling and worker processing must be implemented before public
mount; an expiry column alone is not proof of deletion.

Add NOLOGIN `commerce_meta_ingress` and non-inheritable private definer owner
`commerce_meta_writer`. Control-plane route activation uses a separate
`commerce_meta_registrar` authority; explicit reviewed rejection/discard uses
`commerce_meta_curator`. Neither inherits the other or ingress, and neither can
read raw/scoped body ciphertext via general SELECT. These are narrow function
roles, not additional services. Dedicated ingress login has only ingress authority,
no generic runtime/worker/control-plane membership or SET privilege. Existing
pool validation must reject new mixed authority combinations in either direction.
Functions use fixed qualified names/search_path and reject unsafe session_user.
Writer has only exact table/column privileges and explicit RLS policies, not
schema/table ownership or BYPASSRLS. PUBLIC EXECUTE/USAGE revoked.
Registrar/curator stay NOLOGIN and unassigned outside isolated synthetic test
fixtures until actual verified OAuth / authorized review workflows exist. A
curator evidence hash alone is not a production review workflow or body reader.

Ingress may create only `meta_inbox_v1` jobs on fixed `meta_inbox` queue; args
exactly `{event_id:<internal UUID>,version:1}`. Minimal River insert permissions
plus DB admission guard prevent foreign job kind/queue/args injection. No broad
worker role can read ciphertext or invoke trusted route mutation. A later scoped
consumer must recheck the stored route/binding epoch before reading/decrypting.

## Atomic admission sequence

1. Existing HTTP verifies exact raw body, type/encoding and full batch limits.
   Begin one bounded transaction for the complete batch; defer bounded rollback
   with independent cleanup context. No external calls in this transaction.
2. Serialize permanent `(app,object,BodyHash)` identity. If already finalized,
   return the historical receipt without route lookup/new bodies/new jobs. After
   ambiguous COMMIT retry this same identity; no fresh idempotency key.
3. New receipt retains encrypted exact raw bytes. Sort event admission locks by
   canonical Key then PayloadHash to avoid cross-batch lock inversion; retain
   original ordinal mapping and all duplicates.
4. Check permanent `(app,object,Event.Key)` identity before mutable route lookup.
   Equal hash is historical duplicate, no second job or rehoming. Different
   hash is a durable restricted conflict version, no new business job; repeated
   conflict hash is also deduped. Lock the first-key decision across concurrency.
5. For new key: protocol quarantine stays restricted. Otherwise resolve exact
   trusted route, active tenant/store, binding identity/enabled/version and
   authorization expiry inside locked transaction. Untrusted/missing/expired/
   revoked routes become durable quarantine, never broadcast. Freeze accepted
   scope, binding version and route epoch. Recheck wall-clock validity after
   waits, not only before locking. No automatic cross-owner transfer.
6. Encrypt each newly admitted unit with its frozen context. Valid routed events
   get one River `InsertTx` with internal ID/version only and the fixed queue;
   quarantine gets no business-processing job. Store ciphertext and verify exact
   job args/kind/queue before completing the event. Every ordinal is linked.
7. Finalize only when exact unit count, raw body and all new event bodies/jobs
   exist. Deferred DB guards prevent partial prepared batches/events from
   committing even if a caller mistakenly skips finalize. Commit succeeds before
   handler may write 200. Any statement/encryption/job/commit error rolls back
   all effects and returns fixed 503; no partial batch slicing or ACK.

SQL function signatures and trust/lifecycle guards were independently reviewed
through `1d2d5d0` with no open P0/P1/P2. They are frozen for parallel SQL/service/
test implementation. Routes and receipt/event identity survive
ciphertext cleanup. Historical replay must neither depend on current binding nor
reanimate bodies/jobs after cleanup. No job backfill for this brand-new producer.

## SQL interface (integrator-owned)

All functions are schema-qualified, SECURITY DEFINER with fixed `pg_catalog`
search_path and PUBLIC EXECUTE revoked. The trusted control-plane functions are
not granted to ingress or any merchant/buyer/worker login. Ingress functions
verify session_user's exclusive dedicated authority, never a mutable tenant GUC.
Expected SQL errors use fixed classes (`22023` invalid, `42501` unauthorized,
`PT409` conflict), not provider content. Go still returns only a safe storage error.

| Function / result | Admission and mutation |
| --- | --- |
| `meta_inbox.activate_route(app text, object text, asset text, tenant uuid, store uuid, binding uuid, binding_version bigint, proof_hash text, proof_expires timestamptz, expected_epoch bigint) -> (route_id uuid, route_epoch bigint)` | Trusted control plane only. Exact local provider `facebook` for page / `instagram` for instagram, asset, active scope and binding/version. Create uses expected_epoch=0, replacement uses CAS; immutable owner/store/binding ID. Existing different owner always conflicts, including other app. Proof digest lowercase64hex, future expiry; audit each activation. |
| `meta_inbox.disable_route(route uuid, expected_epoch bigint) -> bigint` | Trusted control plane only; CAS disable increments epoch and appends metadata audit. No old event mutation. |
| `meta_inbox.begin_batch(app text, object text, body_hash text, unit_count int) -> (batch_id uuid, replay bool)` | Ingress only. Bound count 1..1000. Serialize app/object/raw hash; return finalized historical receipt before route lookup. Otherwise insert new unfinalized receipt owned by current transaction. |
| `meta_inbox.prepare_event(batch uuid, ordinal int, event_key text, payload_hash text, asset text, kind text, protocol_reason text, occurred_at timestamptz) -> (event_id uuid, needs_body bool, body_class text, tenant_id uuid, store_id uuid, route_id uuid, route_epoch bigint)` | Ingress only; new batch owned by this transaction, ordinal1..unit_count. Global first-key/version check before routing. Serialize Key, preserve one primary hash forever, represent other hashes as conflict versions. Return historical equal-hash identity without remapping. Every call inserts one exact ordinal link, even when event is repeated. New routable event returns class event; all other new units quarantine. |
| `meta_inbox.complete_event(batch uuid, event uuid, key_id text, nonce bytea, ciphertext bytea, job_id bigint) -> void` | Ingress only; event created in this transaction and linked to this new batch. Require the exact stored class/shape, ciphertext length17..4194320, 12-byte nonce and valid key ID. Routed event must reference existing exact meta_inbox_v1 job/queue/args; quarantine must have NULL job. Insert body and atomically mark completed. No update/re-encrypt of historical rows. |
| `meta_inbox.complete_batch(batch uuid, key_id text, nonce bytea, ciphertext bytea) -> void` | Ingress only; new receipt in this transaction. Raw length17..1048592, valid key/nonce; insert raw body, verify complete ordinal coverage and all events, then finalize. Commit guard still required. |
| `meta_inbox.record_terminal(event uuid, reason text, evidence_hash text) -> void` | Separate curator authority only, not route registrar or ingress. Fixed reason reviewed_rejected/retention_discarded requires explicit authorized review, evidence digest and immutable timestamp/actor audit; never called on receipt/ACK or inferred from River state. `processed` is reserved and rejected until a later narrow consumer function proves business completion. |
| `meta_inbox.purge_expired(limit int) -> int` | Trusted retention authority only; limit1..1000 total body rows, oldest eligible first, SKIP LOCKED. Recheck age and terminal evidence under lock. Any linked processing job must also be completed/cancelled/discarded or already pruned; raw body waits for all members. Preserve all metadata/audit/job references. |

Provider labels above are local adapter identifiers, not OAuth eligibility.
Current source/tests have only generic/mock bindings, not an established Meta
spelling to migrate. This contract chooses exactly `facebook` / `instagram`;
do not normalize unknown arbitrary identifiers or reinterpret existing mock rows.

`events` represents versions directly: unique `(app_id,object,event_key,payload_hash)`
plus a partial unique `(app_id,object,event_key) WHERE is_primary`; the first
version has `is_primary=true` whether routed or quarantined. Later changed hash
rows have `is_primary=false`, `disposition=QUARANTINED`, reason payload_conflict,
and never a job. Key-level transaction lock precedes first-version selection.
Historical equal-hash duplicates must also match stored kind/asset, otherwise
reject inconsistent metadata. No delete/update of key/hash/scope/routing columns.

Each receipt/event records its insertion transaction (`xid8`) privately. Only
that transaction may append/finalize its pending row. All partial-row INSERTs
have deferred constraint triggers which query their final state at COMMIT:
receipt finalized, exactly count ordinals1..N, every linked event completed;
new event has exactly the proper body table and exact required job linkage.
The guard is at insertion/initial completion, not a permanent foreign key to
short-retention bodies or prunable River jobs. Legitimate later body/job pruning
must not break historical receipts or duplicate ACK.

Lock ordering: batch identity, sorted Key/PayloadHash/original ordinal; per new
routable event scope rows (tenant then store), binding FOR SHARE, route FOR SHARE.
Route's binding ID/owner is immutable so preliminary identifier lookup cannot
silently redirect this lock path. Activators follow the same scope/binding/route
order. Concurrent revoke/version/active changes wait for admitted transaction;
all clock-based proof checks occur after waiting and again before finalization.
If a newly routed event becomes ineligible before finalization, fail the batch
and retry; do not ACK or silently change an already encrypted frozen context.

Post-River migration installs an exact producer admission constraint: only fixed
kind/queue/args, and job ID must link to exactly one completed routed inbox event
at COMMIT. An ingress-created orphan/foreign-kind job cannot commit. Existing
runtime/worker cannot insert this new producer kind or queue. Use deferred
checks to allow normal River InsertTx -> event completion order. Existing jobs
have no backfill. Producer-only `river.Client` uses explicit queue and no unique
period (persistent dedupe is in metadata); no processor for this queue starts yet.
The reserved-family guard must also cover UPDATE: existing generic jobs cannot
be changed into Meta kind/queue, and existing Meta kind/queue/args/id/unique_key
are immutable. Allow ordinary River lifecycle state/attempt/schedule updates,
not cross-family migration or changed event identity. This is required because
existing runtime roles can UPDATE(kind) and commerce_worker can UPDATE all River
columns. Gate both ingress INSERT attacks and runtime/worker UPDATE attacks.
Future consumers must treat an explicitly terminalized event as no-op/cancel,
not attempt processing its ciphertext. Curator terminalization is not itself a
River cancellation; until its job is terminal, ciphertext is not purgeable.
The consumer/no-op and actual authorized review workflow are public-mount gates.

Go uses a single max10s admission context with transaction-local statement/lock
timeouts; rollback gets its own bounded cleanup context. Cancellation/deadlock/
ambiguous commit returns safe 503 and relies on provider retry of the same body,
not internal unbounded retry. Raw buffer and normalized payloads remain scoped
to the request; none are retained by formatting, errors, logs or River args.

## Required actual acceptance gates

| Gate | Evidence required, never replaced by mock-only success |
| --- | --- |
| MI01 role / trust | Real PG dedicated ingress succeeds; merchant/worker/mixed/owner/SET ROLE fail; self-registered foreign asset cannot receive messages; cross-app conflicting owner fails |
| MI02 encryption | Independent AES-GCM round trip and tamper/AAD swaps; source key ownership/rotation; DB/log/job inspection no plaintext; cross-scope ciphertext inaccessible; exact raw byte recovery |
| MI03 atomic batch | Multi-tenant, known+unknown+duplicates+conflict batch all accounted; failures at each persistent stage roll back receipt/body/event/member/job; partial manual finalize commit rejected |
| MI04 dedupe | Concurrent same body and rebatched same MID produce one canonical event/job; changed payload quarantines; repeat after revoke/expiry/body purge/job prune returns history without rehome |
| MI05 routing fence | Revocation/binding version/tenant/store disable/expiry races and lock waits fail closed; immutable ownership; unknown keys durably quarantined |
| MI06 HTTP durability | Real handler+PG: forced commit failure no 200; committed response lost then retry adds nothing; queue args exact; existing MWP tests retained |
| MI07 retention | Bounded purge removes only expired AND terminal ciphertext; overdue jobs/unresolved quarantine retain recoverable data; raw batch waits for all members; ingress/registrar cannot terminalize, processed cannot be asserted by curator; permanent receipts/retries unchanged; no unauthorized raw/quarantine reader |
| Regression | Root isolated PG18 + complete Go race/vet; existing queue/role/payment/checkout gates unchanged; source+independent reviewer zero open P0/P1 |

Excluded from this increment, not from full SaaS delivery: actual OAuth/proof
issuance and operator activation UI, scoped event processing into Meta-domain
conversation/comment records, worker deployment, quotas/monitoring/retention
scheduling, public webhook mount, real Meta qualification, legal sending policy,
customer live broadcast behavior, remaining checkout/logistics/full SaaS gates.

## Rejected shortcuts and upgrade signals

- No binding self-assertion route, timestamp-based asset transfer, hash-only raw
  retention, one tenant's copy of a mixed-tenant batch, plaintext job payload,
or cleanup of permanent dedupe with River jobs.
- No replacement of existing transaction/queue framework; add a separate broker
  only after measured PG admission/queue capacity requires an approved ADR.
- No automatic asset transfer in v1; design a verified cutover and late-event
  policy when a real merchant transfer workflow is required.

## Amendment by claims-retention-purge-v1 (integrator, 2026-09-30, U08 merge)

Recorded from `contracts/claims-retention-purge-v1.md` §6 (FROZEN 2026-09-30); that file is the source of the rows.

- Clause 5 (ruling B27): function grants gain EXECUTE `meta_inbox.lock_purgeable(uuid)` to the NOLOGIN
  `commerce_retention_writer` only; "no execution grant for any login role" still holds.
