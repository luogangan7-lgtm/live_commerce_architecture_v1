-- 0071 claims retention purge and actor-level deletion (U08; contracts/claims-retention-purge-v1.md §2-§4,
-- FROZEN 2026-09-30; rulings R-6 (number released), X4 (W1 lapses when contract §10 holds), B23 = IR-U1 (D4), B26).
--
-- Owns: claims.retention_policy (the one policy row), claims.retention_log (append-only audit of counts and
-- replayable digests, never a person's id), bundles.purged_at + the reserved-label CHECK, the trigger that keeps a
-- de-identified bundle from ever getting a link again (RD8), the NOLOGIN roles commerce_retention_writer /
-- commerce_retention_job / commerce_retention_operator and the definers below.
--
-- Non-goals: no Meta deletion callback, no request queue, no K_actor rotation, no whole-bundle DELETE (claims.events
-- and claims.lines stay: statistics and source_event_id dedup), no meta_private/meta_inbox/order retention (0028 and
-- customers-billing own those), no River file (IR-4: commerce_worker already inserts main-river jobs, migrate.go), no
-- network. Nothing here purges anything by itself: enforced=false at migration (report-only, RD2).
--
-- Depends on: 0060 (claims.*, live.claim_windows), 0029 (social.*), 0064 (claims.meta_intake, integration.operations
-- meta.private_reply, live.claim_window_intervals), 0028 (meta_inbox.lock_purgeable), 0008 (integration.operations).
--
-- Callers (each function's COMMENT repeats its only caller): internal/retention.
--   claims-worker job pool (login lc_retention_job): claims.run_retention, claims.retention_status
--   operator CLI cmd/retention-admin (login lc_retention_operator, created by the runbook, never logins.tsv):
--     claims.run_retention, retention_status, erase_actor, set_retention_policy, replay_actor_erasures
--   apply_actor_erasure and links_not_purged have no EXECUTE grant (called by definers / fired as a trigger).
--
-- Security decisions (why each exists):
--  * The definers are platform-level: they read no scope GUC; every row they touch is chosen by the class rules or by
--    the one selector (c) argument, so a caller cannot widen a purge to other rows.
--  * Every definer pins search_path=pg_catalog, lock_timeout=2s (IR-5), fully qualifies relations, REVOKEs PUBLIC.
--  * Global lock order: advisory hashtextextended('claims-retention',0) -> claim_windows FOR SHARE -> bundles FOR NO
--    KEY UPDATE -> lines -> links -> meta_intake -> operations -> conversations -> messages/comment_events -> log.
--    The job takes SKIP LOCKED on every row it CHOOSES; C2 then updates/deletes lines and links of a bundle it already
--    holds FOR NO KEY UPDATE without SKIP LOCKED (every claims writer locks the bundle first, so those rows cannot be
--    held by anyone else; a stuck one costs 55P03 after 2 s and a River retry). Erase waits (55P03 after 2 s).
--    Replay mode (r2-close-retention): claims.replay_actor_erasures sets the transaction-local flag lc.retention_replay
--    so that apply_actor_erasure may also delete PENDING intake rows and redact non-terminal reply operations of an
--    erased actor (a restored snapshot must not keep them); erase_actor resets the flag first (RD5 hold applies).
--  * The reserved label pattern `purged-|erased-`+32 hex is enforced by CHECK so a merchant cannot pre-occupy the
--    (random) label a later purge assigns (contract §13 F1). D4/B23: a bundle may carry only ITS OWN `erased-<id hex>`
--    (customers.apply_erasure, 0078); that value is not random and not predictable by another bundle's merchant
--    because it embeds only the bundle's own id.

DO $$
BEGIN
 IF to_regclass('social.comment_events') IS NULL OR to_regclass('claims.links') IS NULL
  OR to_regclass('claims.meta_intake') IS NULL OR to_regclass('live.claim_window_intervals') IS NULL THEN
  RAISE EXCEPTION '0071 requires 0029, 0060 and 0064' USING ERRCODE='55000';
 END IF;
END $$;

CREATE ROLE commerce_retention_writer   NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
CREATE ROLE commerce_retention_job      NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
CREATE ROLE commerce_retention_operator NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
COMMENT ON ROLE commerce_retention_writer IS
 'U08 retention definer owner (migrations/0071). NOLOGIN; owns only the claims.* retention definers and the links_not_purged trigger function. No login may reach it (platform validatePoolAuthority). Non-goal: any other table access.';
COMMENT ON ROLE commerce_retention_job IS
 'U08 claims-worker retention pool (migrations/0071). NOLOGIN group; exactly one dedicated login lc_retention_job validated by platform.ValidateRetentionJobPool. EXECUTE run_retention and retention_status only; cannot erase, set policy or replay.';
COMMENT ON ROLE commerce_retention_operator IS
 'U08 operator CLI (migrations/0071). NOLOGIN group; login lc_retention_operator is created by the runbook after owner approval, never by deploy/postgres/logins.tsv, and its DSN never reaches the deploy host or CI (contract §10(5)). Used only by cmd/retention-admin.';

ALTER TABLE claims.bundles ADD COLUMN purged_at timestamptz;
ALTER TABLE claims.bundles ADD CONSTRAINT bundle_purged_unbound
 CHECK (purged_at IS NULL OR (owner_id IS NULL AND bound_at IS NULL));
-- RD1/F1/D4: reserved pattern; only a purged bundle, or a bundle's own id form, may carry it.
ALTER TABLE claims.bundles ADD CONSTRAINT bundle_label_reserved
 CHECK (purged_at IS NOT NULL OR label IS NULL OR label !~ '^(purged|erased)-[0-9a-f]{32}$'
  OR label = 'erased-'||replace(id::text,'-','')) NOT VALID;
DO $$
BEGIN
 -- B26: a pre-existing reserved-pattern label stops the migration (operator renames it; no auto-relabel of merchant data).
 IF EXISTS(SELECT 1 FROM claims.bundles b WHERE b.purged_at IS NULL AND b.label ~ '^(purged|erased)-[0-9a-f]{32}$'
   AND b.label <> 'erased-'||replace(b.id::text,'-','')) THEN
  RAISE EXCEPTION '0071: a claims.bundles label already matches the reserved purged-/erased- pattern; rename it first' USING ERRCODE='55000';
 END IF;
 ALTER TABLE claims.bundles VALIDATE CONSTRAINT bundle_label_reserved;
END $$;
CREATE INDEX claims_bundle_actor ON claims.bundles(actor_key) WHERE platform<>'manual' AND purged_at IS NULL;
CREATE INDEX claims_bundle_purge ON claims.bundles(tenant_id,store_id,session_id) WHERE purged_at IS NULL;
CREATE INDEX meta_intake_actor ON claims.meta_intake(actor_key);
CREATE INDEX meta_intake_retention ON claims.meta_intake(received_at) WHERE state<>'PENDING';
CREATE INDEX claims_link_expiry ON claims.links(expires_at);
CREATE INDEX social_comment_retention ON social.comment_events(received_at);
CREATE INDEX social_message_retention ON social.messages(received_at);

CREATE TABLE claims.retention_policy (
 id boolean PRIMARY KEY DEFAULT true CHECK (id),
 enforced boolean NOT NULL DEFAULT false,
 link_days   integer NOT NULL DEFAULT 7  CHECK (link_days   BETWEEN 1 AND 365),
 intake_days integer NOT NULL DEFAULT 30 CHECK (intake_days BETWEEN 8 AND 3650),
 claims_days integer NOT NULL DEFAULT 90 CHECK (claims_days BETWEEN 8 AND 3650),
 social_days integer NOT NULL DEFAULT 30 CHECK (social_days BETWEEN 8 AND 3650),
 CHECK (social_days <= intake_days),
 version bigint NOT NULL DEFAULT 1 CHECK (version>0),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_by name NOT NULL DEFAULT session_user);
INSERT INTO claims.retention_policy DEFAULT VALUES;

CREATE TABLE claims.retention_log (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 kind text NOT NULL CHECK (kind IN ('run','policy_set','actor_erased','replay')),
 request_id uuid,
 selector_digest bytea CHECK (octet_length(selector_digest)=32),
 actor_digest bytea CHECK (octet_length(actor_digest)=32),
 bundle_tenant uuid, bundle_store uuid, bundle_ref uuid,
 counts jsonb NOT NULL CHECK (jsonb_typeof(counts)='object' AND octet_length(counts::text)<=1024
  AND NOT jsonb_path_exists(counts,'strict $.* ? (@.type() != "number")')),
 executed_by name NOT NULL DEFAULT session_user,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK ((kind='actor_erased') = (request_id IS NOT NULL AND selector_digest IS NOT NULL)),
 CHECK (kind='actor_erased' OR (actor_digest IS NULL AND bundle_ref IS NULL AND request_id IS NULL AND selector_digest IS NULL)),
 CHECK (kind<>'actor_erased' OR ((actor_digest IS NOT NULL) <> (bundle_ref IS NOT NULL))),
 CHECK ((bundle_ref IS NULL) = (bundle_tenant IS NULL) AND (bundle_ref IS NULL) = (bundle_store IS NULL)));
CREATE UNIQUE INDEX retention_one_request ON claims.retention_log(request_id) WHERE kind='actor_erased';
CREATE INDEX retention_recent_runs ON claims.retention_log(created_at DESC) WHERE kind='run';

ALTER TABLE claims.retention_policy ENABLE ROW LEVEL SECURITY;
ALTER TABLE claims.retention_policy FORCE ROW LEVEL SECURITY;
ALTER TABLE claims.retention_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE claims.retention_log FORCE ROW LEVEL SECURITY;
REVOKE ALL ON claims.retention_policy, claims.retention_log FROM PUBLIC;

-- Full actor_erased tuple: T20 exports whole tuples, never bare digests (contract §13 F3).
CREATE TYPE claims.erasure_tombstone AS (request_id uuid, selector_digest bytea, actor_digest bytea,
 bundle_tenant uuid, bundle_store uuid, bundle_ref uuid);

-- ---------------------------------------------------------------------------------------
-- Privileges of commerce_retention_writer (§4, exact). "lock-only" = UPDATE(col) with a FOR UPDATE policy
-- USING(true) WITH CHECK(false): PostgreSQL needs an UPDATE privilege for FOR SHARE/UPDATE row locks and any real
-- update fails. Every policy is TO commerce_retention_writer only; the definer bodies are the control.
-- ---------------------------------------------------------------------------------------
GRANT USAGE ON SCHEMA claims, live, integration, social, meta_inbox TO commerce_retention_writer;

GRANT SELECT(tenant_id,store_id,id,session_id,platform,actor_key,label,owner_id,purged_at) ON claims.bundles TO commerce_retention_writer;
GRANT UPDATE(actor_key,label,owner_id,bound_at,purged_at,updated_at) ON claims.bundles TO commerce_retention_writer;
CREATE POLICY bundle_retention_read ON claims.bundles FOR SELECT TO commerce_retention_writer USING (true);
CREATE POLICY bundle_retention_update ON claims.bundles FOR UPDATE TO commerce_retention_writer
 USING (purged_at IS NULL) WITH CHECK (purged_at IS NOT NULL AND owner_id IS NULL);

GRANT SELECT(tenant_id,store_id,bundle_id) ON claims.lines TO commerce_retention_writer;
GRANT UPDATE(applied_version) ON claims.lines TO commerce_retention_writer;
CREATE POLICY line_retention_read ON claims.lines FOR SELECT TO commerce_retention_writer USING (true);
CREATE POLICY line_retention_update ON claims.lines FOR UPDATE TO commerce_retention_writer
 USING (true) WITH CHECK (applied_version IS NULL);

GRANT SELECT(tenant_id,store_id,bundle_id,expires_at) ON claims.links TO commerce_retention_writer;
GRANT DELETE ON claims.links TO commerce_retention_writer;
GRANT UPDATE(expires_at) ON claims.links TO commerce_retention_writer;
CREATE POLICY link_retention_read ON claims.links FOR SELECT TO commerce_retention_writer USING (true);
CREATE POLICY link_retention_delete ON claims.links FOR DELETE TO commerce_retention_writer USING (true);
CREATE POLICY link_retention_lock ON claims.links FOR UPDATE TO commerce_retention_writer USING (true) WITH CHECK (false);

GRANT SELECT(tenant_id,store_id,id,inbox_event_id,session_id,object,asset_id,comment_ref,actor_key,state,received_at)
 ON claims.meta_intake TO commerce_retention_writer;
GRANT DELETE ON claims.meta_intake TO commerce_retention_writer;
GRANT UPDATE(updated_at) ON claims.meta_intake TO commerce_retention_writer;
CREATE POLICY intake_retention_read ON claims.meta_intake FOR SELECT TO commerce_retention_writer USING (true);
CREATE POLICY intake_retention_delete ON claims.meta_intake FOR DELETE TO commerce_retention_writer USING (state<>'PENDING' OR coalesce(current_setting('lc.retention_replay',true),'')='on');
CREATE POLICY intake_retention_lock ON claims.meta_intake FOR UPDATE TO commerce_retention_writer USING (true) WITH CHECK (false);

GRANT SELECT(tenant_id,store_id,session_id,state,closed_at) ON live.claim_windows TO commerce_retention_writer;
GRANT UPDATE(updated_at) ON live.claim_windows TO commerce_retention_writer;
CREATE POLICY window_retention_read ON live.claim_windows FOR SELECT TO commerce_retention_writer USING (true);
CREATE POLICY window_retention_lock ON live.claim_windows FOR UPDATE TO commerce_retention_writer USING (true) WITH CHECK (false);

GRANT SELECT(id,tenant_id,store_id,action,state,request,created_at) ON integration.operations TO commerce_retention_writer;
GRANT UPDATE(request,semantic_key,updated_at) ON integration.operations TO commerce_retention_writer;
CREATE POLICY operation_retention_read ON integration.operations FOR SELECT TO commerce_retention_writer
 USING (action='meta.private_reply');
CREATE POLICY operation_retention_update ON integration.operations FOR UPDATE TO commerce_retention_writer
 USING (action='meta.private_reply' AND (state IN ('SUCCEEDED','FAILED_FINAL','CANCELLED','BLOCKED_POLICY','STALE_BINDING')
  OR coalesce(current_setting('lc.retention_replay',true),'')='on'))
 WITH CHECK (action='meta.private_reply' AND (state IN ('SUCCEEDED','FAILED_FINAL','CANCELLED','BLOCKED_POLICY','STALE_BINDING')
  OR coalesce(current_setting('lc.retention_replay',true),'')='on') AND NOT request ? 'comment_ref' AND semantic_key LIKE 'mpr-%');

GRANT SELECT, DELETE ON social.conversations, social.messages, social.comment_events TO commerce_retention_writer;
GRANT UPDATE(next_seq) ON social.conversations TO commerce_retention_writer;
GRANT UPDATE(received_at) ON social.messages, social.comment_events TO commerce_retention_writer;
CREATE POLICY conversation_retention_read ON social.conversations FOR SELECT TO commerce_retention_writer USING (true);
CREATE POLICY conversation_retention_delete ON social.conversations FOR DELETE TO commerce_retention_writer USING (true);
CREATE POLICY conversation_retention_lock ON social.conversations FOR UPDATE TO commerce_retention_writer USING (true) WITH CHECK (false);
CREATE POLICY message_retention_read ON social.messages FOR SELECT TO commerce_retention_writer USING (true);
CREATE POLICY message_retention_delete ON social.messages FOR DELETE TO commerce_retention_writer USING (true);
CREATE POLICY message_retention_lock ON social.messages FOR UPDATE TO commerce_retention_writer USING (true) WITH CHECK (false);
CREATE POLICY comment_retention_read ON social.comment_events FOR SELECT TO commerce_retention_writer USING (true);
CREATE POLICY comment_retention_delete ON social.comment_events FOR DELETE TO commerce_retention_writer USING (true);
CREATE POLICY comment_retention_lock ON social.comment_events FOR UPDATE TO commerce_retention_writer USING (true) WITH CHECK (false);

GRANT SELECT ON claims.retention_policy TO commerce_retention_writer;
GRANT UPDATE(enforced,link_days,intake_days,claims_days,social_days,version,updated_at,updated_by) ON claims.retention_policy TO commerce_retention_writer;
CREATE POLICY policy_retention_read ON claims.retention_policy FOR SELECT TO commerce_retention_writer USING (true);
CREATE POLICY policy_retention_update ON claims.retention_policy FOR UPDATE TO commerce_retention_writer USING (true) WITH CHECK (true);
GRANT SELECT, INSERT, DELETE ON claims.retention_log TO commerce_retention_writer;
CREATE POLICY log_retention_read ON claims.retention_log FOR SELECT TO commerce_retention_writer USING (true);
CREATE POLICY log_retention_insert ON claims.retention_log FOR INSERT TO commerce_retention_writer WITH CHECK (true);
CREATE POLICY log_retention_delete ON claims.retention_log FOR DELETE TO commerce_retention_writer USING (kind='run');

-- The predicate holds the terminal job row through the delete (0028). Its definer owner is not a login; the writer has
-- no login either, so 0028's "no execution grant for any login role" stays true (B27, clause 5).
GRANT EXECUTE ON FUNCTION meta_inbox.lock_purgeable(uuid) TO commerce_retention_writer;

GRANT USAGE ON SCHEMA claims TO commerce_retention_job, commerce_retention_operator;

-- ---------------------------------------------------------------------------------------
-- RD8: a de-identified bundle can never get a link again. Fires for issue_link (merchant), issue_system_link
-- (intake) and any UPDATE; the bundle row is locked FOR NO KEY UPDATE by every writer first, so it serializes
-- against the purge.
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION claims.links_not_purged() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog SET lock_timeout='2s' AS $$
BEGIN
 IF EXISTS(SELECT 1 FROM claims.bundles b WHERE b.tenant_id=NEW.tenant_id AND b.store_id=NEW.store_id
   AND b.id=NEW.bundle_id AND b.purged_at IS NOT NULL) THEN
  -- issue_link maps PT404 to "not found" (claims §3.3): same answer as an unknown bundle.
  RAISE EXCEPTION 'claim access unavailable' USING ERRCODE='PT404';
 END IF;
 RETURN NEW;
END $$;
ALTER FUNCTION claims.links_not_purged() OWNER TO commerce_retention_writer;
REVOKE ALL ON FUNCTION claims.links_not_purged() FROM PUBLIC;
CREATE TRIGGER links_not_purged BEFORE INSERT OR UPDATE ON claims.links
 FOR EACH ROW EXECUTE FUNCTION claims.links_not_purged();

-- ---------------------------------------------------------------------------------------
-- claims.run_retention (job, operator): one batch of the hourly purge. RD2 report-only unless enforced.
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION claims.run_retention(p_limit integer) RETURNS jsonb
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog SET lock_timeout='2s' AS $$
DECLARE pol claims.retention_policy%ROWTYPE; v_now timestamptz:=clock_timestamp();
 n_links integer:=0; n_bundles integer:=0; n_intake integer:=0; n_ops integer:=0; n_comments integer:=0;
 n_messages integer:=0; n_conv integer:=0; v_more boolean:=false; r record; v_counts jsonb;
BEGIN
 IF p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 1000 THEN
  RAISE EXCEPTION 'invalid retention limit' USING ERRCODE='22023';
 END IF;
 -- busy: another run (or an erasure) holds the single advisory key; the next hour retries.
 IF NOT pg_try_advisory_xact_lock(hashtextextended('claims-retention',0)) THEN
  RETURN jsonb_build_object('busy',1);
 END IF;
 -- The policy row is read under FOR SHARE so set_retention_policy (row FOR UPDATE) cannot flip mid-batch;
 -- a policy write in flight makes this batch busy, the next one sees the new value.
 SELECT * INTO pol FROM claims.retention_policy WHERE id FOR SHARE SKIP LOCKED;
 IF NOT FOUND THEN RETURN jsonb_build_object('busy',1); END IF;

 -- C1 links: expired long enough. Not enforced: count only.
 IF pol.enforced THEN
  WITH c AS (SELECT l.tenant_id,l.store_id,l.bundle_id FROM claims.links l
    WHERE l.expires_at < v_now - make_interval(days=>pol.link_days) ORDER BY l.expires_at LIMIT p_limit
    FOR UPDATE SKIP LOCKED),
  d AS (DELETE FROM claims.links k USING c WHERE k.tenant_id=c.tenant_id AND k.store_id=c.store_id
    AND k.bundle_id=c.bundle_id RETURNING 1)
  SELECT count(*) INTO n_links FROM d;
 ELSE
  SELECT count(*) INTO n_links FROM (SELECT 1 FROM claims.links l
   WHERE l.expires_at < v_now - make_interval(days=>pol.link_days) LIMIT p_limit) s;
 END IF;

 -- C2 claim identity: RD1 de-identify in place. Window CLOSED for claims_days (rechecked under FOR SHARE: a
 -- reopen in between skips the bundle); lock order windows -> bundles -> lines -> links.
 FOR r IN SELECT b.tenant_id,b.store_id,b.id,b.session_id,b.platform FROM claims.bundles b
   JOIN live.claim_windows w ON w.tenant_id=b.tenant_id AND w.store_id=b.store_id AND w.session_id=b.session_id
   WHERE b.purged_at IS NULL AND w.state='CLOSED' AND w.closed_at < v_now - make_interval(days=>pol.claims_days)
   ORDER BY w.closed_at, b.id LIMIT p_limit LOOP
  IF NOT pol.enforced THEN n_bundles:=n_bundles+1; CONTINUE; END IF;
  PERFORM 1 FROM live.claim_windows w WHERE w.tenant_id=r.tenant_id AND w.store_id=r.store_id AND w.session_id=r.session_id
   AND w.state='CLOSED' AND w.closed_at < v_now - make_interval(days=>pol.claims_days) FOR SHARE SKIP LOCKED;
  IF NOT FOUND THEN CONTINUE; END IF;
  PERFORM 1 FROM claims.bundles b WHERE b.tenant_id=r.tenant_id AND b.store_id=r.store_id AND b.id=r.id
   AND b.purged_at IS NULL FOR NO KEY UPDATE SKIP LOCKED;
  IF NOT FOUND THEN CONTINUE; END IF;
  UPDATE claims.lines l SET applied_version=NULL WHERE l.tenant_id=r.tenant_id AND l.store_id=r.store_id AND l.bundle_id=r.id;
  DELETE FROM claims.links k WHERE k.tenant_id=r.tenant_id AND k.store_id=r.store_id AND k.bundle_id=r.id;
  -- RD1: random key and random label (never derived from the bundle id), binding cleared, purged_at set.
  UPDATE claims.bundles b SET
    actor_key=replace(gen_random_uuid()::text,'-','')||replace(gen_random_uuid()::text,'-',''),
    label=CASE WHEN b.platform='manual' THEN 'purged-'||replace(gen_random_uuid()::text,'-','') END,
    owner_id=NULL, bound_at=NULL, purged_at=v_now, updated_at=v_now
   WHERE b.tenant_id=r.tenant_id AND b.store_id=r.store_id AND b.id=r.id;
  n_bundles:=n_bundles+1;
 END LOOP;

 -- C3 intake: terminal rows older than intake_days (>= 8, RD5).
 IF pol.enforced THEN
  WITH c AS (SELECT x.tenant_id,x.store_id,x.id FROM claims.meta_intake x
    WHERE x.state IN ('APPLIED','DROPPED','FAILED') AND x.received_at < v_now - make_interval(days=>pol.intake_days)
    ORDER BY x.received_at LIMIT p_limit FOR UPDATE SKIP LOCKED),
  d AS (DELETE FROM claims.meta_intake m USING c WHERE m.tenant_id=c.tenant_id AND m.store_id=c.store_id
    AND m.id=c.id AND m.state<>'PENDING' RETURNING 1)
  SELECT count(*) INTO n_intake FROM d;
 ELSE
  SELECT count(*) INTO n_intake FROM (SELECT 1 FROM claims.meta_intake x
   WHERE x.state IN ('APPLIED','DROPPED','FAILED') AND x.received_at < v_now - make_interval(days=>pol.intake_days)
   LIMIT p_limit) s;
 END IF;

 -- C4 reply ledger: redact comment_ref of terminal private-reply operations; request_hash keeps the original's hash.
 -- Non-terminal and UNKNOWN operations are untouched (reconcile still needs comment_ref, G07).
 IF pol.enforced THEN
  WITH c AS (SELECT o.id FROM integration.operations o
    WHERE o.action='meta.private_reply' AND o.state IN ('SUCCEEDED','FAILED_FINAL','CANCELLED','BLOCKED_POLICY','STALE_BINDING')
     AND o.created_at < v_now - make_interval(days=>pol.intake_days) AND o.request ? 'comment_ref'
    ORDER BY o.created_at LIMIT p_limit FOR UPDATE SKIP LOCKED),
  d AS (UPDATE integration.operations o SET request=(o.request - 'comment_ref') || '{"redacted":true}'::jsonb,
    semantic_key='mpr-purged:'||o.id::text, updated_at=v_now FROM c WHERE o.id=c.id RETURNING 1)
  SELECT count(*) INTO n_ops FROM d;
 ELSE
  SELECT count(*) INTO n_ops FROM (SELECT 1 FROM integration.operations o
   WHERE o.action='meta.private_reply' AND o.state IN ('SUCCEEDED','FAILED_FINAL','CANCELLED','BLOCKED_POLICY','STALE_BINDING')
    AND o.created_at < v_now - make_interval(days=>pol.intake_days) AND o.request ? 'comment_ref' LIMIT p_limit) s;
 END IF;

 -- C5 social ciphertext: old enough AND the inbox event and its River job are terminal (lock_purgeable holds the job
 -- row through the delete). A non-terminal consumer job keeps its row, else its retry would hit social_terminal XX000.
 -- ponytail: the scan reads at most 10*p_limit candidates per class so a few stuck rows do not starve the queue;
 -- more than 10*p_limit old non-terminal rows would stall it (raise the limit or fix the stuck jobs).
 FOR r IN SELECT c.event_id FROM social.comment_events c WHERE c.received_at < v_now - make_interval(days=>pol.social_days)
   ORDER BY c.received_at LIMIT p_limit*10 LOOP
  EXIT WHEN n_comments>=p_limit;
  IF meta_inbox.lock_purgeable(r.event_id) THEN
   IF pol.enforced THEN DELETE FROM social.comment_events c WHERE c.event_id=r.event_id; END IF;
   n_comments:=n_comments+1;
  END IF;
 END LOOP;
 FOR r IN SELECT m.event_id FROM social.messages m WHERE m.received_at < v_now - make_interval(days=>pol.social_days)
   ORDER BY m.received_at LIMIT p_limit*10 LOOP
  EXIT WHEN n_messages>=p_limit;
  IF meta_inbox.lock_purgeable(r.event_id) THEN
   IF pol.enforced THEN DELETE FROM social.messages m WHERE m.event_id=r.event_id; END IF;
   n_messages:=n_messages+1;
  END IF;
 END LOOP;

 -- C5b conversations without messages. Lock the row first, then re-check with a fresh statement: a consumer that
 -- committed a message in between wins (READ COMMITTED sees it) and one still holding next_seq makes us skip.
 FOR r IN SELECT v.id FROM social.conversations v WHERE v.created_at < v_now - make_interval(days=>pol.social_days)
   AND NOT EXISTS(SELECT 1 FROM social.messages m WHERE m.conversation_id=v.id) ORDER BY v.created_at LIMIT p_limit LOOP
  IF NOT pol.enforced THEN n_conv:=n_conv+1; CONTINUE; END IF;
  PERFORM 1 FROM social.conversations v WHERE v.id=r.id FOR UPDATE SKIP LOCKED;
  IF NOT FOUND THEN CONTINUE; END IF;
  IF EXISTS(SELECT 1 FROM social.messages m WHERE m.conversation_id=r.id) THEN CONTINUE; END IF;
  DELETE FROM social.conversations v WHERE v.id=r.id;
  n_conv:=n_conv+1;
 END LOOP;

 -- C6 run log: only in enforced mode, uncounted; erasure and policy rows are kept.
 IF pol.enforced THEN
  DELETE FROM claims.retention_log g WHERE g.id IN (SELECT x.id FROM claims.retention_log x
   WHERE x.kind='run' AND x.created_at < v_now - interval '400 days' ORDER BY x.created_at LIMIT p_limit);
 END IF;

 -- more=1 only when enforced: a report-only run removes nothing, so its backlog never shrinks and more=1 would make the
 -- job rerun 20 batches an hour and trip the runbook's last_run_more escalation (r2-close-retention).
 v_more:=pol.enforced AND greatest(n_links,n_bundles,n_intake,n_ops,n_comments,n_messages,n_conv)>=p_limit;
 v_counts:=jsonb_build_object('enforced',pol.enforced::int,'links',n_links,'bundles',n_bundles,'intake',n_intake,
  'operations',n_ops,'comment_events',n_comments,'messages',n_messages,'conversations',n_conv,'more',v_more::int);
 INSERT INTO claims.retention_log(kind,counts) VALUES('run',v_counts);
 RETURN v_counts;
END $$;
ALTER FUNCTION claims.run_retention(integer) OWNER TO commerce_retention_writer;
REVOKE ALL ON FUNCTION claims.run_retention(integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claims.run_retention(integer) TO commerce_retention_job, commerce_retention_operator;


-- ---------------------------------------------------------------------------------------
-- claims.apply_actor_erasure (internal): idempotent RD1 + deletions for one actor (or one manual bundle when p_bundle
-- is set). No EXECUTE grant: erase_actor and replay_actor_erasures (same owner) call it. It never applies the RD5
-- hold (erase evaluates it first; replay runs before services reopen). Social rows failing lock_purgeable are skipped
-- and counted social_deferred (C5 removes them later). Bundles are de-identified LAST because the actor_key that
-- selects them is what gets replaced.
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION claims.apply_actor_erasure(p_platform text, p_actor_key text, p_tenant uuid, p_store uuid,
 p_bundle uuid, p_peer_keys text[]) RETURNS jsonb
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog SET lock_timeout='2s' AS $$
DECLARE v_now timestamptz:=clock_timestamp(); v_single boolean:=p_bundle IS NOT NULL;
 n_bundles integer:=0; n_lines integer:=0; n_links integer:=0; n_intake integer:=0; n_comments integer:=0;
 n_ops integer:=0; n_messages integer:=0; n_conv integer:=0; n_defer integer:=0; r record;
 v_replay boolean:=coalesce(current_setting('lc.retention_replay',true),'')='on';
BEGIN
 IF (v_single AND (p_tenant IS NULL OR p_store IS NULL OR p_actor_key IS NOT NULL))
  OR (NOT v_single AND (p_platform IS NULL OR p_platform NOT IN ('facebook','instagram')
   OR p_actor_key IS NULL OR p_actor_key !~ '^[0-9a-f]{64}$'))
  OR (p_peer_keys IS NOT NULL AND (v_single OR array_ndims(p_peer_keys)<>1 OR cardinality(p_peer_keys)>8
   OR EXISTS(SELECT 1 FROM unnest(p_peer_keys) x WHERE x IS NULL OR x !~ '^[0-9a-f]{64}$'))) THEN
  RAISE EXCEPTION 'invalid actor erasure' USING ERRCODE='22023';
 END IF;
 -- Lock order: windows FOR SHARE (a reopen waits) -> bundles FOR NO KEY UPDATE (fixed order, blocking: the caller
 -- holds the advisory key and lock_timeout turns a stuck row into 55P03).
 PERFORM 1 FROM live.claim_windows w WHERE (w.tenant_id,w.store_id,w.session_id) IN (
   SELECT b.tenant_id,b.store_id,b.session_id FROM claims.bundles b WHERE b.purged_at IS NULL AND
    CASE WHEN v_single THEN b.tenant_id=p_tenant AND b.store_id=p_store AND b.id=p_bundle
     ELSE b.platform=p_platform AND b.actor_key=p_actor_key END)
  ORDER BY w.tenant_id,w.store_id,w.session_id FOR SHARE;
 PERFORM 1 FROM claims.bundles b WHERE b.purged_at IS NULL AND
   CASE WHEN v_single THEN b.tenant_id=p_tenant AND b.store_id=p_store AND b.id=p_bundle
    ELSE b.platform=p_platform AND b.actor_key=p_actor_key END
  ORDER BY b.tenant_id,b.store_id,b.id FOR NO KEY UPDATE;

 UPDATE claims.lines l SET applied_version=NULL FROM claims.bundles b
  WHERE l.tenant_id=b.tenant_id AND l.store_id=b.store_id AND l.bundle_id=b.id AND b.purged_at IS NULL AND
   CASE WHEN v_single THEN b.tenant_id=p_tenant AND b.store_id=p_store AND b.id=p_bundle
    ELSE b.platform=p_platform AND b.actor_key=p_actor_key END;
 GET DIAGNOSTICS n_lines=ROW_COUNT;
 DELETE FROM claims.links k USING claims.bundles b
  WHERE k.tenant_id=b.tenant_id AND k.store_id=b.store_id AND k.bundle_id=b.id AND b.purged_at IS NULL AND
   CASE WHEN v_single THEN b.tenant_id=p_tenant AND b.store_id=p_store AND b.id=p_bundle
    ELSE b.platform=p_platform AND b.actor_key=p_actor_key END;
 GET DIAGNOSTICS n_links=ROW_COUNT;

 IF NOT v_single THEN
  -- Every add/edit/remove of the actor's comments: comment_key is shared by the events of one comment, and the actor's
  -- intake rows name one event of each comment (a comment id maps to one sender).
  FOR r IN SELECT ce.event_id FROM social.comment_events ce WHERE (ce.tenant_id,ce.store_id,ce.comment_key) IN (
    SELECT e.tenant_id,e.store_id,e.comment_key FROM social.comment_events e WHERE e.event_id IN (
     SELECT i.inbox_event_id FROM claims.meta_intake i WHERE i.actor_key=p_actor_key)) LOOP
   IF meta_inbox.lock_purgeable(r.event_id) THEN
    DELETE FROM social.comment_events ce WHERE ce.event_id=r.event_id;
    n_comments:=n_comments+1;
   ELSE
    n_defer:=n_defer+1;
   END IF;
  END LOOP;
  -- replay (restore): a PENDING row of an erased actor goes too, else the consumer would re-claim and reply to them.
  DELETE FROM claims.meta_intake m WHERE m.actor_key=p_actor_key AND (m.state<>'PENDING' OR v_replay);
  GET DIAGNOSTICS n_intake=ROW_COUNT;
  -- C4: terminal private-reply operations of the bundles; non-terminal ones were a hold (RD5) or, in replay, are redacted
  -- too and keep their state: the adapter then fails pre-send (errBadRequest, zero calls) and the dispatcher records UNKNOWN.
  UPDATE integration.operations o SET request=(o.request - 'comment_ref') || '{"redacted":true}'::jsonb,
    semantic_key='mpr-purged:'||o.id::text, updated_at=v_now FROM claims.bundles b
   WHERE o.action='meta.private_reply' AND (v_replay OR o.state IN ('SUCCEEDED','FAILED_FINAL','CANCELLED','BLOCKED_POLICY','STALE_BINDING'))
    AND o.request ? 'comment_ref' AND o.tenant_id=b.tenant_id AND o.store_id=b.store_id
    AND o.request->>'bundle_id'=b.id::text AND b.purged_at IS NULL AND b.platform=p_platform AND b.actor_key=p_actor_key;
  GET DIAGNOSTICS n_ops=ROW_COUNT;
  IF p_peer_keys IS NOT NULL AND cardinality(p_peer_keys)>0 THEN
   PERFORM 1 FROM social.conversations c WHERE c.peer_key=ANY(p_peer_keys) ORDER BY c.id FOR UPDATE;
   FOR r IN SELECT m.event_id FROM social.messages m JOIN social.conversations c ON c.id=m.conversation_id
     WHERE c.peer_key=ANY(p_peer_keys) LOOP
    IF meta_inbox.lock_purgeable(r.event_id) THEN
     DELETE FROM social.messages m WHERE m.event_id=r.event_id;
     n_messages:=n_messages+1;
    ELSE
     n_defer:=n_defer+1;
    END IF;
   END LOOP;
   DELETE FROM social.conversations c WHERE c.peer_key=ANY(p_peer_keys)
    AND NOT EXISTS(SELECT 1 FROM social.messages m WHERE m.conversation_id=c.id);
   GET DIAGNOSTICS n_conv=ROW_COUNT;
  END IF;
 END IF;

 -- RD1: random key/label (never derived from the bundle id), binding cleared, purged_at set; label only for manual.
 UPDATE claims.bundles b SET
   actor_key=replace(gen_random_uuid()::text,'-','')||replace(gen_random_uuid()::text,'-',''),
   label=CASE WHEN b.platform='manual' THEN 'erased-'||replace(gen_random_uuid()::text,'-','') END,
   owner_id=NULL, bound_at=NULL, purged_at=v_now, updated_at=v_now
  WHERE b.purged_at IS NULL AND
   CASE WHEN v_single THEN b.tenant_id=p_tenant AND b.store_id=p_store AND b.id=p_bundle
    ELSE b.platform=p_platform AND b.actor_key=p_actor_key END;
 GET DIAGNOSTICS n_bundles=ROW_COUNT;
 RETURN jsonb_build_object('bundles',n_bundles,'lines',n_lines,'links',n_links,'intake',n_intake,
  'comment_events',n_comments,'operations',n_ops,'messages',n_messages,'conversations',n_conv)
  || CASE WHEN n_defer>0 THEN jsonb_build_object('social_deferred',n_defer) ELSE '{}'::jsonb END;
END $$;
ALTER FUNCTION claims.apply_actor_erasure(text,text,uuid,uuid,uuid,text[]) OWNER TO commerce_retention_writer;
REVOKE ALL ON FUNCTION claims.apply_actor_erasure(text,text,uuid,uuid,uuid,text[]) FROM PUBLIC;

-- ---------------------------------------------------------------------------------------
-- claims.erase_actor (operator): synchronous erasure of one actor / one manual bundle (RD4), one tx, RD5 hold.
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION claims.erase_actor(p_request uuid, p_object text, p_asset text, p_actor_key text, p_comment_ref text,
 p_tenant uuid, p_store uuid, p_bundle uuid, p_peer_keys text[]) RETURNS jsonb
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog SET lock_timeout='2s' AS $$
DECLARE v_a boolean:=p_actor_key IS NOT NULL; v_b boolean:=p_comment_ref IS NOT NULL; v_c boolean:=p_bundle IS NOT NULL;
 v_canon text; v_digest bytea; v_old record; r record; v_platform text; v_key text; v_single boolean:=false;
 v_tenant uuid; v_store uuid; v_bundle uuid; v_now timestamptz:=clock_timestamp(); v_hold boolean:=false;
 v_retry timestamptz; v_last timestamptz; v_pending boolean; v_recent boolean; v_counts jsonb; v_sum bigint;
BEGIN
 -- Exactly one selector (a) sender key, (b) comment ref, (c) bundle; each with only its own arguments.
 IF p_request IS NULL OR (v_a::int+v_b::int+v_c::int)<>1
  OR (NOT v_c AND (p_object IS NULL OR p_object NOT IN ('page','instagram') OR p_asset IS NULL OR p_asset !~ '^[0-9]{1,40}$'
   OR p_tenant IS NOT NULL OR p_store IS NOT NULL))
  OR (v_a AND (p_actor_key !~ '^[0-9a-f]{64}$' OR (p_peer_keys IS NOT NULL AND (array_ndims(p_peer_keys)<>1
   OR cardinality(p_peer_keys)>8 OR EXISTS(SELECT 1 FROM unnest(p_peer_keys) x WHERE x IS NULL OR x !~ '^[0-9a-f]{64}$')))))
  OR (v_b AND (p_comment_ref !~ '^[0-9_]{1,80}$' OR p_peer_keys IS NOT NULL))
  OR (v_c AND (p_tenant IS NULL OR p_store IS NULL OR p_object IS NOT NULL OR p_asset IS NOT NULL OR p_peer_keys IS NOT NULL)) THEN
  RAISE EXCEPTION 'invalid actor erasure' USING ERRCODE='22023';
 END IF;
 -- The replay flag is transaction-local and settable by any session: erase always runs with the RD5 hold semantics.
 PERFORM set_config('lc.retention_replay','off',true);
 -- busy: the advisory wait is bounded by lock_timeout (55P03); the operator re-runs (a repeat is idempotent by request id).
 PERFORM pg_advisory_xact_lock(hashtextextended('claims-retention',0));
 v_canon:=CASE WHEN v_a THEN 'a|'||p_object||'|'||p_asset||'|'||encode(sha256(convert_to(p_actor_key,'UTF8')),'hex')
  WHEN v_b THEN 'b|'||p_object||'|'||p_asset||'|'||encode(sha256(convert_to(p_comment_ref,'UTF8')),'hex')
  ELSE 'c|'||p_bundle::text END;
 v_digest:=sha256(convert_to(v_canon,'UTF8'));
 SELECT g.selector_digest,g.counts INTO v_old FROM claims.retention_log g WHERE g.kind='actor_erased' AND g.request_id=p_request;
 IF FOUND THEN
  IF v_old.selector_digest=v_digest THEN RETURN v_old.counts || '{"replayed":1}'::jsonb; END IF;
  RAISE EXCEPTION 'erasure request conflict' USING ERRCODE='PT409';
 END IF;

 -- Resolve the selector to one actor key (any tenant/store: the key embeds object and asset) or one manual bundle.
 IF v_a THEN
  v_platform:=CASE p_object WHEN 'page' THEN 'facebook' ELSE 'instagram' END; v_key:=p_actor_key;
 ELSIF v_b THEN
  -- platform is not in the writer's column grant on meta_intake (contract §4); it is a function of object.
  SELECT i.actor_key,CASE i.object WHEN 'page' THEN 'facebook' ELSE 'instagram' END AS platform INTO r FROM claims.meta_intake i
   WHERE i.object=p_object AND i.asset_id=p_asset AND i.comment_ref=p_comment_ref;
  IF NOT FOUND THEN RAISE EXCEPTION 'erasure target not found' USING ERRCODE='PT404'; END IF;
  v_platform:=r.platform; v_key:=r.actor_key;
 ELSE
  SELECT b.platform,b.actor_key INTO r FROM claims.bundles b
   WHERE b.tenant_id=p_tenant AND b.store_id=p_store AND b.id=p_bundle AND b.purged_at IS NULL;
  IF NOT FOUND THEN RAISE EXCEPTION 'erasure target not found' USING ERRCODE='PT404'; END IF;
  IF r.platform='manual' THEN
   v_single:=true; v_tenant:=p_tenant; v_store:=p_store; v_bundle:=p_bundle;
  ELSE
   v_platform:=r.platform; v_key:=r.actor_key;
  END IF;
 END IF;

 -- Locks first (windows FOR SHARE, bundles FOR NO KEY UPDATE), then the hold is evaluated under them (RD5).
 PERFORM 1 FROM live.claim_windows w WHERE (w.tenant_id,w.store_id,w.session_id) IN (
   SELECT b.tenant_id,b.store_id,b.session_id FROM claims.bundles b WHERE b.purged_at IS NULL AND
    CASE WHEN v_single THEN b.tenant_id=v_tenant AND b.store_id=v_store AND b.id=v_bundle
     ELSE b.platform=v_platform AND b.actor_key=v_key END)
  ORDER BY w.tenant_id,w.store_id,w.session_id FOR SHARE;
 PERFORM 1 FROM claims.bundles b WHERE b.purged_at IS NULL AND
   CASE WHEN v_single THEN b.tenant_id=v_tenant AND b.store_id=v_store AND b.id=v_bundle
    ELSE b.platform=v_platform AND b.actor_key=v_key END
  ORDER BY b.tenant_id,b.store_id,b.id FOR NO KEY UPDATE;

 IF EXISTS(SELECT 1 FROM claims.bundles b JOIN live.claim_windows w ON w.tenant_id=b.tenant_id AND w.store_id=b.store_id
   AND w.session_id=b.session_id WHERE w.state='OPEN' AND b.purged_at IS NULL AND
   CASE WHEN v_single THEN b.tenant_id=v_tenant AND b.store_id=v_store AND b.id=v_bundle
    ELSE b.platform=v_platform AND b.actor_key=v_key END) THEN
  v_hold:=true; v_retry:=greatest(coalesce(v_retry,v_now),v_now+interval '1 hour');
 END IF;
 IF NOT v_single THEN
  SELECT max(x.received_at), bool_or(x.state='PENDING'), bool_or(x.received_at > v_now-interval '8 days')
   INTO v_last, v_pending, v_recent FROM claims.meta_intake x WHERE x.actor_key=v_key;
  IF coalesce(v_pending,false) THEN v_hold:=true; v_retry:=greatest(coalesce(v_retry,v_now),v_now+interval '1 hour'); END IF;
  -- IR-6: 8 days = Meta's 7-day private-reply window + 1 day of webhook redelivery; deleting intake earlier would
  -- let a re-delivered comment re-claim and re-reply.
  IF coalesce(v_recent,false) THEN v_hold:=true; v_retry:=greatest(coalesce(v_retry,v_now),v_last+interval '8 days'); END IF;
  IF EXISTS(SELECT 1 FROM integration.operations o JOIN claims.bundles b ON o.tenant_id=b.tenant_id AND o.store_id=b.store_id
    AND o.request->>'bundle_id'=b.id::text WHERE o.action='meta.private_reply'
    AND o.state NOT IN ('SUCCEEDED','FAILED_FINAL','CANCELLED','BLOCKED_POLICY','STALE_BINDING')
    AND b.purged_at IS NULL AND b.platform=v_platform AND b.actor_key=v_key) THEN
   v_hold:=true; v_retry:=greatest(coalesce(v_retry,v_now),v_now+interval '1 hour');
  END IF;
  -- A social row whose inbox job is not terminal would make its consumer retry hit social_terminal XX000 (F5).
  FOR r IN SELECT ce.event_id FROM social.comment_events ce WHERE (ce.tenant_id,ce.store_id,ce.comment_key) IN (
    SELECT e.tenant_id,e.store_id,e.comment_key FROM social.comment_events e WHERE e.event_id IN (
     SELECT i.inbox_event_id FROM claims.meta_intake i WHERE i.actor_key=v_key)) LOOP
   IF NOT meta_inbox.lock_purgeable(r.event_id) THEN
    v_hold:=true; v_retry:=greatest(coalesce(v_retry,v_now),v_now+interval '1 hour');
   END IF;
  END LOOP;
  IF p_peer_keys IS NOT NULL AND cardinality(p_peer_keys)>0 THEN
   FOR r IN SELECT m.event_id FROM social.messages m JOIN social.conversations c ON c.id=m.conversation_id
     WHERE c.peer_key=ANY(p_peer_keys) LOOP
    IF NOT meta_inbox.lock_purgeable(r.event_id) THEN
     v_hold:=true; v_retry:=greatest(coalesce(v_retry,v_now),v_now+interval '1 hour');
    END IF;
   END LOOP;
  END IF;
 END IF;
 IF v_hold THEN
  -- held: nothing is written (no log row); the operator retries after retry_after.
  RETURN jsonb_build_object('held',1,'retry_after',floor(extract(epoch FROM v_retry))::bigint);
 END IF;

 -- claims.apply_actor_erasure: RD1 de-identify + deletions, same tx as the hold check and the log row.
 v_counts:=claims.apply_actor_erasure(v_platform,v_key,v_tenant,v_store,v_bundle,p_peer_keys);
 SELECT sum(e.value::bigint) INTO v_sum FROM jsonb_each_text(v_counts) e;
 IF coalesce(v_sum,0)=0 THEN
  RAISE EXCEPTION 'erasure target not found' USING ERRCODE='PT404';
 END IF;
 v_counts:=v_counts - 'social_deferred';
 INSERT INTO claims.retention_log(kind,request_id,selector_digest,actor_digest,bundle_tenant,bundle_store,bundle_ref,counts)
  VALUES('actor_erased',p_request,v_digest,CASE WHEN NOT v_single THEN sha256(convert_to(v_key,'UTF8')) END,
   v_tenant,v_store,v_bundle,v_counts);
 RETURN v_counts;
END $$;
ALTER FUNCTION claims.erase_actor(uuid,text,text,text,text,uuid,uuid,uuid,text[]) OWNER TO commerce_retention_writer;
REVOKE ALL ON FUNCTION claims.erase_actor(uuid,text,text,text,text,uuid,uuid,uuid,text[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claims.erase_actor(uuid,text,text,text,text,uuid,uuid,uuid,text[]) TO commerce_retention_operator;

-- ---------------------------------------------------------------------------------------
-- claims.set_retention_policy (operator): CAS on version; the one audited way to change periods or enforcement.
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION claims.set_retention_policy(p_expected_version bigint, p_enforced boolean, p_link integer, p_intake integer,
 p_claims integer, p_social integer) RETURNS bigint
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog SET lock_timeout='2s' AS $$
DECLARE v_version bigint;
BEGIN
 -- The table CHECKs are the bounds; they are repeated here so a bad value is 22023 (usage), not 23514.
 IF p_expected_version IS NULL OR p_expected_version<=0 OR p_enforced IS NULL
  OR p_link IS NULL OR p_link NOT BETWEEN 1 AND 365
  OR p_intake IS NULL OR p_intake NOT BETWEEN 8 AND 3650
  OR p_claims IS NULL OR p_claims NOT BETWEEN 8 AND 3650
  OR p_social IS NULL OR p_social NOT BETWEEN 8 AND 3650 OR p_social>p_intake THEN
  RAISE EXCEPTION 'invalid retention policy' USING ERRCODE='22023';
 END IF;
 SELECT p.version INTO v_version FROM claims.retention_policy p WHERE p.id FOR UPDATE;
 IF v_version IS DISTINCT FROM p_expected_version THEN
  RAISE EXCEPTION 'retention policy version changed' USING ERRCODE='PT409';
 END IF;
 UPDATE claims.retention_policy p SET enforced=p_enforced, link_days=p_link, intake_days=p_intake, claims_days=p_claims,
  social_days=p_social, version=v_version+1, updated_at=clock_timestamp(), updated_by=session_user WHERE p.id;
 INSERT INTO claims.retention_log(kind,counts) VALUES('policy_set',jsonb_build_object('enforced',p_enforced::int,
  'link_days',p_link,'intake_days',p_intake,'claims_days',p_claims,'social_days',p_social,'version',v_version+1));
 RETURN v_version+1;
END $$;
ALTER FUNCTION claims.set_retention_policy(bigint,boolean,integer,integer,integer,integer) OWNER TO commerce_retention_writer;
REVOKE ALL ON FUNCTION claims.set_retention_policy(bigint,boolean,integer,integer,integer,integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claims.set_retention_policy(bigint,boolean,integer,integer,integer,integer) TO commerce_retention_operator;

-- ---------------------------------------------------------------------------------------
-- claims.retention_status (job, operator): numbers only; smoke parses enforced and last_run_unix.
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION claims.retention_status() RETURNS jsonb
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog SET lock_timeout='2s' AS $$
 SELECT jsonb_build_object('enforced',p.enforced::int,'version',p.version,'link_days',p.link_days,
  'intake_days',p.intake_days,'claims_days',p.claims_days,'social_days',p.social_days,
  'last_run_unix',coalesce((SELECT floor(extract(epoch FROM g.created_at))::bigint FROM claims.retention_log g
    WHERE g.kind='run' ORDER BY g.created_at DESC LIMIT 1),0),
  'last_run_more',coalesce((SELECT (g.counts->>'more')::integer FROM claims.retention_log g
    WHERE g.kind='run' ORDER BY g.created_at DESC LIMIT 1),0))
 FROM claims.retention_policy p WHERE p.id
$$;
ALTER FUNCTION claims.retention_status() OWNER TO commerce_retention_writer;
REVOKE ALL ON FUNCTION claims.retention_status() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claims.retention_status() TO commerce_retention_job, commerce_retention_operator;

-- ---------------------------------------------------------------------------------------
-- claims.replay_actor_erasures (operator): restore replay (架构 §21.4). Applies every actor_erased tuple (from the log,
-- or the supplied external list, inserted first) without the RD5 hold: services are not reopened yet.
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION claims.replay_actor_erasures(p_tombstones claims.erasure_tombstone[] DEFAULT NULL) RETURNS integer
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog SET lock_timeout='2s' AS $$
DECLARE v_ts claims.erasure_tombstone[]; t claims.erasure_tombstone; r record; v_digests bytea[]; v_inserted integer:=0;
 v_c jsonb; v_sum jsonb:='{}'::jsonb; k text;
 v_keys text[]:=ARRAY['bundles','lines','links','intake','comment_events','operations','messages','conversations','social_deferred'];
BEGIN
 IF p_tombstones IS NOT NULL AND (cardinality(p_tombstones)>10000 OR (cardinality(p_tombstones)>0 AND array_ndims(p_tombstones)<>1)
   OR EXISTS(SELECT 1 FROM unnest(p_tombstones) x WHERE x IS NULL OR x.request_id IS NULL OR x.selector_digest IS NULL
    OR octet_length(x.selector_digest)<>32
    OR (x.actor_digest IS NULL) = (x.bundle_ref IS NULL)
    OR (x.actor_digest IS NOT NULL AND octet_length(x.actor_digest)<>32)
    OR (x.bundle_ref IS NULL) <> (x.bundle_tenant IS NULL) OR (x.bundle_ref IS NULL) <> (x.bundle_store IS NULL))) THEN
  RAISE EXCEPTION 'invalid erasure tombstones' USING ERRCODE='22023';
 END IF;
 PERFORM pg_advisory_xact_lock(hashtextextended('claims-retention',0));
 -- Replay mode for apply_actor_erasure and the §4 policies, transaction-local, reset below.
 PERFORM set_config('lc.retention_replay','on',true);
 IF p_tombstones IS NOT NULL THEN
  -- A missing tombstone is inserted as an actor_erased row (counts {}); an existing one is not duplicated.
  INSERT INTO claims.retention_log(kind,request_id,selector_digest,actor_digest,bundle_tenant,bundle_store,bundle_ref,counts)
   SELECT 'actor_erased',x.request_id,x.selector_digest,x.actor_digest,x.bundle_tenant,x.bundle_store,x.bundle_ref,'{}'::jsonb
   FROM unnest(p_tombstones) x ON CONFLICT (request_id) WHERE kind='actor_erased' DO NOTHING;
  GET DIAGNOSTICS v_inserted=ROW_COUNT;
  v_ts:=p_tombstones;
 ELSE
  SELECT coalesce(array_agg(ROW(g.request_id,g.selector_digest,g.actor_digest,g.bundle_tenant,g.bundle_store,g.bundle_ref)::claims.erasure_tombstone),
   ARRAY[]::claims.erasure_tombstone[]) INTO v_ts FROM claims.retention_log g WHERE g.kind='actor_erased';
 END IF;
 SELECT array_agg(x.actor_digest) INTO v_digests FROM unnest(v_ts) x WHERE x.actor_digest IS NOT NULL;
 IF v_digests IS NOT NULL THEN
  -- ponytail: one scan per table hashing every key; fine for a restore, index a digest column if replay gets frequent.
  FOR r IN SELECT DISTINCT k2.platform,k2.actor_key FROM (
    SELECT b.platform,b.actor_key FROM claims.bundles b WHERE b.platform<>'manual' AND b.purged_at IS NULL
     AND sha256(convert_to(b.actor_key,'UTF8'))=ANY(v_digests)
    UNION SELECT CASE i.object WHEN 'page' THEN 'facebook' ELSE 'instagram' END,i.actor_key FROM claims.meta_intake i
     WHERE sha256(convert_to(i.actor_key,'UTF8'))=ANY(v_digests)) k2(platform,actor_key) LOOP
   v_c:=claims.apply_actor_erasure(r.platform,r.actor_key,NULL,NULL,NULL,NULL);
   SELECT jsonb_object_agg(kk,coalesce((v_sum->>kk)::bigint,0)+coalesce((v_c->>kk)::bigint,0)) INTO v_sum FROM unnest(v_keys) kk;
  END LOOP;
 END IF;
 FOR t IN SELECT x.* FROM unnest(v_ts) x WHERE x.bundle_ref IS NOT NULL LOOP
  v_c:=claims.apply_actor_erasure(NULL,NULL,t.bundle_tenant,t.bundle_store,t.bundle_ref,NULL);
  SELECT jsonb_object_agg(kk,coalesce((v_sum->>kk)::bigint,0)+coalesce((v_c->>kk)::bigint,0)) INTO v_sum FROM unnest(v_keys) kk;
 END LOOP;
 PERFORM set_config('lc.retention_replay','off',true);
 IF v_sum='{}'::jsonb THEN
  v_sum:=(SELECT jsonb_object_agg(kk,0) FROM unnest(v_keys) kk);
 END IF;
 INSERT INTO claims.retention_log(kind,counts) VALUES('replay',v_sum || jsonb_build_object('tombstones',cardinality(v_ts),'inserted',v_inserted));
 RETURN cardinality(v_ts);
END $$;
ALTER FUNCTION claims.replay_actor_erasures(claims.erasure_tombstone[]) OWNER TO commerce_retention_writer;
REVOKE ALL ON FUNCTION claims.replay_actor_erasures(claims.erasure_tombstone[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claims.replay_actor_erasures(claims.erasure_tombstone[]) TO commerce_retention_operator;

-- ---------------------------------------------------------------------------------------
-- COMMENT ON (PROCESS §5): owning package internal/retention, allowed roles, non-goals.
-- ---------------------------------------------------------------------------------------
COMMENT ON TABLE claims.retention_policy IS
 'internal/retention: the single retention policy row (enforced=false = report-only, periods, CAS version). Read/written only by the commerce_retention_writer definers (set_retention_policy, run_retention, retention_status). Non-goal: per-tenant periods.';
COMMENT ON TABLE claims.retention_log IS
 'internal/retention: append-only audit of purge runs, policy changes, actor erasures and restore replays. Counts and digests only, never a sender id, comment ref, label or key (CHECK: every count is a JSON number). Written by commerce_retention_writer definers; run rows older than 400 days are deleted by run_retention when enforced.';
COMMENT ON TYPE claims.erasure_tombstone IS
 'internal/retention: the full actor_erased tuple exported by T20 and replayed by claims.replay_actor_erasures after a restore (never bare digests).';
COMMENT ON COLUMN claims.bundles.purged_at IS
 'U08 (internal/retention): set when the bundle was de-identified in place (RD1: random actor_key and label, binding cleared, link deleted). Written only by the claims.run_retention / apply_actor_erasure definers (commerce_retention_writer). A purged bundle can never get a link again (trigger links_not_purged).';
COMMENT ON CONSTRAINT bundle_label_reserved ON claims.bundles IS
 'U08: the purged-/erased- + 32 hex label pattern is reserved for de-identified bundles (a merchant cannot pre-occupy a label the purge will assign, contract §13 F1); a bundle may carry only its own erased-<id hex> (customers-billing CD7, ruling B23).';
COMMENT ON CONSTRAINT bundle_purged_unbound ON claims.bundles IS 'U08: a purged bundle has no buyer binding.';
DO $$
DECLARE r record;
BEGIN
 FOR r IN SELECT * FROM (VALUES
  ('claims.retention_policy','id','Constant true: the table holds exactly one row.'),
  ('claims.retention_policy','enforced','false = report-only (RD2); set true only by retention-admin policy-set after owner approval (contract §10(3)).'),
  ('claims.retention_policy','link_days','Days after link expiry before the link row is deleted (C1).'),
  ('claims.retention_policy','intake_days','Days before terminal claims.meta_intake rows and reply-ledger comment refs are purged (C3/C4); min 8 (RD5).'),
  ('claims.retention_policy','claims_days','Days after the session claim window closed before a bundle is de-identified (C2).'),
  ('claims.retention_policy','social_days','Days before social comment/message ciphertext is purged (C5); min 8 and <= intake_days (F4).'),
  ('claims.retention_policy','version','CAS version: set_retention_policy requires the current value and increments it.'),
  ('claims.retention_policy','updated_at','Last policy change.'),
  ('claims.retention_policy','updated_by','DB login (session_user) that changed the policy, not a person.'),
  ('claims.retention_log','id','Row id.'),
  ('claims.retention_log','kind','run (one purge batch), policy_set, actor_erased (erasure tombstone), replay (restore replay).'),
  ('claims.retention_log','request_id','actor_erased only: operator ticket id = the confirmation code given to the requester.'),
  ('claims.retention_log','selector_digest','actor_erased only: sha256 of the canonical selector, so a repeat with the same request id is recognised without storing the selector.'),
  ('claims.retention_log','actor_digest','actor_erased only: sha256(actor_key), the restore-replay handle; matches only the erased actor.'),
  ('claims.retention_log','bundle_tenant','Manual-bundle erasure only: tenant of the erased bundle (replay handle with bundle_store, bundle_ref).'),
  ('claims.retention_log','bundle_store','Manual-bundle erasure only: store of the erased bundle.'),
  ('claims.retention_log','bundle_ref','Manual-bundle erasure only: id of the erased bundle (a uuid, no person data).'),
  ('claims.retention_log','counts','JSON object of numbers only (booleans as 0/1): rows purged/erased per class, or the policy values.'),
  ('claims.retention_log','executed_by','DB login (session_user) that wrote the row, not a person.'),
  ('claims.retention_log','created_at','Row time; run rows older than 400 days are pruned.')
 ) AS v(tbl,col,why) LOOP
  EXECUTE format('COMMENT ON COLUMN %s.%I IS %L',r.tbl,r.col,r.why);
 END LOOP;
 -- Every policy of commerce_retention_writer, whatever its command, names its purpose (PROCESS §5).
 FOR r IN SELECT pol.polname AS name, pol.polrelid::regclass::text AS tbl, pol.polcmd AS cmd FROM pg_policy pol
   WHERE pol.polroles = ARRAY[(SELECT ro.oid FROM pg_roles ro WHERE ro.rolname='commerce_retention_writer')]::oid[] LOOP
  EXECUTE format('COMMENT ON POLICY %I ON %s IS %L',r.name,r.tbl,
   '0071: TO commerce_retention_writer (NOLOGIN definer owner, internal/retention) only; '||
   CASE r.cmd WHEN 'r' THEN 'read' WHEN 'd' THEN 'delete' WHEN 'a' THEN 'insert' ELSE 'lock/update' END||
   ' access for the claims retention definers; the definer bodies choose rows, a lock-only UPDATE policy has WITH CHECK (false).');
 END LOOP;
END $$;
COMMENT ON TRIGGER links_not_purged ON claims.links IS
 'U08 RD8: BEFORE INSERT OR UPDATE; a link for a de-identified bundle raises PT404 (issue_link maps it to not found). Function owner commerce_retention_writer, no EXECUTE grant.';
COMMENT ON FUNCTION claims.links_not_purged() IS
 'internal/retention trigger function of claims.links only; no caller EXECUTE (RD8). Owner commerce_retention_writer. Non-goal: authority checks (issue_link owns those).';
COMMENT ON FUNCTION claims.run_retention(integer) IS
 'internal/retention (Worker.Work on commerce_retention_job, RunOnce on commerce_retention_operator). One batch of the hourly purge, C1-C6 of the contract, <= p_limit rows per class, SKIP LOCKED, advisory key hashtextextended(''claims-retention'',0) (busy => {"busy":1}). Report-only unless claims.retention_policy.enforced (report-only never returns more=1). Returns and logs numeric counts only. Non-goals: erasing one actor, choosing rows by caller input.';
COMMENT ON FUNCTION claims.apply_actor_erasure(text,text,uuid,uuid,uuid,text[]) IS
 'internal/retention internal helper of erase_actor and replay_actor_erasures; no EXECUTE grant. Idempotent RD1 de-identification plus deletion of the actor''s links, intake, comment events, terminal reply refs and (peer keys) conversations. Never applies the RD5 hold; in replay mode (lc.retention_replay=on, set only by replay_actor_erasures) it also deletes PENDING intake rows and redacts non-terminal reply operations.';
COMMENT ON FUNCTION claims.erase_actor(uuid,text,text,text,text,uuid,uuid,uuid,text[]) IS
 'internal/retention Erase (commerce_retention_operator via cmd/retention-admin erase). One synchronous tx: exactly one selector (sender key / comment ref / bundle), request-id idempotency (PT409 on another selector, PT404 unknown), RD5 8-day hold returning {"held":1,"retry_after"} with nothing written, then apply_actor_erasure and an actor_erased log row (digest only). Non-goals: a request queue, Meta callbacks.';
COMMENT ON FUNCTION claims.set_retention_policy(bigint,boolean,integer,integer,integer,integer) IS
 'internal/retention SetPolicy (commerce_retention_operator via cmd/retention-admin policy-set). CAS on version (PT409), bounds 22023, audited as a policy_set log row. Non-goal: per-tenant periods.';
COMMENT ON FUNCTION claims.retention_status() IS
 'internal/retention GetStatus (commerce_retention_job and commerce_retention_operator; smoke uses the job login). Numbers only: enforced, version, periods, last_run_unix (0 = never), last_run_more.';
COMMENT ON FUNCTION claims.replay_actor_erasures(claims.erasure_tombstone[]) IS
 'internal/retention Replay (commerce_retention_operator via cmd/retention-admin replay) after a restore (架构 §21.4): applies every actor_erased tuple from the log or the supplied list (inserted first, no duplicates) without the RD5 hold (PENDING intake rows and non-terminal reply operations of an erased actor are deleted/redacted too). Peer-key social deletions are not replayable. Non-goal: normal operation.';
