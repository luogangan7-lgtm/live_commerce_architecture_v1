# Meta social consumer v1

Status: **FROZEN / PASS_LOCAL_MC01_07**. Independent design preflight and
the two ordering/family clarifications at `80e678f` passed with no open P0/P1/P2.
Implementation, populated upgrade, 530-test full regression and independent
MC01–07 verdict are recorded in the [bounded acceptance](../docs/implementation/2026-09-26-meta-social-consumer-acceptance.md).
Builds on the accepted
[durable inbox](meta-inbox-v1.md); not a public/provider or whole-SaaS gate.
Integrator owns migration `0029`, shared authority changes and this contract.

## Outcome and reuse

Consume a genuinely routed Meta job into durable, tenant/store-scoped social
facts: one conversation per exact scoped peer, inbound messages, and separate
comment version observations. Reuse pgx, River, the existing strict classifier
and `PayloadKeyring`. No network calls, SDK, extra broker, new service, generic
cross-channel conversation, or alternate commerce writer.

This is read-side materialization, not permission to send. It never renews a
message window, creates marketing consent, links a buyer identity, matches a
purchase claim, replies, changes inventory/order/payment, or touches webchat,
support or login sessions. Those remain explicit later domains and gates.

## Private projection

`projectSocial(context payloadContext, assetID, kind string, plaintext []byte)`
returns a private `{family, subjectKey string}` or fixed `ErrConsumerPayload`.
All formatting/JSON of that value and the worker is redacted.

1. Require an event-class validated context, valid asset and known kind.
2. Reuse `parseStrict`, `Verifier.change/message` and `emit` on the decrypted
   canonical bytes. Require exactly one non-quarantined event with exact asset,
   kind, event Key and PayloadHash equality. Do not duplicate the provider parser
   or infer a kind from arbitrary JSON keys. Canonical byte digest must match.
3. Exact family `comment` (`page_comment_add/edit/remove`, `instagram_comment`,
   `instagram_live_comment`): subjectKey is `tupleHash` of strings
   `["meta-social-comment/v1", AppID, Object, assetID, ExternalID]`.
4. Exact family `message` (`page_message`, `instagram_message`): subjectKey is tupleHash
   `["meta-social-peer/v1", AppID, Object, assetID, sender.id]`.
   The existing classifier requires inbound recipient=asset, non-asset sender,
   valid MID and no echo. Message identity remains the original event Key.

The hashes are scoped identity indexes, not anonymity or authority proofs.
Same text is not identity; same peer/ID under a different app/object/asset does
not merge. Unknown fields, attachments and original text remain in the complete
authenticated ciphertext, not silently discarded or flattened to text only.

## Authority and storage

Add NOLOGIN `commerce_meta_consumer`, inherited but not SET-able by one dedicated
login. Extend the existing exact authority validator/SQL allowlist; reject all
mixed commerce authority, predefined PostgreSQL role, owner/admin and SET ROLE
combinations in both directions. Do not give this role generic table SELECT,
River lifecycle writes, ingress, registrar or curator rights. Existing private
`commerce_meta_writer` gets only the additional column/table permissions and
fixed-search-path definer functions needed below; it remains non-login,
non-owner and non-BYPASSRLS. PUBLIC schema/function access revoked.

River continues to use a separately validated ordinary worker pool for job
lifecycle. It hands a job to a worker with the dedicated consumer pool. The
consumer cannot acquire authority by arbitrary tenant GUC or caller scope fields.

New `social` schema, FORCE RLS on all three tables, explicit private-writer
policies and no merchant/buyer/support read grants yet:

- `conversations`: UUID id, tenant/store composite FK, app_id/object/asset_id,
  peer_key (64 lowercase hex), next_seq initially 0, created_at. Unique exact
  `(tenant_id,store_id,app_id,object,asset_id,peer_key)`; no external actor ID.
- `messages`: event_id primary key and `(event_id,tenant_id,store_id)` FK to
  permanent inbox event; same-scope conversation FK; positive server_seq unique
  within conversation; original event Key and occurred_at/received_at; key_id,
  nonce, ciphertext. One message per durable source event.
- `comment_events`: event_id primary key and same-scope inbox FK; comment_key,
  original kind, occurred_at/received_at; key_id, nonce, ciphertext. Each edit,
  deletion and live comment remains a distinct observation. No invented DM.

Copy the already authenticated scoped ciphertext envelope from the stored event,
not from caller arguments. The immutable source event supplies its original
AAD context forever; no ciphertext is re-labeled or opened as another class.
This is a separate durable domain copy so short inbox-body retention cannot
erase the conversation. No raw-batch or quarantine ciphertext is copied.
Social retention/deletion policy and authorized reader are later explicit gates;
this copy must not be publicly mounted before those gates. No SQL plaintext.
Both fact tables persist `consumer_attempt` solely to verify the exact River
attempt again in the deferred guard; the job identity remains in the source
event. It is not a GUC ticket or caller-supplied tenant authority.

Comment observations do not guess a current platform snapshot from delivery
order or `created_time`; out-of-order add/edit/remove stay visible as separate
facts. A future authoritative snapshot reconciler must precede a claimed current
comment-state projection. Message server_seq is successful materialization order,
starting at 1, not receipt time or platform causal order. `occurred_at` always
comes from source event.occurred_at; `received_at` from source event.created_at.

## Fixed SQL interface and transaction

`meta_inbox.load_social_event(event uuid, job bigint, attempt integer)` returns
one row with columns in this exact order:
`outcome text, app_id text, object text, asset_id text, kind text, event_key text,
payload_hash text, tenant_id uuid, store_id uuid, route_id uuid, route_epoch bigint,
key_id text, nonce bytea, ciphertext bytea`.
Outcomes are `READY`, `ALREADY`, `REVIEWED`, `STALE`; only READY exposes the
envelope/context. Other row fields are NULL and must not be decoded. Invalid
identity/arguments are SQLSTATE 22023; unexpected storage remains retryable.

`meta_inbox.finish_social_event(event uuid, job bigint, attempt integer,
family text, subject_key text) -> void` is the sole projection writer.
Both functions authenticate original session_user through the shared validator.
No supplied tenant/store/route/envelope/plaintext/terminal reason is accepted.
The trusted Go consumer derives subject_key after AEAD+classifier validation;
SQL validates its shape/family, not a cryptographic proof of that derivation.
This does not claim to contain a fully compromised consumer with the keyring.
Family is exactly `message` or `comment`, matched to the kind sets above;
NULL, unknown and kind-mismatched values are rejected with SQLSTATE 22023.

For each job, in one bounded READ COMMITTED transaction:

1. Validate canonical event UUID, positive job ID/attempt, version=1 and exact
   persisted source event job identity. Only completed ROUTED primary events.
   Processed history with a matching durable social fact returns ALREADY before
   route/body lookup, even after body/job cleanup. Explicit curator terminal
   history returns REVIEWED with no projection or ciphertext.
2. For a new projection, lock active tenant -> store -> binding -> route in the
   existing admission order, then source event FOR UPDATE, then River job FOR
   SHARE. Re-read after waits. Require original scope, app/object/asset, binding
   identity/version and route epoch; enabled/current proof by clock_timestamp.
   Require exact River kind/queue/args/no unique_key, state=running and exact
   attempt. Hold locks through COMMIT; a rescued/stale attempt cannot commit.
3. Recheck terminal state after event lock. A revoked/expired/disabled/moved
   route returns STALE, without reading ciphertext or marking consumed. Missing
   or mismatched job/attempt fails without business effects.
4. READY returns only this event's scoped envelope and immutable AAD metadata.
   Open with the independent Meta keyring, then projectSocial. No network wait.
   Missing key, tag/hash/classifier failure leaves all source/domain state intact.
5. Finish repeats locked current authority/job/proof validation. Message family
   upserts and locks the exact scoped conversation, increments its sequence once,
   then inserts message with copied ciphertext. Comment family inserts only its
   comment observation. No cross-domain rows or extra River jobs are created.
6. Mark event `terminal_reason=processed`, with time/session actor/payload hash
   evidence and immutable audit action, only with the corresponding durable fact.
   A deferred INSERT guard checks one matching family fact, same-scope lineage,
   exact source envelope, processed evidence and final current route/proof/job
   attempt at COMMIT. Partial fact/terminal/sequence writes cannot commit.
7. Commit before returning success. Retry after ambiguous COMMIT observes
   ALREADY and changes nothing, including sequence and audit. A completed fact
   remains readable in its original scope even after a later revocation; that
   does not authorize any further processing or sends.

Load can be rolled back without writes. Finish independently checks its own
preconditions; no GUC, process-local cache or durable reservation ticket is a
substitute. Duplicate consumers serialize on event then conversation; no
reverse conversation->event lock acquisition. Existing curator/retention locks
must not deadlock with this order. Normal curator functions still cannot assert
`processed`; only this concrete projection can. Retention continues to require
source terminal evidence AND terminal/pruned job before source-body deletion.

## Go worker and errors

`NewConsumerWorker(ctx context.Context, pool *pgxpool.Pool,
keys *PayloadKeyring) (*ConsumerWorker,error)` validates and borrows the dedicated
pool/keyring; does not start River or expose arbitrary Batch processing.
Platform exposes `ValidateMetaConsumerPool(ctx context.Context, pool *pgxpool.Pool)
error`, following the existing Meta ingress validator's bounded safe-error style.
`ConsumerWorker` implements `river.Worker[inboxJobArgs]`, reusing the existing
fixed kind. Work checks nonnil job, ID/attempt, version, EventID and Row kind/queue.
Five-second operation context covers load, AEAD/classifier, finish and COMMIT;
LOCAL statement/lock/idle limits and independent two-second rollback.
Timeout returns five seconds. Fixed safe errors only, no SQL/DSN/payload details.

- Invalid job/SQL 22023: `river.JobCancel(ErrConsumerJob)`; no fact or terminal.
- STALE/REVIEWED: commit read-only TX, then JobCancel(ErrConsumerPolicy).
- ALREADY: commit and return nil. READY: finish+commit, then nil.
- Crypto/classifier and storage/commit errors: return fixed retryable errors;
  do not cancel as successfully consumed or authorize age-only purge.

Process runtime/CLI, production key loading, route OAuth proof, UI reads,
retention scheduling/alerts and real provider qualification remain separate next
work, not a reason to replace this actual consumer with an always-success mock.

## Required executable gates

| Gate | Actual required evidence |
| --- | --- |
| MC01 | Dedicated and mixed/SET/system/owner role negatives in Go and SQL; no direct social/raw/quarantine read or arbitrary scope write |
| MC02 | Classifier reuse, exact event/hash/kind/asset agreement; all seven kinds, stable peer/comment identities, Unicode/attachments and redacted errors |
| MC03 | Actual River+PG Page and IG messages/comments produce separate scoped facts; no webchat/identity/order/payment writes; decrypted domain copy equals source |
| MC04 | Concurrent same event and different messages/same peer; first seq=1, reverse consumption retains source timestamps while seq follows materialization; real rollback at each fact/terminal/audit write; rollback/replay never advances seq; lost response/replay and source purge preserve one fact |
| MC05 | Exact job/attempt and live row-lock tests; rescue/route disable/binding epoch/proof expiry while waiting or before COMMIT prevents materialization |
| MC06 | Missing key/tampered body/invalid payload and already-reviewed/stale event never becomes processed; pending body survives purge; processed source cleanup preserves social ciphertext/history |
| MC07 | Fresh/upgraded migration, prior MI gates, full PG/race/vet, independent source and evidence verdict; no fake provider/live/browser acceptance |

## Known limits and upgrade signals

No DM policy/window engine, public sending, attachments download, comment snapshot
fetch, social read UI or keyword-to-order automation yet. Out-of-order provider
observations remain source evidence, not inferred state. Add those concrete
contracts next; measured contention on a single hot conversation would justify
revisiting per-conversation sequencing. No performance-superiority claim.

## Amendment by claims-retention-purge-v1 (integrator, 2026-09-30, U08 merge)

Recorded from `contracts/claims-retention-purge-v1.md` §6 (FROZEN 2026-09-30); that file is the source of the rows.

- Clause 5: the social grants gain the §4 rows for NOLOGIN `commerce_retention_writer` on `social.conversations`,
  `social.messages`, `social.comment_events` (SELECT, DELETE, lock-only UPDATE(next_seq)/UPDATE(received_at)); the
  MIso/MC gates must still pass (no `commerce_meta_*` login reaches a retention role).
