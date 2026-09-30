-- 0064 Meta claims intake (T10c; contracts/meta-claims-intake-v1.md §2-§7, FROZEN 2026-09-29;
-- integrator rulings a-h in docs/delivery/units/meta-intake-rulings.md).
--
-- Owns: live.claim_sources (session <-> Meta object binding), live.claim_window_intervals
-- (window history), claims.meta_intake (text-free staging queue), the Page-token custody
-- tables (integration.meta_page_credentials/heads), the NOLOGIN role commerce_claims_intake
-- and every definer function below. Post-River 0014 owns only the main-river guards.
--
-- Non-goals: no comment text, unresolved keyword, from.name/username, raw from.id or link
-- token is stored anywhere here; no Graph call, River job or network from SQL; no merchant
-- HTTP (Go owns put_claim_source's receipt/audit); no OAuth/token refresh (T07); no retention
-- purge or DSAR (T14, blocks production mount, contract §1/§8).
--
-- Depends on: 0060 (claims/live tables, commerce_claims_writer), 0028/0029 (meta_inbox routes,
-- finish_social_event, social_source), 0008/0016/0018 (integration.operations ledger, actor
-- family CHECK), 0014 (bindings composite key), 0020 (storefront domains), 0001/0003
-- (identity, resolve_access pattern for principal_holds).
--
-- Callers (each function's COMMENT repeats its only caller):
--   consumer tx (commerce_meta_consumer login): meta_inbox.stage_claim_intake
--   claims intake worker (commerce_claims_intake login): claims.lease_meta_intake, fail_meta_intake,
--     integration.claim_reply_plannable, integration.plan_claim_reply
--   dispatcher pool (commerce_worker): claims.check_meta_reply, integration.load_meta_page_token
--   merchant tx (commerce_runtime): live.put_claim_source
--   operator registrar (commerce_meta_registrar): integration.register_meta_page_token
--
-- Security decisions:
--  * commerce_claims_intake has no GUC-only authority: every policy of its scope is
--    claims.intake_scope(), a definer returning the single row leased in THIS transaction.
--  * NOGUC/SYSTEM policies exist only for commerce_claims_writer (NOLOGIN, reachable only
--    through definers); they never widen a merchant or buyer transaction, which always sets
--    app.tenant_id (0060 GUC policies stay the only ones that apply there).
--  * Every definer pins search_path=pg_catalog, REVOKEs PUBLIC and filters tenant/store from
--    the passed scope or the locked row.

CREATE ROLE commerce_claims_intake NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
COMMENT ON ROLE commerce_claims_intake IS
 'T10c claims intake worker (migrations/0064). NOLOGIN; exactly one dedicated login validated by platform.ValidateClaimsIntakePool. Applies claims.meta_intake rows and plans the first private reply; table access only through claims.intake_scope() policies.';

-- Schema USAGE (contract §4.3 rows; nothing else is granted on these schemas).
GRANT USAGE ON SCHEMA claims TO commerce_worker, commerce_meta_writer, commerce_integration_writer, commerce_claims_intake;
GRANT USAGE ON SCHEMA live, integration, river TO commerce_claims_intake;
GRANT USAGE ON SCHEMA live, control, ops TO commerce_integration_writer;
-- identity USAGE: register_meta_page_token (owner commerce_integration_writer) calls identity.principal_holds (§7 owner validation).
GRANT USAGE ON SCHEMA identity TO commerce_integration_writer;
GRANT USAGE ON SCHEMA integration, meta_inbox TO commerce_claims_writer;
GRANT USAGE ON SCHEMA integration TO commerce_meta_registrar;

-- ---------------------------------------------------------------------------------------
-- identity.principal_holds: the DB-side authority check of a stored principal (no session
-- row). Same joins as identity.resolve_access. Owner commerce_auth (the only role that can
-- read the identity/control projections), EXECUTE commerce_claims_writer only.
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION identity.principal_holds(p_tenant uuid,p_store uuid,p_principal uuid,p_permissions text[])
RETURNS boolean LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 IF p_tenant IS NULL OR p_store IS NULL OR p_principal IS NULL OR p_permissions IS NULL
  OR array_ndims(p_permissions)<>1 OR cardinality(p_permissions) NOT BETWEEN 1 AND 8
  OR EXISTS(SELECT 1 FROM unnest(p_permissions) x WHERE x IS NULL) THEN
  RETURN false;
 END IF;
 IF NOT EXISTS(SELECT 1 FROM identity.principals p
   JOIN control.stores s ON s.id=p_store AND s.tenant_id=p_tenant AND s.active
   JOIN control.tenants t ON t.id=s.tenant_id AND t.active
   JOIN identity.memberships m ON m.tenant_id=s.tenant_id AND m.principal_id=p.id AND m.active
   JOIN identity.store_grants g ON g.tenant_id=s.tenant_id AND g.store_id=s.id
    AND g.principal_id=p.id AND g.permission='store:read'
   WHERE p.id=p_principal AND p.active) THEN
  RETURN false;
 END IF;
 RETURN NOT EXISTS(SELECT 1 FROM unnest(p_permissions) x WHERE NOT EXISTS(
  SELECT 1 FROM identity.store_grants g WHERE g.tenant_id=p_tenant AND g.store_id=p_store
   AND g.principal_id=p_principal AND g.permission=x));
END $$;
ALTER FUNCTION identity.principal_holds(uuid,uuid,uuid,text[]) OWNER TO commerce_auth;
REVOKE ALL ON FUNCTION identity.principal_holds(uuid,uuid,uuid,text[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION identity.principal_holds(uuid,uuid,uuid,text[]) TO commerce_claims_writer, commerce_integration_writer;
COMMENT ON FUNCTION identity.principal_holds(uuid,uuid,uuid,text[]) IS
 'identity owner (commerce_auth); only callers commerce_claims_writer definers (live.put_claim_source, claims.check_meta_reply) and commerce_integration_writer definer integration.register_meta_page_token (integration:manage). True iff the principal, membership, tenant, store and store:read plus every listed permission are active/held; no session, no side effect.';

-- ---------------------------------------------------------------------------------------
-- CHECK widening (contract §4). Existing (platform='manual')=(label IS NOT NULL) and
-- (source_kind='manual')=(principal_id IS NOT NULL) stay and now carry meaning.
-- ---------------------------------------------------------------------------------------
ALTER TABLE claims.bundles DROP CONSTRAINT bundles_platform_check;
ALTER TABLE claims.bundles ADD CONSTRAINT bundles_platform_check CHECK (platform IN ('manual','facebook','instagram'));
ALTER TABLE claims.events DROP CONSTRAINT events_source_kind_check;
ALTER TABLE claims.events ADD CONSTRAINT events_source_kind_check CHECK (source_kind IN ('manual','meta'));
ALTER TABLE claims.events DROP CONSTRAINT events_platform_check;
ALTER TABLE claims.events ADD CONSTRAINT events_platform_check CHECK (platform IN ('manual','facebook','instagram'));
ALTER TABLE claims.events DROP CONSTRAINT events_reason_check;
ALTER TABLE claims.events ADD CONSTRAINT events_reason_check CHECK (reason IN ('NO_MATCH','UNKNOWN_KEYWORD','OFFER_INACTIVE',
 'INVALID_QUANTITY','QUANTITY_REQUIRED','QUANTITY_OVER_MAX','BUNDLE_LIMIT','RATE_LIMITED'));
ALTER TABLE claims.events ADD CONSTRAINT events_rate_limited_meta_only
 CHECK (reason IS DISTINCT FROM 'RATE_LIMITED' OR source_kind='meta');
-- A manual event is platform 'manual' and a meta event a Meta platform: the widened platform CHECK alone would
-- admit ('manual','facebook'), which KC03 has always rejected (§4: the two existing CHECKs "carry meaning").
ALTER TABLE claims.events ADD CONSTRAINT events_source_platform
 CHECK ((source_kind='manual')=(platform='manual'));

-- ---------------------------------------------------------------------------------------
-- live.claim_window_intervals (H5): history of OPEN windows. Maintained only by the trigger.
-- ---------------------------------------------------------------------------------------
CREATE TABLE live.claim_window_intervals (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, session_id uuid NOT NULL,
 generation bigint NOT NULL CHECK (generation>0),
 opened_at timestamptz NOT NULL, closed_at timestamptz,
 PRIMARY KEY (tenant_id,store_id,session_id,generation),
 -- ON DELETE CASCADE: owner SQL that purges a session (test fixtures, T14) removes its history.
 FOREIGN KEY (tenant_id,store_id,session_id) REFERENCES live.claim_windows(tenant_id,store_id,session_id) ON DELETE CASCADE,
 CHECK (closed_at IS NULL OR closed_at>=opened_at));
ALTER TABLE live.claim_window_intervals ENABLE ROW LEVEL SECURITY;
ALTER TABLE live.claim_window_intervals FORCE ROW LEVEL SECURITY;
REVOKE ALL ON live.claim_window_intervals FROM PUBLIC;

CREATE FUNCTION live.track_claim_window_interval() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_rows bigint;
BEGIN
 IF NEW.state='OPEN' AND (TG_OP='INSERT' OR OLD.state<>'OPEN') THEN
  INSERT INTO live.claim_window_intervals(tenant_id,store_id,session_id,generation,opened_at)
   VALUES(NEW.tenant_id,NEW.store_id,NEW.session_id,NEW.generation,NEW.opened_at);
 ELSIF TG_OP='UPDATE' AND OLD.state='OPEN' AND NEW.state<>'OPEN' THEN
  -- Exactly the open interval of the generation that was OPEN; anything else is a corrupted history.
  UPDATE live.claim_window_intervals i SET closed_at=NEW.closed_at
   WHERE i.tenant_id=OLD.tenant_id AND i.store_id=OLD.store_id AND i.session_id=OLD.session_id
    AND i.generation=OLD.generation AND i.closed_at IS NULL;
  GET DIAGNOSTICS v_rows=ROW_COUNT;
  IF v_rows<>1 THEN RAISE EXCEPTION 'claim window interval history broken' USING ERRCODE='23514'; END IF;
 END IF;
 RETURN NULL;
END $$;
ALTER FUNCTION live.track_claim_window_interval() OWNER TO commerce_claims_writer;
REVOKE ALL ON FUNCTION live.track_claim_window_interval() FROM PUBLIC;
CREATE TRIGGER claim_window_interval AFTER INSERT OR UPDATE OF state ON live.claim_windows
 FOR EACH ROW EXECUTE FUNCTION live.track_claim_window_interval();
COMMENT ON FUNCTION live.track_claim_window_interval() IS
 'internal/claims trigger function (live.claim_windows only); owner commerce_claims_writer, no caller EXECUTE. Inserts/closes live.claim_window_intervals on OPEN transitions; no other write path exists for that table.';

-- Backfill: one interval per existing generation>0 window (earlier generations are unknown, MCI06).
INSERT INTO live.claim_window_intervals(tenant_id,store_id,session_id,generation,opened_at,closed_at)
 SELECT tenant_id,store_id,session_id,generation,opened_at,CASE WHEN state='OPEN' THEN NULL ELSE closed_at END
 FROM live.claim_windows WHERE generation>0;

-- ---------------------------------------------------------------------------------------
-- live.claim_sources (§2): which Meta object feeds which live session.
-- ---------------------------------------------------------------------------------------
CREATE TABLE live.claim_sources (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, id uuid NOT NULL DEFAULT gen_random_uuid(),
 session_id uuid NOT NULL,
 platform text NOT NULL CHECK (platform IN ('facebook','instagram')),
 binding_id uuid NOT NULL, binding_version bigint NOT NULL CHECK (binding_version>0),
 object text NOT NULL CHECK (object IN ('page','instagram')),
 asset_id text NOT NULL CHECK (asset_id ~ '^[0-9]{1,40}$'),
 source_object_id text NOT NULL CHECK (source_object_id ~ '^[0-9_]{1,80}$'),
 private_reply boolean NOT NULL DEFAULT false,
 reply_locale text NOT NULL DEFAULT 'zh-TW' CHECK (reply_locale IN ('zh-TW','zh-CN','en')),
 intake_count bigint NOT NULL DEFAULT 0 CHECK (intake_count>=0),
 intake_capped bigint NOT NULL DEFAULT 0 CHECK (intake_capped>=0),
 active boolean NOT NULL DEFAULT true,
 version bigint NOT NULL DEFAULT 1 CHECK (version>0),
 principal_id uuid NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (tenant_id,store_id,id),
 UNIQUE (id),
 UNIQUE (tenant_id,store_id,session_id,object,asset_id,source_object_id),
 FOREIGN KEY (tenant_id,store_id,session_id) REFERENCES live.claim_windows(tenant_id,store_id,session_id),
 FOREIGN KEY (tenant_id,store_id,binding_id) REFERENCES integration.bindings(tenant_id,store_id,id),
 FOREIGN KEY (tenant_id,principal_id) REFERENCES identity.memberships(tenant_id,principal_id),
 CHECK ((platform='facebook')=(object='page')));
-- One live object feeds one session, globally (an object never maps to two sessions).
CREATE UNIQUE INDEX claim_sources_one_active ON live.claim_sources(object,asset_id,source_object_id) WHERE active;
ALTER TABLE live.claim_sources ENABLE ROW LEVEL SECURITY;
ALTER TABLE live.claim_sources FORCE ROW LEVEL SECURITY;
REVOKE ALL ON live.claim_sources FROM PUBLIC;

-- ---------------------------------------------------------------------------------------
-- claims.meta_intake (§4): text-free staging row, one per Meta comment (globally).
-- ---------------------------------------------------------------------------------------
CREATE TABLE claims.meta_intake (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, id uuid NOT NULL DEFAULT gen_random_uuid(),
 inbox_event_id uuid NOT NULL UNIQUE,
 source_id uuid NOT NULL,
 session_id uuid NOT NULL,
 platform text NOT NULL CHECK (platform IN ('facebook','instagram')),
 app_id text NOT NULL CHECK (app_id ~ '^[0-9]{1,40}$'),
 object text NOT NULL CHECK (object IN ('page','instagram')),
 asset_id text NOT NULL CHECK (asset_id ~ '^[0-9]{1,40}$'),
 comment_ref text NOT NULL CHECK (comment_ref ~ '^[0-9_]{1,80}$'),
 live_media boolean NOT NULL,                                     -- ruling (a): instagram_live_comment
 actor_key text NOT NULL CHECK (actor_key ~ '^[0-9a-f]{64}$'),
 occurred_at timestamptz NOT NULL, received_at timestamptz NOT NULL,
 grammar_version text NOT NULL CHECK (grammar_version='kw-v1'),
 grammar_kind text NOT NULL CHECK (grammar_kind IN ('MATCH','NO_MATCH','INVALID_QUANTITY')),
 offer_id uuid,
 unknown_keyword boolean NOT NULL DEFAULT false,
 quantity integer CHECK (quantity BETWEEN 1 AND 999),
 explicit_quantity boolean,
 state text NOT NULL DEFAULT 'PENDING' CHECK (state IN ('PENDING','APPLIED','DROPPED','FAILED')),
 drop_reason text CHECK (drop_reason='window_closed'),
 fail_code text CHECK (fail_code ~ '^[a-z0-9_]{1,40}$'),
 attempts integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 10),
 not_before timestamptz NOT NULL,
 applied_event_id uuid,
 lease_xid xid8,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (tenant_id,store_id,id),
 UNIQUE (id),
 UNIQUE (object,asset_id,comment_ref),
 FOREIGN KEY (tenant_id,store_id,source_id) REFERENCES live.claim_sources(tenant_id,store_id,id),
 CHECK (offer_id IS NULL OR NOT unknown_keyword),
 CHECK (grammar_kind<>'NO_MATCH' OR offer_id IS NULL),
 CHECK (grammar_kind<>'NO_MATCH' OR NOT unknown_keyword),
 CHECK (grammar_kind='NO_MATCH' OR offer_id IS NOT NULL OR unknown_keyword),
 CHECK ((quantity IS NULL)=(explicit_quantity IS NULL)),
 CHECK (quantity IS NULL OR (grammar_kind='MATCH' AND offer_id IS NOT NULL)),
 CHECK (grammar_kind<>'MATCH' OR unknown_keyword OR quantity IS NOT NULL),
 CHECK ((state='APPLIED')=(applied_event_id IS NOT NULL)),
 CHECK ((state='DROPPED')=(drop_reason IS NOT NULL)),
 CHECK (state<>'FAILED' OR fail_code IS NOT NULL));
CREATE INDEX meta_intake_poll ON claims.meta_intake(not_before,created_at) WHERE state='PENDING';
ALTER TABLE claims.meta_intake ENABLE ROW LEVEL SECURITY;
ALTER TABLE claims.meta_intake FORCE ROW LEVEL SECURITY;
REVOKE ALL ON claims.meta_intake FROM PUBLIC;

-- ---------------------------------------------------------------------------------------
-- Page-token custody (§7). Ciphertext only; no runtime SELECT of ciphertext, no worker grant.
-- ---------------------------------------------------------------------------------------
CREATE TABLE integration.meta_page_credentials (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, binding_id uuid NOT NULL,
 provider text NOT NULL CHECK (provider IN ('facebook','instagram')),
 asset_id text NOT NULL CHECK (asset_id ~ '^[0-9]{1,40}$'),
 version bigint NOT NULL CHECK (version>0),
 key_id text NOT NULL CHECK (key_id ~ '^[A-Za-z0-9_-]{1,64}$'),
 nonce bytea NOT NULL CHECK (octet_length(nonce)=12),
 ciphertext bytea NOT NULL CHECK (octet_length(ciphertext) BETWEEN 17 AND 8192),
 scopes_attested text[] NOT NULL CHECK (cardinality(scopes_attested) BETWEEN 1 AND 16
  AND array_to_string(scopes_attested,',') ~ '^[a-z_]{1,64}(,[a-z_]{1,64}){0,15}$'),
 principal_id uuid NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (tenant_id,store_id,binding_id,version),
 FOREIGN KEY (tenant_id,store_id,binding_id,provider,asset_id)
  REFERENCES integration.bindings(tenant_id,store_id,id,provider,external_asset_id),
 FOREIGN KEY (tenant_id,principal_id) REFERENCES identity.memberships(tenant_id,principal_id));
CREATE TABLE integration.meta_page_heads (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, binding_id uuid NOT NULL,
 current_version bigint NOT NULL CHECK (current_version>0),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (tenant_id,store_id,binding_id),
 -- Head and encrypted version commit together (insert head before version, as 0014).
 FOREIGN KEY (tenant_id,store_id,binding_id,current_version)
  REFERENCES integration.meta_page_credentials(tenant_id,store_id,binding_id,version)
  DEFERRABLE INITIALLY DEFERRED);
ALTER TABLE integration.meta_page_credentials ENABLE ROW LEVEL SECURITY;
ALTER TABLE integration.meta_page_credentials FORCE ROW LEVEL SECURITY;
ALTER TABLE integration.meta_page_heads ENABLE ROW LEVEL SECURITY;
ALTER TABLE integration.meta_page_heads FORCE ROW LEVEL SECURITY;
REVOKE ALL ON integration.meta_page_credentials, integration.meta_page_heads FROM PUBLIC;

-- ---------------------------------------------------------------------------------------
-- Privilege delta (contract §4.3, exactly). Shorthand as the contract:
--  I  = (tenant_id,store_id) = claims.intake_scope()       IS = I AND session_id matches
--  LEASED = lease_xid = pg_current_xact_id()               SYSTEM = principal and buyer GUC unset
--  G  = tenant/store GUC scope                             M  = G AND principal set AND buyer unset
--  NOGUC = tenant, store, principal and buyer GUCs all unset
-- ---------------------------------------------------------------------------------------

-- claims.intake_scope(): the single row leased in this transaction (else zero rows).
CREATE FUNCTION claims.intake_scope()
RETURNS TABLE(tenant_id uuid,store_id uuid,session_id uuid)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT x.tenant_id,x.store_id,x.session_id FROM claims.meta_intake x WHERE x.lease_xid=pg_current_xact_id()
$$;
ALTER FUNCTION claims.intake_scope() OWNER TO commerce_claims_writer;
REVOKE ALL ON FUNCTION claims.intake_scope() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claims.intake_scope() TO commerce_claims_intake, commerce_integration_writer;
COMMENT ON FUNCTION claims.intake_scope() IS
 'internal/claims policy helper; owner commerce_claims_writer; EXECUTE commerce_claims_intake and commerce_integration_writer (their RLS policies call it). Returns (tenant,store,session) of the claims.meta_intake row leased by THIS transaction, else zero rows; reads through non-recursive policy intake_owner_read.';

-- commerce_claims_writer: claims.meta_intake (definer bodies are the control).
GRANT SELECT, INSERT ON claims.meta_intake TO commerce_claims_writer;
GRANT UPDATE(state,fail_code,attempts,not_before,lease_xid,updated_at) ON claims.meta_intake TO commerce_claims_writer;
CREATE POLICY intake_owner_read ON claims.meta_intake FOR SELECT TO commerce_claims_writer
 USING (lease_xid=pg_current_xact_id());
CREATE POLICY intake_stage ON claims.meta_intake FOR ALL TO commerce_claims_writer USING (true) WITH CHECK (true);

-- commerce_claims_intake: claims.meta_intake (leased row only).
GRANT SELECT ON claims.meta_intake TO commerce_claims_intake;
GRANT UPDATE(state,drop_reason,applied_event_id,lease_xid,updated_at) ON claims.meta_intake TO commerce_claims_intake;
CREATE POLICY intake_read ON claims.meta_intake FOR SELECT TO commerce_claims_intake
 USING (lease_xid=pg_current_xact_id());
CREATE POLICY intake_update ON claims.meta_intake FOR UPDATE TO commerce_claims_intake
 USING (lease_xid=pg_current_xact_id())
 WITH CHECK (lease_xid=pg_current_xact_id() OR lease_xid IS NULL);

-- commerce_claims_intake: live.claim_windows (row lock for the FOR SHARE fence only).
GRANT SELECT ON live.claim_windows TO commerce_claims_intake;
GRANT UPDATE(updated_at) ON live.claim_windows TO commerce_claims_intake;
CREATE POLICY window_intake_read ON live.claim_windows FOR SELECT TO commerce_claims_intake
 USING ((tenant_id,store_id,session_id)=(SELECT s.tenant_id,s.store_id,s.session_id FROM claims.intake_scope() s));
CREATE POLICY window_intake_lock ON live.claim_windows FOR UPDATE TO commerce_claims_intake
 USING ((tenant_id,store_id,session_id)=(SELECT s.tenant_id,s.store_id,s.session_id FROM claims.intake_scope() s))
 WITH CHECK (false);

-- commerce_claims_intake: live.claim_window_intervals.
GRANT SELECT ON live.claim_window_intervals TO commerce_claims_intake;
CREATE POLICY interval_intake_read ON live.claim_window_intervals FOR SELECT TO commerce_claims_intake
 USING ((tenant_id,store_id,session_id)=(SELECT s.tenant_id,s.store_id,s.session_id FROM claims.intake_scope() s));
-- commerce_claims_writer: trigger body only.
GRANT SELECT, INSERT ON live.claim_window_intervals TO commerce_claims_writer;
GRANT UPDATE(closed_at) ON live.claim_window_intervals TO commerce_claims_writer;
CREATE POLICY interval_writer ON live.claim_window_intervals FOR ALL TO commerce_claims_writer USING (true) WITH CHECK (true);

-- commerce_claims_intake: live.claim_sources (columns needed to decide "source off").
GRANT SELECT(tenant_id,store_id,id,session_id,active,private_reply) ON live.claim_sources TO commerce_claims_intake;
CREATE POLICY source_intake_read ON live.claim_sources FOR SELECT TO commerce_claims_intake
 USING ((tenant_id,store_id,session_id)=(SELECT s.tenant_id,s.store_id,s.session_id FROM claims.intake_scope() s));

-- commerce_claims_intake: live.offers (FOR SHARE fence).
GRANT SELECT ON live.offers TO commerce_claims_intake;
GRANT UPDATE(updated_at) ON live.offers TO commerce_claims_intake;
CREATE POLICY offer_intake_read ON live.offers FOR SELECT TO commerce_claims_intake
 USING ((tenant_id,store_id,session_id)=(SELECT s.tenant_id,s.store_id,s.session_id FROM claims.intake_scope() s));
CREATE POLICY offer_intake_lock ON live.offers FOR UPDATE TO commerce_claims_intake
 USING ((tenant_id,store_id,session_id)=(SELECT s.tenant_id,s.store_id,s.session_id FROM claims.intake_scope() s))
 WITH CHECK (false);

-- commerce_claims_intake: claims.bundles / lines / events.
GRANT SELECT(tenant_id,store_id,id,session_id,platform,actor_key,label,bound_at,line_count,version,created_at,updated_at)
 ON claims.bundles TO commerce_claims_intake;
GRANT INSERT(tenant_id,store_id,session_id,platform,actor_key,label) ON claims.bundles TO commerce_claims_intake;
GRANT UPDATE(line_count,version,updated_at) ON claims.bundles TO commerce_claims_intake;
CREATE POLICY bundle_intake_read ON claims.bundles FOR SELECT TO commerce_claims_intake
 USING ((tenant_id,store_id,session_id)=(SELECT s.tenant_id,s.store_id,s.session_id FROM claims.intake_scope() s));
CREATE POLICY bundle_intake_insert ON claims.bundles FOR INSERT TO commerce_claims_intake
 WITH CHECK ((tenant_id,store_id,session_id)=(SELECT s.tenant_id,s.store_id,s.session_id FROM claims.intake_scope() s)
  AND platform IN ('facebook','instagram') AND label IS NULL);
CREATE POLICY bundle_intake_update ON claims.bundles FOR UPDATE TO commerce_claims_intake
 USING ((tenant_id,store_id,session_id)=(SELECT s.tenant_id,s.store_id,s.session_id FROM claims.intake_scope() s))
 WITH CHECK ((tenant_id,store_id,session_id)=(SELECT s.tenant_id,s.store_id,s.session_id FROM claims.intake_scope() s));

GRANT SELECT ON claims.lines TO commerce_claims_intake;
GRANT INSERT(tenant_id,store_id,session_id,bundle_id,offer_id,sku_id,quantity,version,updated_at) ON claims.lines TO commerce_claims_intake;
GRANT UPDATE(quantity,version,updated_at) ON claims.lines TO commerce_claims_intake;
CREATE POLICY line_intake_read ON claims.lines FOR SELECT TO commerce_claims_intake
 USING ((tenant_id,store_id,session_id)=(SELECT s.tenant_id,s.store_id,s.session_id FROM claims.intake_scope() s));
CREATE POLICY line_intake_insert ON claims.lines FOR INSERT TO commerce_claims_intake
 WITH CHECK ((tenant_id,store_id,session_id)=(SELECT s.tenant_id,s.store_id,s.session_id FROM claims.intake_scope() s));
CREATE POLICY line_intake_update ON claims.lines FOR UPDATE TO commerce_claims_intake
 USING ((tenant_id,store_id,session_id)=(SELECT s.tenant_id,s.store_id,s.session_id FROM claims.intake_scope() s))
 WITH CHECK ((tenant_id,store_id,session_id)=(SELECT s.tenant_id,s.store_id,s.session_id FROM claims.intake_scope() s));

GRANT SELECT, INSERT ON claims.events TO commerce_claims_intake;
CREATE POLICY event_intake_read ON claims.events FOR SELECT TO commerce_claims_intake
 USING ((tenant_id,store_id,session_id)=(SELECT s.tenant_id,s.store_id,s.session_id FROM claims.intake_scope() s));
CREATE POLICY event_intake_insert ON claims.events FOR INSERT TO commerce_claims_intake
 WITH CHECK ((tenant_id,store_id,session_id)=(SELECT s.tenant_id,s.store_id,s.session_id FROM claims.intake_scope() s)
  AND source_kind='meta' AND principal_id IS NULL);

-- commerce_claims_writer: live.claim_sources (definers only; bodies filter tenant/store).
GRANT SELECT ON live.claim_sources TO commerce_claims_writer;
GRANT INSERT(tenant_id,store_id,session_id,platform,binding_id,binding_version,object,asset_id,source_object_id,private_reply,reply_locale,active,version,principal_id)
 ON live.claim_sources TO commerce_claims_writer;
GRANT UPDATE(intake_count,intake_capped,binding_id,binding_version,private_reply,reply_locale,active,version,principal_id,updated_at)
 ON live.claim_sources TO commerce_claims_writer;
CREATE POLICY claim_source_writer ON live.claim_sources FOR SELECT TO commerce_claims_writer USING (true);
CREATE POLICY claim_source_writer_update ON live.claim_sources FOR UPDATE TO commerce_claims_writer USING (true) WITH CHECK (true);
CREATE POLICY claim_source_put ON live.claim_sources FOR INSERT TO commerce_claims_writer
 WITH CHECK (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND nullif(current_setting('app.principal_id',true),'')::uuid IS NOT NULL
  AND nullif(current_setting('app.buyer_id',true),'')::uuid IS NULL);

-- commerce_claims_writer: NOGUC/SYSTEM system reads (IR-15) and links.
CREATE POLICY offer_system_read ON live.offers FOR SELECT TO commerce_claims_writer
 USING (nullif(current_setting('app.tenant_id',true),'') IS NULL AND nullif(current_setting('app.store_id',true),'') IS NULL
  AND nullif(current_setting('app.principal_id',true),'') IS NULL AND nullif(current_setting('app.buyer_id',true),'') IS NULL);
CREATE POLICY link_system_read ON claims.links FOR SELECT TO commerce_claims_writer
 USING (nullif(current_setting('app.tenant_id',true),'') IS NULL AND nullif(current_setting('app.store_id',true),'') IS NULL
  AND nullif(current_setting('app.principal_id',true),'') IS NULL AND nullif(current_setting('app.buyer_id',true),'') IS NULL);
GRANT SELECT(tenant_id,store_id,session_id,state,generation) ON live.claim_windows TO commerce_claims_writer;
CREATE POLICY window_system_read ON live.claim_windows FOR SELECT TO commerce_claims_writer
 USING (nullif(current_setting('app.tenant_id',true),'') IS NULL AND nullif(current_setting('app.store_id',true),'') IS NULL
  AND nullif(current_setting('app.principal_id',true),'') IS NULL AND nullif(current_setting('app.buyer_id',true),'') IS NULL);
GRANT SELECT(tenant_id,store_id,id,session_id,source_kind,source_event_id,outcome,bundle_id) ON claims.events TO commerce_claims_writer;
CREATE POLICY event_system_read ON claims.events FOR SELECT TO commerce_claims_writer
 USING (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND nullif(current_setting('app.principal_id',true),'') IS NULL AND nullif(current_setting('app.buyer_id',true),'') IS NULL);
-- INSERT-only system link: generation 1 for the leased intake's bundle; no UPDATE policy is added
-- (link_rotate/link_issue require app.principal_id IS NOT NULL, which SYSTEM excludes).
CREATE POLICY link_system_issue ON claims.links FOR INSERT TO commerce_claims_writer
 WITH CHECK ((tenant_id,store_id)=(SELECT s.tenant_id,s.store_id FROM claims.intake_scope() s)
  AND nullif(current_setting('app.principal_id',true),'') IS NULL AND nullif(current_setting('app.buyer_id',true),'') IS NULL
  AND generation=1);

-- commerce_claims_writer: cross-domain reads of put_claim_source / check_meta_reply (IR-16).
GRANT SELECT(id,tenant_id,store_id,action,state,request) ON integration.operations TO commerce_claims_writer;
CREATE POLICY claim_reply_operation_read ON integration.operations FOR SELECT TO commerce_claims_writer
 USING (action='meta.private_reply');
GRANT SELECT(tenant_id,store_id,object,asset_id,binding_id,enabled) ON meta_inbox.routes TO commerce_claims_writer;
GRANT SELECT(id,tenant_id,store_id,provider,external_asset_id,semantic_version,enabled) ON integration.bindings TO commerce_claims_writer;
CREATE POLICY binding_claims_read ON integration.bindings FOR SELECT TO commerce_claims_writer
 USING (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND nullif(current_setting('app.principal_id',true),'') IS NOT NULL AND nullif(current_setting('app.buyer_id',true),'') IS NULL);
GRANT SELECT(tenant_id,store_id,binding_id,current_version) ON integration.meta_page_heads TO commerce_claims_writer;
CREATE POLICY page_head_claims_read ON integration.meta_page_heads FOR SELECT TO commerce_claims_writer
 USING (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND nullif(current_setting('app.principal_id',true),'') IS NOT NULL AND nullif(current_setting('app.buyer_id',true),'') IS NULL);

-- IR-16: every cross-domain column grant carries a COMMENT naming the one function that needs it and that no
-- other column of the table is readable by commerce_claims_writer.
DO $$
DECLARE r record; c text;
BEGIN
 FOR r IN SELECT * FROM (VALUES
  ('meta_inbox.routes','tenant_id,store_id,object,asset_id,binding_id,enabled',
   'Column SELECT granted to commerce_claims_writer for live.put_claim_source only (an enabled route of this store names the binding of an asset); no other meta_inbox.routes column is readable by that role (IR-16).'),
  ('integration.bindings','id,tenant_id,store_id,provider,external_asset_id,semantic_version,enabled',
   'Column SELECT granted to commerce_claims_writer for live.put_claim_source under policy binding_claims_read (merchant transaction only): binding provider/asset/version/enabled; no other integration.bindings column is readable by that role (IR-16).'),
  ('integration.meta_page_heads','tenant_id,store_id,binding_id,current_version',
   'Column SELECT granted to commerce_claims_writer for the live.put_claim_source private_reply credential-exists check (merchant transaction only); Page-token ciphertext is never readable by that role (IR-16).'),
  ('integration.operations','id,tenant_id,store_id,action,state,request',
   'Column SELECT granted to commerce_claims_writer for claims.check_meta_reply, restricted by policy claim_reply_operation_read to action=meta.private_reply; the frozen request holds no token, text or name; no other operations column is readable by that role (IR-16).')
 ) AS v(tbl,cols,why)
 LOOP
  FOREACH c IN ARRAY string_to_array(r.cols,',') LOOP
   EXECUTE format('COMMENT ON COLUMN %s.%I IS %L',r.tbl,c,r.why);
  END LOOP;
 END LOOP;
END $$;

-- commerce_claims_intake: function EXECUTE (each created below); commerce_runtime: source read.
GRANT SELECT ON live.claim_sources TO commerce_runtime;
CREATE POLICY claim_source_read ON live.claim_sources FOR SELECT TO commerce_runtime
 USING (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
-- Only the intake producer may insert a meta.private_reply operation.
CREATE POLICY no_merchant_claim_reply ON integration.operations AS RESTRICTIVE FOR INSERT TO commerce_runtime
 WITH CHECK (action<>'meta.private_reply');
-- One reply per Meta comment forever; the key excludes template, policy, source and key versions (§6.1).
CREATE UNIQUE INDEX operations_meta_private_reply_key ON integration.operations(semantic_key) WHERE action='meta.private_reply';

-- commerce_integration_writer: reply planning (plan_claim_reply / claim_reply_plannable, §6.2).
GRANT INSERT ON integration.operations TO commerce_integration_writer;
CREATE POLICY claim_reply_insert ON integration.operations FOR INSERT TO commerce_integration_writer
 WITH CHECK (state='READY' AND generation=0 AND actor_kind='MERCHANT' AND action='meta.private_reply'
  AND purpose='service' AND provider IN ('facebook','instagram'));
GRANT SELECT(tenant_id,store_id,id,inbox_event_id,source_id,session_id,platform,object,asset_id,comment_ref,live_media,occurred_at,received_at,state,lease_xid)
 ON claims.meta_intake TO commerce_integration_writer;
CREATE POLICY intake_reply_read ON claims.meta_intake FOR SELECT TO commerce_integration_writer
 USING (lease_xid=pg_current_xact_id());
GRANT SELECT(tenant_id,store_id,id,session_id,source_event_id,outcome,bundle_id,bundle_version) ON claims.events TO commerce_integration_writer;
CREATE POLICY event_reply_read ON claims.events FOR SELECT TO commerce_integration_writer
 USING ((tenant_id,store_id)=(SELECT s.tenant_id,s.store_id FROM claims.intake_scope() s));
GRANT SELECT(tenant_id,store_id,id,session_id,platform,binding_id,binding_version,object,asset_id,private_reply,reply_locale,active,principal_id)
 ON live.claim_sources TO commerce_integration_writer;
CREATE POLICY source_reply_read ON live.claim_sources FOR SELECT TO commerce_integration_writer
 USING ((tenant_id,store_id,session_id)=(SELECT s.tenant_id,s.store_id,s.session_id FROM claims.intake_scope() s));
-- Row lock for FOR SHARE only; WITH CHECK false forbids any mutation.
GRANT UPDATE(updated_at) ON live.claim_sources TO commerce_integration_writer;
CREATE POLICY source_reply_lock ON live.claim_sources FOR UPDATE TO commerce_integration_writer
 USING ((tenant_id,store_id,session_id)=(SELECT s.tenant_id,s.store_id,s.session_id FROM claims.intake_scope() s))
 WITH CHECK (false);
GRANT SELECT(tenant_id,store_id,published) ON control.storefront_publications TO commerce_integration_writer;
GRANT SELECT(id,tenant_id,store_id,origin,state) ON control.storefront_domains TO commerce_integration_writer;
CREATE POLICY publication_reply_read ON control.storefront_publications FOR SELECT TO commerce_integration_writer
 USING ((tenant_id,store_id)=(SELECT s.tenant_id,s.store_id FROM claims.intake_scope() s));
CREATE POLICY domain_reply_read ON control.storefront_domains FOR SELECT TO commerce_integration_writer
 USING ((tenant_id,store_id)=(SELECT s.tenant_id,s.store_id FROM claims.intake_scope() s));
GRANT INSERT ON ops.audit_events TO commerce_integration_writer;
CREATE POLICY claim_reply_audit ON ops.audit_events FOR INSERT TO commerce_integration_writer
 WITH CHECK ((tenant_id,store_id)=(SELECT s.tenant_id,s.store_id FROM claims.intake_scope() s)
  AND action IN ('meta.private_reply.planned','claim_reply_skipped:source_off','claim_reply_skipped:binding_disabled',
   'claim_reply_skipped:binding_changed','claim_reply_skipped:no_storefront'));
-- register_meta_page_token's audit row (registrar tx pins the GUC scope like register_stripe_account).
CREATE POLICY meta_page_token_audit ON ops.audit_events FOR INSERT TO commerce_integration_writer
 WITH CHECK (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND principal_id=nullif(current_setting('app.principal_id',true),'')::uuid
  AND action='meta.page_token_registered');
-- Owner-side custody rows for the §7 definers (bodies are the control).
GRANT SELECT, INSERT ON integration.meta_page_credentials TO commerce_integration_writer;
GRANT SELECT, INSERT ON integration.meta_page_heads TO commerce_integration_writer;
GRANT UPDATE(current_version,updated_at) ON integration.meta_page_heads TO commerce_integration_writer;
CREATE POLICY page_credential_writer ON integration.meta_page_credentials FOR ALL TO commerce_integration_writer USING (true) WITH CHECK (true);
CREATE POLICY page_head_writer ON integration.meta_page_heads FOR ALL TO commerce_integration_writer USING (true) WITH CHECK (true);

-- ---------------------------------------------------------------------------------------
-- claims.insert_meta_intake (§5.1): the single Meta -> claims edge. Runs in the consumer tx,
-- which sets no scope GUC (asserts NOGUC). Returns NULL when nothing is staged.
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION claims.insert_meta_intake(p_tenant uuid,p_store uuid,p_event uuid,p_received timestamptz,
 p_app text,p_object text,p_asset text,p_object_id text,p_comment_ref text,p_actor_key text,
 p_occurred timestamptz,p_kind text,p_keyword text,p_quantity integer,p_explicit boolean,p_live_media boolean)
RETURNS uuid LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE s record; v_offer uuid; v_unknown boolean:=false; v_id uuid;
BEGIN
 IF current_setting('transaction_isolation')<>'read committed'
  OR nullif(current_setting('app.tenant_id',true),'') IS NOT NULL OR nullif(current_setting('app.store_id',true),'') IS NOT NULL
  OR nullif(current_setting('app.principal_id',true),'') IS NOT NULL OR nullif(current_setting('app.buyer_id',true),'') IS NOT NULL
  OR p_tenant IS NULL OR p_store IS NULL OR p_event IS NULL OR p_received IS NULL OR p_occurred IS NULL OR p_live_media IS NULL
  OR p_app IS NULL OR p_app !~ '^[0-9]{1,40}$' OR p_object IS NULL OR p_object NOT IN ('page','instagram')
  OR p_asset IS NULL OR p_asset !~ '^[0-9]{1,40}$' OR p_object_id IS NULL OR p_object_id !~ '^[0-9_]{1,80}$'
  OR p_comment_ref IS NULL OR p_comment_ref !~ '^[0-9_]{1,80}$' OR p_actor_key IS NULL OR p_actor_key !~ '^[0-9a-f]{64}$'
  OR p_kind IS NULL OR p_kind NOT IN ('MATCH','NO_MATCH','INVALID_QUANTITY')
  OR (p_kind='NO_MATCH' AND (p_keyword IS NOT NULL OR p_quantity IS NOT NULL OR p_explicit IS NOT NULL))
  OR (p_kind='INVALID_QUANTITY' AND (p_keyword IS NULL OR p_quantity IS NOT NULL OR p_explicit IS NOT NULL))
  OR (p_kind='MATCH' AND (p_keyword IS NULL OR p_quantity IS NULL OR p_quantity NOT BETWEEN 1 AND 999 OR p_explicit IS NULL))
  OR (p_keyword IS NOT NULL AND p_keyword !~ '^[A-Z0-9]{1,16}$') THEN
  RAISE EXCEPTION 'invalid meta intake' USING ERRCODE='22023';
 END IF;
 -- Source lock first (lock order §9: consumer -> claim_sources FOR NO KEY UPDATE -> offers read).
 SELECT x.id,x.tenant_id,x.store_id,x.session_id,x.platform,x.intake_count INTO s FROM live.claim_sources x
  WHERE x.object=p_object AND x.asset_id=p_asset AND x.source_object_id=p_object_id AND x.active FOR NO KEY UPDATE;
 IF NOT FOUND OR s.tenant_id<>p_tenant OR s.store_id<>p_store
  OR s.platform<>(CASE p_object WHEN 'page' THEN 'facebook' ELSE 'instagram' END) THEN
  RETURN NULL;
 END IF;
 -- Staging bound (I23): a viral post cannot grow the queue without limit; NO_MATCH comments count.
 IF s.intake_count>=50000 THEN
  UPDATE live.claim_sources SET intake_capped=intake_capped+1 WHERE tenant_id=s.tenant_id AND store_id=s.store_id AND id=s.id;
  RETURN NULL;
 END IF;
 IF p_kind<>'NO_MATCH' THEN
  SELECT o.id INTO v_offer FROM live.offers o
   WHERE o.tenant_id=p_tenant AND o.store_id=p_store AND o.session_id=s.session_id AND o.keyword=p_keyword;
  v_unknown:=v_offer IS NULL;
 END IF;
 INSERT INTO claims.meta_intake(tenant_id,store_id,inbox_event_id,source_id,session_id,platform,app_id,object,asset_id,
  comment_ref,live_media,actor_key,occurred_at,received_at,grammar_version,grammar_kind,offer_id,unknown_keyword,
  quantity,explicit_quantity,state,attempts,not_before,lease_xid)
 VALUES(p_tenant,p_store,p_event,s.id,s.session_id,s.platform,p_app,p_object,p_asset,p_comment_ref,p_live_media,p_actor_key,
  p_occurred,p_received,'kw-v1',p_kind,v_offer,v_unknown,CASE WHEN v_unknown THEN NULL ELSE p_quantity END,
  CASE WHEN v_unknown THEN NULL ELSE p_explicit END,'PENDING',0,clock_timestamp(),NULL)
 ON CONFLICT DO NOTHING RETURNING id INTO v_id;
 IF v_id IS NULL THEN RETURN NULL; END IF;   -- one intake per comment, globally (any app, any kind)
 UPDATE live.claim_sources SET intake_count=intake_count+1 WHERE tenant_id=s.tenant_id AND store_id=s.store_id AND id=s.id;
 RETURN v_id;
END $$;
ALTER FUNCTION claims.insert_meta_intake(uuid,uuid,uuid,timestamptz,text,text,text,text,text,text,timestamptz,text,text,integer,boolean,boolean)
 OWNER TO commerce_claims_writer;
REVOKE ALL ON FUNCTION claims.insert_meta_intake(uuid,uuid,uuid,timestamptz,text,text,text,text,text,text,timestamptz,text,text,integer,boolean,boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claims.insert_meta_intake(uuid,uuid,uuid,timestamptz,text,text,text,text,text,text,timestamptz,text,text,integer,boolean,boolean)
 TO commerce_meta_writer;
COMMENT ON FUNCTION claims.insert_meta_intake(uuid,uuid,uuid,timestamptz,text,text,text,text,text,text,timestamptz,text,text,integer,boolean,boolean) IS
 'internal/claims; only caller meta_inbox.stage_claim_intake (owner commerce_meta_writer). Locks the active claim source, applies the 50000 staging bound, resolves the offer keyword (never stored) and inserts one text-free claims.meta_intake row, NULL when nothing is staged.';

-- ---------------------------------------------------------------------------------------
-- meta_inbox.stage_claim_intake (§5.1): re-derives scope from the LOCKED inbox event and the
-- running river_meta job with the same validator as finish_social_event.
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION meta_inbox.stage_claim_intake(p_event uuid,p_job bigint,p_attempt integer,p_object_id text,
 p_comment_ref text,p_actor_key text,p_occurred timestamptz,p_kind text,p_keyword text,p_quantity integer,p_explicit boolean)
RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE gate record; e meta_inbox.events%ROWTYPE;
BEGIN
 PERFORM meta_inbox.require_authority('commerce_meta_consumer');
 IF p_object_id IS NULL OR p_object_id !~ '^[0-9_]{1,80}$' OR p_comment_ref IS NULL OR p_comment_ref !~ '^[0-9_]{1,80}$'
  OR p_actor_key IS NULL OR p_actor_key !~ '^[0-9a-f]{64}$' OR p_kind IS NULL OR p_kind NOT IN ('MATCH','NO_MATCH','INVALID_QUANTITY') THEN
  RAISE EXCEPTION 'invalid meta claim intake' USING ERRCODE='22023';
 END IF;
 SELECT * INTO gate FROM meta_inbox.social_source(p_event,p_job,p_attempt,false);
 e:=gate.source;
 IF gate.outcome<>'READY' THEN RAISE EXCEPTION 'meta claim intake policy denied' USING ERRCODE='PT409'; END IF;
 -- Only a comment that qualifies, and only the comment fact this very transaction wrote.
 IF e.kind NOT IN ('page_comment_add','instagram_comment','instagram_live_comment')
  OR NOT EXISTS(SELECT 1 FROM social.comment_events c WHERE c.event_id=e.id AND c.tenant_id=e.tenant_id AND c.store_id=e.store_id
   AND c.kind=e.kind AND c.xmin=pg_current_xact_id()::xid) THEN
  RAISE EXCEPTION 'invalid meta claim intake' USING ERRCODE='22023';
 END IF;
 -- Ruling (c): IG units carry no time (U7), so the locked event's occurred_at is the comment time.
 IF e.occurred_at IS NULL THEN RETURN NULL; END IF;
 IF p_occurred IS NOT NULL AND p_occurred<>e.occurred_at THEN
  RAISE EXCEPTION 'invalid meta claim intake' USING ERRCODE='22023';
 END IF;
 RETURN claims.insert_meta_intake(e.tenant_id,e.store_id,e.id,e.created_at,e.app_id,e.object,e.asset_id,p_object_id,
  p_comment_ref,p_actor_key,e.occurred_at,p_kind,p_keyword,p_quantity,p_explicit,e.kind='instagram_live_comment');
END $$;
ALTER FUNCTION meta_inbox.stage_claim_intake(uuid,bigint,integer,text,text,text,timestamptz,text,text,integer,boolean) OWNER TO commerce_meta_writer;
REVOKE ALL ON FUNCTION meta_inbox.stage_claim_intake(uuid,bigint,integer,text,text,text,timestamptz,text,text,integer,boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION meta_inbox.stage_claim_intake(uuid,bigint,integer,text,text,text,timestamptz,text,text,integer,boolean) TO commerce_meta_consumer;
COMMENT ON FUNCTION meta_inbox.stage_claim_intake(uuid,bigint,integer,text,text,text,timestamptz,text,text,integer,boolean) IS
 'meta_inbox owner; only caller internal/integrations/meta ConsumerWorker (commerce_meta_consumer) after finish_social_event, before COMMIT. Re-derives scope from the locked event and running river_meta job, then calls claims.insert_meta_intake (the single sanctioned Meta -> claims edge). NULL = not staged.';

-- ---------------------------------------------------------------------------------------
-- Intake poll: lease / fail (§5.3). No GUC authority; the lease is the authority.
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION claims.lease_meta_intake() RETURNS SETOF claims.meta_intake
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE r claims.meta_intake%ROWTYPE;
BEGIN
 IF current_setting('transaction_isolation')<>'read committed'
  OR nullif(current_setting('app.tenant_id',true),'') IS NOT NULL OR nullif(current_setting('app.store_id',true),'') IS NOT NULL
  OR nullif(current_setting('app.principal_id',true),'') IS NOT NULL OR nullif(current_setting('app.buyer_id',true),'') IS NOT NULL THEN
  RAISE EXCEPTION 'invalid meta intake lease' USING ERRCODE='22023';
 END IF;
 -- One lease per transaction: claims.intake_scope() must return at most one row.
 IF EXISTS(SELECT 1 FROM claims.meta_intake x WHERE x.lease_xid=pg_current_xact_id()) THEN
  RAISE EXCEPTION 'meta intake already leased' USING ERRCODE='22023';
 END IF;
 SELECT x.* INTO r FROM claims.meta_intake x WHERE x.state='PENDING' AND x.not_before<=clock_timestamp()
  ORDER BY x.not_before,x.created_at LIMIT 1 FOR UPDATE SKIP LOCKED;
 IF NOT FOUND THEN RETURN; END IF;
 UPDATE claims.meta_intake x SET lease_xid=pg_current_xact_id(),updated_at=clock_timestamp()
  WHERE x.tenant_id=r.tenant_id AND x.store_id=r.store_id AND x.id=r.id RETURNING x.* INTO r;
 RETURN NEXT r;
END $$;
ALTER FUNCTION claims.lease_meta_intake() OWNER TO commerce_claims_writer;
REVOKE ALL ON FUNCTION claims.lease_meta_intake() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claims.lease_meta_intake() TO commerce_claims_intake;
COMMENT ON FUNCTION claims.lease_meta_intake() IS
 'internal/claims; only caller internal/claimsintake Poller (commerce_claims_intake). Leases the oldest due PENDING claims.meta_intake row (FOR UPDATE SKIP LOCKED) for this transaction; zero rows = idle.';

CREATE FUNCTION claims.fail_meta_intake(p_intake uuid,p_code text,p_final boolean) RETURNS void
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE r record; v_attempts integer; v_final boolean;
BEGIN
 IF p_intake IS NULL OR p_code IS NULL OR p_code !~ '^[a-z0-9_]{1,40}$' OR p_final IS NULL
  OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid meta intake failure' USING ERRCODE='22023';
 END IF;
 SELECT x.tenant_id,x.store_id,x.attempts INTO r FROM claims.meta_intake x WHERE x.id=p_intake AND x.state='PENDING' FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'meta intake not pending' USING ERRCODE='P0002'; END IF;
 v_attempts:=r.attempts+1;
 v_final:=p_final OR v_attempts>=10;
 UPDATE claims.meta_intake x SET attempts=v_attempts,fail_code=p_code,lease_xid=NULL,updated_at=clock_timestamp(),
  state=CASE WHEN v_final THEN 'FAILED' ELSE 'PENDING' END,
  not_before=CASE WHEN v_final THEN x.not_before
   ELSE clock_timestamp()+make_interval(secs=>least(power(2,v_attempts)::double precision,300)) END
  WHERE x.tenant_id=r.tenant_id AND x.store_id=r.store_id AND x.id=p_intake;
END $$;
ALTER FUNCTION claims.fail_meta_intake(uuid,text,boolean) OWNER TO commerce_claims_writer;
REVOKE ALL ON FUNCTION claims.fail_meta_intake(uuid,text,boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claims.fail_meta_intake(uuid,text,boolean) TO commerce_claims_intake;
COMMENT ON FUNCTION claims.fail_meta_intake(uuid,text,boolean) IS
 'internal/claims; only caller internal/claimsintake Poller (commerce_claims_intake) in a separate short transaction after a rolled-back apply. Bumps attempts with exponential backoff (2^n s, cap 300 s); FAILED at attempts=10 or when final. PENDING rows only, no lease needed.';

-- ---------------------------------------------------------------------------------------
-- claims.issue_system_link (§4.4 clause 5): INSERT-only generation 1 for the bundle the leased
-- intake's ACCEPTED event created. Never updates, rotates or releases a link.
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION claims.issue_system_link(p_intake uuid,p_hash bytea) RETURNS timestamptz
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE i record; v_bundle uuid; v_principal uuid; v_now timestamptz;
BEGIN
 IF p_intake IS NULL OR p_hash IS NULL OR octet_length(p_hash)<>32
  OR current_setting('transaction_isolation')<>'read committed'
  OR coalesce(current_setting('app.tenant_id',true),'') !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  OR coalesce(current_setting('app.store_id',true),'') !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  OR nullif(current_setting('app.principal_id',true),'') IS NOT NULL OR nullif(current_setting('app.buyer_id',true),'') IS NOT NULL THEN
  RAISE EXCEPTION 'invalid system link issue' USING ERRCODE='22023';
 END IF;
 SELECT x.tenant_id,x.store_id,x.source_id,x.inbox_event_id INTO i FROM claims.meta_intake x
  WHERE x.id=p_intake AND x.lease_xid=pg_current_xact_id();
 IF NOT FOUND OR i.tenant_id<>current_setting('app.tenant_id')::uuid OR i.store_id<>current_setting('app.store_id')::uuid THEN
  RAISE EXCEPTION 'invalid system link issue' USING ERRCODE='22023';
 END IF;
 SELECT e.bundle_id INTO v_bundle FROM claims.events e WHERE e.tenant_id=i.tenant_id AND e.store_id=i.store_id
  AND e.source_event_id=i.inbox_event_id AND e.source_kind='meta' AND e.outcome='ACCEPTED';
 SELECT s.principal_id INTO v_principal FROM live.claim_sources s WHERE s.tenant_id=i.tenant_id AND s.store_id=i.store_id AND s.id=i.source_id;
 IF v_bundle IS NULL OR v_principal IS NULL THEN
  RAISE EXCEPTION 'invalid system link issue' USING ERRCODE='22023';
 END IF;
 v_now:=clock_timestamp();   -- one clock read: issued_at and expires_at differ by exactly 72 hours
 INSERT INTO claims.links(tenant_id,store_id,bundle_id,token_hash,generation,issued_at,expires_at,principal_id)
  VALUES(i.tenant_id,i.store_id,v_bundle,p_hash,1,v_now,v_now+interval '72 hours',v_principal);
 RETURN v_now+interval '72 hours';
END $$;
ALTER FUNCTION claims.issue_system_link(uuid,bytea) OWNER TO commerce_claims_writer;
REVOKE ALL ON FUNCTION claims.issue_system_link(uuid,bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claims.issue_system_link(uuid,bytea) TO commerce_integration_writer;
COMMENT ON FUNCTION claims.issue_system_link(uuid,bytea) IS
 'internal/claims; only caller integration.plan_claim_reply (owner commerce_integration_writer). INSERT-only generation-1 link (policy link_system_issue) for the bundle created by the leased intake; returns expires_at. Cannot rotate, release or update.';

-- ---------------------------------------------------------------------------------------
-- claims.check_meta_reply (§6.3): every dispatch attempt, on the dispatcher's commerce_worker
-- pool with no GUCs. Returns OK or one fixed deny code; only a returned code is a policy denial.
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION claims.check_meta_reply(p_operation uuid,p_hash bytea) RETURNS text
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE o record; src record; v_deadline timestamptz; v_live boolean; v_ok boolean;
BEGIN
 IF p_operation IS NULL OR p_hash IS NULL OR octet_length(p_hash)<>32
  OR nullif(current_setting('app.tenant_id',true),'') IS NOT NULL OR nullif(current_setting('app.store_id',true),'') IS NOT NULL
  OR nullif(current_setting('app.principal_id',true),'') IS NOT NULL OR nullif(current_setting('app.buyer_id',true),'') IS NOT NULL THEN
  RAISE EXCEPTION 'invalid meta reply check' USING ERRCODE='22023';
 END IF;
 SELECT x.tenant_id,x.store_id,x.request INTO o FROM integration.operations x WHERE x.id=p_operation AND x.action='meta.private_reply';
 IF NOT FOUND OR jsonb_typeof(o.request)<>'object' THEN
  RAISE EXCEPTION 'invalid meta reply check' USING ERRCODE='22023';
 END IF;
 v_deadline:=(o.request->>'deadline_at')::timestamptz;
 v_live:=(o.request->>'live_media')::boolean;
 IF clock_timestamp()>=v_deadline THEN RETURN 'deadline'; END IF;
 SELECT s.active,s.private_reply,s.principal_id,s.session_id INTO src FROM live.claim_sources s
  WHERE s.tenant_id=o.tenant_id AND s.store_id=o.store_id AND s.id=(o.request->>'source_id')::uuid;
 IF NOT FOUND OR NOT src.active OR NOT src.private_reply THEN RETURN 'source_off'; END IF;
 IF NOT identity.principal_holds(o.tenant_id,o.store_id,src.principal_id,ARRAY['live:manage','integration:execute']) THEN
  RETURN 'principal_revoked';
 END IF;
 SELECT true INTO v_ok FROM claims.links k WHERE k.tenant_id=o.tenant_id AND k.store_id=o.store_id
  AND k.bundle_id=(o.request->>'bundle_id')::uuid AND k.generation=1 AND k.token_hash=p_hash
  AND k.expires_at>clock_timestamp()+interval '10 minutes';
 IF v_ok IS NOT TRUE THEN RETURN 'link_invalid'; END IF;
 IF v_live AND NOT EXISTS(SELECT 1 FROM live.claim_windows w WHERE w.tenant_id=o.tenant_id AND w.store_id=o.store_id
  AND w.session_id=src.session_id AND w.state='OPEN') THEN
  RETURN 'live_closed';
 END IF;
 RETURN 'OK';
END $$;
ALTER FUNCTION claims.check_meta_reply(uuid,bytea) OWNER TO commerce_claims_writer;
REVOKE ALL ON FUNCTION claims.check_meta_reply(uuid,bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claims.check_meta_reply(uuid,bytea) TO commerce_worker;
COMMENT ON FUNCTION claims.check_meta_reply(uuid,bytea) IS
 'internal/claims; only caller the internal/integrations/metareply Check route (commerce_worker pool, no GUCs); owner commerce_claims_writer. Lock-free read of the frozen operation: OK or deadline | source_off | principal_revoked | link_invalid | live_closed. Never writes; infrastructure errors are not policy denials.';

-- ---------------------------------------------------------------------------------------
-- live.put_claim_source (§2): merchant binds a Meta object to one session. Called inside
-- command.Run by Go (receipt + audit are written there, as commerce_runtime).
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION live.put_claim_source(p_session uuid,p_object text,p_asset text,p_object_id text,
 p_private_reply boolean,p_locale text,p_active boolean,p_expected_version bigint)
RETURNS uuid LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_tenant uuid; v_store uuid; v_principal uuid; v_bindings uuid[]; b record; v_id uuid; v_version bigint;
BEGIN
 IF current_setting('transaction_isolation')<>'read committed'
  OR coalesce(current_setting('app.tenant_id',true),'') !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  OR coalesce(current_setting('app.store_id',true),'') !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  OR coalesce(current_setting('app.principal_id',true),'') !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  OR nullif(current_setting('app.buyer_id',true),'') IS NOT NULL
  OR p_session IS NULL OR p_object IS NULL OR p_object NOT IN ('page','instagram')
  OR p_asset IS NULL OR p_asset !~ '^[0-9]{1,40}$' OR p_object_id IS NULL OR p_object_id !~ '^[0-9_]{1,80}$'
  OR p_private_reply IS NULL OR p_locale IS NULL OR p_locale NOT IN ('zh-TW','zh-CN','en')
  OR p_active IS NULL OR p_expected_version IS NULL OR p_expected_version<0 OR p_expected_version>=9223372036854775807 THEN
  RAISE EXCEPTION 'invalid claim source' USING ERRCODE='22023';
 END IF;
 v_tenant:=current_setting('app.tenant_id')::uuid;
 v_store:=current_setting('app.store_id')::uuid;
 v_principal:=current_setting('app.principal_id')::uuid;
 IF NOT identity.principal_holds(v_tenant,v_store,v_principal,ARRAY['live:manage','integration:execute']) THEN
  RAISE EXCEPTION 'claim source access unavailable' USING ERRCODE='PT403';
 END IF;
 -- (object, asset) must be an enabled route of this very store; several apps may route one asset,
 -- but they must all point at one binding.
 SELECT array_agg(DISTINCT r.binding_id) INTO v_bindings FROM meta_inbox.routes r
  WHERE r.tenant_id=v_tenant AND r.store_id=v_store AND r.object=p_object AND r.asset_id=p_asset AND r.enabled;
 IF v_bindings IS NULL OR cardinality(v_bindings)<>1 THEN
  RAISE EXCEPTION 'claim source route mismatch' USING ERRCODE='PT409';
 END IF;
 SELECT x.id,x.provider,x.external_asset_id,x.semantic_version,x.enabled INTO b FROM integration.bindings x
  WHERE x.tenant_id=v_tenant AND x.store_id=v_store AND x.id=v_bindings[1];
 IF NOT FOUND OR b.provider<>(CASE p_object WHEN 'page' THEN 'facebook' ELSE 'instagram' END)
  OR b.external_asset_id<>p_asset OR (p_active AND NOT b.enabled) THEN
  RAISE EXCEPTION 'claim source binding mismatch' USING ERRCODE='PT409';
 END IF;
 IF p_private_reply AND NOT EXISTS(SELECT 1 FROM integration.meta_page_heads h
  WHERE h.tenant_id=v_tenant AND h.store_id=v_store AND h.binding_id=b.id) THEN
  RAISE EXCEPTION 'claim source has no page credential' USING ERRCODE='PT409';
 END IF;
 SELECT s.id,s.version INTO v_id,v_version FROM live.claim_sources s
  WHERE s.tenant_id=v_tenant AND s.store_id=v_store AND s.session_id=p_session AND s.object=p_object
   AND s.asset_id=p_asset AND s.source_object_id=p_object_id FOR UPDATE;
 IF NOT FOUND THEN
  IF p_expected_version<>0 THEN RAISE EXCEPTION 'claim source version changed' USING ERRCODE='PT409'; END IF;
  INSERT INTO live.claim_sources(tenant_id,store_id,session_id,platform,binding_id,binding_version,object,asset_id,
   source_object_id,private_reply,reply_locale,active,version,principal_id)
  VALUES(v_tenant,v_store,p_session,b.provider,b.id,b.semantic_version,p_object,p_asset,p_object_id,p_private_reply,
   p_locale,p_active,1,v_principal) RETURNING id INTO v_id;
 ELSE
  IF v_version<>p_expected_version THEN RAISE EXCEPTION 'claim source version changed' USING ERRCODE='PT409'; END IF;
  UPDATE live.claim_sources s SET binding_id=b.id,binding_version=b.semantic_version,private_reply=p_private_reply,
   reply_locale=p_locale,active=p_active,version=s.version+1,principal_id=v_principal,updated_at=clock_timestamp()
   WHERE s.tenant_id=v_tenant AND s.store_id=v_store AND s.id=v_id;
 END IF;
 RETURN v_id;
END $$;
ALTER FUNCTION live.put_claim_source(uuid,text,text,text,boolean,text,boolean,bigint) OWNER TO commerce_claims_writer;
REVOKE ALL ON FUNCTION live.put_claim_source(uuid,text,text,text,boolean,text,boolean,bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION live.put_claim_source(uuid,text,text,text,boolean,text,boolean,bigint) TO commerce_runtime;
COMMENT ON FUNCTION live.put_claim_source(uuid,text,text,text,boolean,text,boolean,bigint) IS
 'internal/claims (T12 admin route); EXECUTE commerce_runtime only. Merchant-transaction guard, principal_holds(live:manage,integration:execute), route/binding checks, CAS on version (0 creates). One active source per (object, asset, object id) globally.';

-- ---------------------------------------------------------------------------------------
-- integration.claim_reply_plannable / plan_claim_reply (§6.2): the only system producer of a
-- meta.private_reply operation. Lock order: binding FOR SHARE -> source FOR SHARE -> River job
-- (read) -> link insert -> operation.
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION integration.claim_reply_plannable(p_intake uuid) RETURNS text
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE i record; s0 record; s record; b record; v_code text;
BEGIN
 IF p_intake IS NULL OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid claim reply check' USING ERRCODE='22023';
 END IF;
 SELECT x.tenant_id,x.store_id,x.source_id INTO i FROM claims.meta_intake x
  WHERE x.id=p_intake AND x.lease_xid=pg_current_xact_id() AND x.state='PENDING';
 IF NOT FOUND THEN RAISE EXCEPTION 'invalid claim reply check' USING ERRCODE='22023'; END IF;
 SELECT y.binding_id INTO s0 FROM live.claim_sources y WHERE y.tenant_id=i.tenant_id AND y.store_id=i.store_id AND y.id=i.source_id;
 IF NOT FOUND THEN RAISE EXCEPTION 'invalid claim reply check' USING ERRCODE='22023'; END IF;
 -- Binding before source (lock order §9, as the consumer).
 SELECT z.id,z.provider,z.external_asset_id,z.enabled INTO b FROM integration.bindings z
  WHERE z.tenant_id=i.tenant_id AND z.store_id=i.store_id AND z.id=s0.binding_id FOR SHARE;
 SELECT y.principal_id,y.binding_id,y.asset_id,y.platform,y.active,y.private_reply INTO s FROM live.claim_sources y
  WHERE y.tenant_id=i.tenant_id AND y.store_id=i.store_id AND y.id=i.source_id FOR SHARE;
 IF NOT FOUND THEN RAISE EXCEPTION 'invalid claim reply check' USING ERRCODE='22023'; END IF;
 IF NOT s.active OR NOT s.private_reply THEN v_code:='source_off';
 ELSIF b.id IS NULL OR NOT b.enabled THEN v_code:='binding_disabled';
 ELSIF s.binding_id<>s0.binding_id OR b.provider<>s.platform OR b.external_asset_id<>s.asset_id THEN v_code:='binding_changed';
 ELSIF NOT EXISTS(SELECT 1 FROM control.storefront_domains d JOIN control.storefront_publications p
   ON p.tenant_id=d.tenant_id AND p.store_id=d.store_id AND p.published
   WHERE d.tenant_id=i.tenant_id AND d.store_id=i.store_id AND d.state='ACTIVE') THEN v_code:='no_storefront';
 ELSE RETURN 'OK';
 END IF;
 INSERT INTO ops.audit_events(tenant_id,store_id,principal_id,action)
  VALUES(i.tenant_id,i.store_id,s.principal_id,'claim_reply_skipped:'||v_code);
 RETURN v_code;
END $$;
ALTER FUNCTION integration.claim_reply_plannable(uuid) OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION integration.claim_reply_plannable(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION integration.claim_reply_plannable(uuid) TO commerce_claims_intake;
COMMENT ON FUNCTION integration.claim_reply_plannable(uuid) IS
 'integration owner; only caller internal/claimsintake Poller (commerce_claims_intake) before inserting the River job. Locks binding then source FOR SHARE and returns OK or source_off | binding_disabled | binding_changed | no_storefront (audited skip, nothing else written). A bumped binding version alone is never a skip (IR-17).';

CREATE FUNCTION integration.plan_claim_reply(p_intake uuid,p_operation uuid,p_link_hash bytea,p_link_key_id text,p_job bigint)
RETURNS uuid LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE i record; ev record; src record; b record; j record; v_domain uuid; v_origin text; v_expires timestamptz;
 v_deadline timestamptz; v_request jsonb; v_key text;
BEGIN
 IF p_intake IS NULL OR p_operation IS NULL OR p_link_hash IS NULL OR octet_length(p_link_hash)<>32
  OR p_link_key_id IS NULL OR p_link_key_id !~ '^[0-9a-f]{16}$' OR p_job IS NULL OR p_job<=0
  OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid claim reply plan' USING ERRCODE='22023';
 END IF;
 SELECT x.tenant_id,x.store_id,x.inbox_event_id,x.source_id,x.session_id,x.platform,x.object,x.asset_id,x.comment_ref,
  x.live_media,x.occurred_at,x.received_at INTO i FROM claims.meta_intake x
  WHERE x.id=p_intake AND x.lease_xid=pg_current_xact_id() AND x.state='PENDING';
 IF NOT FOUND THEN RAISE EXCEPTION 'invalid claim reply plan' USING ERRCODE='22023'; END IF;
 -- Only the ACCEPTED event that CREATED the bundle plans a reply (bundle_version 1, IR-3).
 SELECT e.id,e.bundle_id INTO ev FROM claims.events e WHERE e.tenant_id=i.tenant_id AND e.store_id=i.store_id
  AND e.source_event_id=i.inbox_event_id AND e.outcome='ACCEPTED' AND e.bundle_version=1;
 IF NOT FOUND THEN RAISE EXCEPTION 'invalid claim reply plan' USING ERRCODE='22023'; END IF;
 SELECT y.id,y.binding_id,y.platform,y.asset_id,y.reply_locale,y.principal_id,y.active,y.private_reply INTO src
  FROM live.claim_sources y WHERE y.tenant_id=i.tenant_id AND y.store_id=i.store_id AND y.id=i.source_id;
 SELECT z.id,z.provider,z.external_asset_id,z.semantic_version,z.enabled INTO b FROM integration.bindings z
  WHERE z.tenant_id=i.tenant_id AND z.store_id=i.store_id AND z.id=src.binding_id;
 -- claim_reply_plannable held the locks since; any failure here is an invariant breach, not a normal path.
 IF src.id IS NULL OR NOT src.active OR NOT src.private_reply OR b.id IS NULL OR NOT b.enabled
  OR b.provider<>src.platform OR b.external_asset_id<>src.asset_id OR b.provider<>i.platform THEN
  RAISE EXCEPTION 'invalid claim reply plan' USING ERRCODE='22023';
 END IF;
 SELECT d.id,d.origin INTO v_domain,v_origin FROM control.storefront_domains d JOIN control.storefront_publications p
  ON p.tenant_id=d.tenant_id AND p.store_id=d.store_id AND p.published
  WHERE d.tenant_id=i.tenant_id AND d.store_id=i.store_id AND d.state='ACTIVE' ORDER BY d.id LIMIT 1;
 IF v_domain IS NULL THEN RAISE EXCEPTION 'invalid claim reply plan' USING ERRCODE='22023'; END IF;
 -- The River job must be exactly the row inserted in this transaction (xmin needs table-level SELECT).
 SELECT r.id INTO j FROM river.river_job r WHERE r.id=p_job AND r.kind='external_operation_v1' AND r.queue='default'
  AND r.unique_key IS NULL AND r.args=jsonb_build_object('operation_id',p_operation::text,'version',1)
  AND r.xmin=pg_current_xact_id()::xid;
 IF NOT FOUND THEN RAISE EXCEPTION 'invalid claim reply plan' USING ERRCODE='22023'; END IF;
 v_expires:=claims.issue_system_link(p_intake,p_link_hash);
 v_deadline:=least(i.occurred_at+interval '7 days'-interval '1 hour',v_expires-interval '10 minutes',
  CASE WHEN i.live_media THEN i.received_at+interval '15 minutes' END);
 v_request:=jsonb_build_object('v',1,'platform',i.platform,'source_id',i.source_id,'asset_id',i.asset_id,
  'comment_ref',i.comment_ref,'bundle_id',ev.bundle_id,'session_id',i.session_id,'link_generation',1,
  'link_key_id',p_link_key_id,'locale',src.reply_locale,'template','claim-link/v1','policy','mpr-policy/v1',
  'message_type','first_private_reply','takeover_generation',0,'origin_ref',v_domain,'origin',v_origin,
  'deadline_at',to_char(v_deadline AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),'live_media',i.live_media);
 IF octet_length(v_request::text)>2048 THEN RAISE EXCEPTION 'invalid claim reply plan' USING ERRCODE='22023'; END IF;
 v_key:='mpr:'||substr(encode(sha256(convert_to(i.object||'|'||i.asset_id||'|'||i.comment_ref,'UTF8')),'hex'),1,48);
 INSERT INTO integration.operations(tenant_id,store_id,id,principal_id,binding_id,binding_version,provider,external_asset_id,
  purpose,action,semantic_key,request_hash,request,job_id)
 VALUES(i.tenant_id,i.store_id,p_operation,src.principal_id,b.id,b.semantic_version,b.provider,b.external_asset_id,
  'service','meta.private_reply',v_key,sha256(convert_to(v_request::text,'UTF8')),v_request,p_job);
 INSERT INTO integration.operation_events(tenant_id,store_id,operation_id,generation,state,mode,reason_code)
  VALUES(i.tenant_id,i.store_id,p_operation,0,'READY','','operation_planned');
 INSERT INTO ops.audit_events(tenant_id,store_id,principal_id,action)
  VALUES(i.tenant_id,i.store_id,src.principal_id,'meta.private_reply.planned');
 RETURN p_operation;
END $$;
ALTER FUNCTION integration.plan_claim_reply(uuid,uuid,bytea,text,bigint) OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION integration.plan_claim_reply(uuid,uuid,bytea,text,bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION integration.plan_claim_reply(uuid,uuid,bytea,text,bigint) TO commerce_claims_intake;
COMMENT ON FUNCTION integration.plan_claim_reply(uuid,uuid,bytea,text,bigint) IS
 'integration owner; only caller internal/claimsintake Poller (commerce_claims_intake) after claim_reply_plannable returned OK. Derives everything from the leased intake, its bundle-creating ACCEPTED event and the source; verifies the River job is this transaction''s external_operation_v1 row; issues the system link, the READY meta.private_reply operation (semantic key per comment, purpose service), its event and audit. Any failed precondition is an invariant breach (22023).';

-- ---------------------------------------------------------------------------------------
-- Page-token custody definers (§7).
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION integration.load_meta_page_token(p_operation uuid,p_generation bigint,p_lease_token bytea)
RETURNS TABLE(tenant_id uuid,store_id uuid,binding_id uuid,provider text,asset_id text,version bigint,key_id text,
 nonce bytea,ciphertext bytea,scopes_attested text[])
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_binding uuid; b record; o record; v_head bigint;
BEGIN
 IF p_operation IS NULL OR p_generation IS NULL OR p_generation<1 OR p_lease_token IS NULL OR octet_length(p_lease_token)<>32 THEN
  RAISE EXCEPTION 'invalid Meta credential load' USING ERRCODE='22023';
 END IF;
 SELECT x.binding_id INTO v_binding FROM integration.operations x
  WHERE x.id=p_operation AND x.action='meta.private_reply' AND x.actor_kind='MERCHANT';
 IF NOT FOUND THEN RAISE EXCEPTION 'Meta credential unavailable' USING ERRCODE='P0002'; END IF;
 -- Binding before operation (dispatcher lock order).
 SELECT z.id,z.provider,z.external_asset_id INTO b FROM integration.bindings z WHERE z.id=v_binding FOR SHARE;
 SELECT x.tenant_id,x.store_id,x.binding_id,x.provider,x.external_asset_id,x.state,x.lease_mode,x.generation,x.lease_until,
  x.lease_token_hash INTO o FROM integration.operations x WHERE x.id=p_operation FOR SHARE;
 IF NOT FOUND OR o.state<>'DISPATCHING' OR o.lease_mode<>'dispatch' OR o.generation<>p_generation OR o.lease_until IS NULL
  OR o.lease_until<=clock_timestamp() OR o.lease_token_hash IS DISTINCT FROM sha256(p_lease_token) THEN
  RAISE EXCEPTION 'Meta credential lease conflict' USING ERRCODE='40001';
 END IF;
 IF b.id IS NULL OR b.id<>o.binding_id OR b.provider<>o.provider OR b.external_asset_id<>o.external_asset_id THEN
  RAISE EXCEPTION 'Meta credential binding mismatch' USING ERRCODE='PT409';
 END IF;
 SELECT h.current_version INTO v_head FROM integration.meta_page_heads h
  WHERE h.tenant_id=o.tenant_id AND h.store_id=o.store_id AND h.binding_id=o.binding_id;
 IF NOT FOUND THEN RETURN; END IF;   -- no credential: zero rows -> BLOCKED_POLICY at the dispatcher
 -- Recheck after waits so an expired claim cannot release secret material.
 IF o.lease_until<=clock_timestamp() THEN RAISE EXCEPTION 'Meta credential lease conflict' USING ERRCODE='40001'; END IF;
 RETURN QUERY SELECT c.tenant_id,c.store_id,c.binding_id,c.provider,c.asset_id,c.version,c.key_id,c.nonce,c.ciphertext,c.scopes_attested
  FROM integration.meta_page_credentials c WHERE c.tenant_id=o.tenant_id AND c.store_id=o.store_id
   AND c.binding_id=o.binding_id AND c.version=v_head;
END $$;
ALTER FUNCTION integration.load_meta_page_token(uuid,bigint,bytea) OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION integration.load_meta_page_token(uuid,bigint,bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION integration.load_meta_page_token(uuid,bigint,bytea) TO commerce_worker;
COMMENT ON FUNCTION integration.load_meta_page_token(uuid,bigint,bytea) IS
 'integration owner; only caller the dispatcher LoadSecret hook of internal/integrations/metareply (commerce_worker). Lease-fenced like load_stripe_credential: returns the current head credential (ciphertext, scopes_attested) of the operation''s frozen binding in dispatch mode only; zero rows = no credential; never plaintext.';

CREATE FUNCTION integration.register_meta_page_token(p_tenant uuid,p_store uuid,p_principal uuid,p_binding uuid,
 p_provider text,p_asset_id text,p_expected_version bigint,p_key_id text,p_nonce bytea,p_ciphertext bytea,p_scopes text[])
RETURNS bigint LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE b record; v_head bigint; v_next bigint;
BEGIN
 IF p_tenant IS NULL OR p_store IS NULL OR p_principal IS NULL OR p_binding IS NULL
  OR p_provider IS NULL OR p_provider NOT IN ('facebook','instagram') OR p_asset_id IS NULL OR p_asset_id !~ '^[0-9]{1,40}$'
  OR p_expected_version IS NULL OR p_expected_version<0 OR p_expected_version>=9223372036854775807
  OR p_key_id IS NULL OR p_key_id !~ '^[A-Za-z0-9_-]{1,64}$'
  OR p_nonce IS NULL OR octet_length(p_nonce)<>12 OR p_ciphertext IS NULL OR octet_length(p_ciphertext) NOT BETWEEN 17 AND 8192
  OR p_scopes IS NULL OR array_ndims(p_scopes)<>1 OR cardinality(p_scopes) NOT BETWEEN 1 AND 16
  OR array_to_string(p_scopes,',') !~ '^[a-z_]{1,64}(,[a-z_]{1,64}){0,15}$'
  OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid Meta page token input' USING ERRCODE='22023';
 END IF;
 -- Contract §7 "owner membership + store validated": active principal, tenant, store, membership and an
 -- integration:manage store grant (the require_stripe_registrar_scope rule) before anything is pinned or written.
 IF NOT identity.principal_holds(p_tenant,p_store,p_principal,ARRAY['integration:manage']) THEN
  RAISE EXCEPTION 'Meta registrar scope unavailable' USING ERRCODE='42501';
 END IF;
 -- Provider and asset must be exactly the binding's (an asset change is a new binding).
 SELECT x.id,x.provider,x.external_asset_id INTO b FROM integration.bindings x
  WHERE x.tenant_id=p_tenant AND x.store_id=p_store AND x.id=p_binding;
 IF NOT FOUND OR b.provider<>p_provider OR b.external_asset_id<>p_asset_id THEN
  RAISE EXCEPTION 'invalid Meta page token input' USING ERRCODE='22023';
 END IF;
 -- Pin the audit scope (as register_stripe_account); the principal was validated above.
 PERFORM set_config('app.tenant_id',p_tenant::text,true);
 PERFORM set_config('app.store_id',p_store::text,true);
 PERFORM set_config('app.principal_id',p_principal::text,true);
 IF p_expected_version=0 THEN
  IF EXISTS(SELECT 1 FROM integration.meta_page_heads h WHERE h.tenant_id=p_tenant AND h.store_id=p_store AND h.binding_id=p_binding) THEN
   RAISE EXCEPTION 'Meta page token version changed' USING ERRCODE='PT409';
  END IF;
  INSERT INTO integration.meta_page_heads(tenant_id,store_id,binding_id,current_version) VALUES(p_tenant,p_store,p_binding,1);
  v_next:=1;
 ELSE
  SELECT h.current_version INTO v_head FROM integration.meta_page_heads h
   WHERE h.tenant_id=p_tenant AND h.store_id=p_store AND h.binding_id=p_binding FOR UPDATE;
  IF NOT FOUND OR v_head<>p_expected_version THEN
   RAISE EXCEPTION 'Meta page token version changed' USING ERRCODE='PT409';
  END IF;
  v_next:=p_expected_version+1;
  UPDATE integration.meta_page_heads h SET current_version=v_next,updated_at=clock_timestamp()
   WHERE h.tenant_id=p_tenant AND h.store_id=p_store AND h.binding_id=p_binding;
 END IF;
 INSERT INTO integration.meta_page_credentials(tenant_id,store_id,binding_id,provider,asset_id,version,key_id,nonce,
  ciphertext,scopes_attested,principal_id)
 VALUES(p_tenant,p_store,p_binding,p_provider,p_asset_id,v_next,p_key_id,p_nonce,p_ciphertext,p_scopes,p_principal);
 INSERT INTO ops.audit_events(tenant_id,store_id,principal_id,action)
  VALUES(p_tenant,p_store,p_principal,'meta.page_token_registered');
 RETURN v_next;
END $$;
ALTER FUNCTION integration.register_meta_page_token(uuid,uuid,uuid,uuid,text,text,bigint,text,bytea,bytea,text[]) OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION integration.register_meta_page_token(uuid,uuid,uuid,uuid,text,text,bigint,text,bytea,bytea,text[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION integration.register_meta_page_token(uuid,uuid,uuid,uuid,text,text,bigint,text,bytea,bytea,text[]) TO commerce_meta_registrar;
COMMENT ON FUNCTION integration.register_meta_page_token(uuid,uuid,uuid,uuid,text,text,bigint,text,bytea,bytea,text[]) IS
 'integration owner; only caller cmd/meta-admin page-token (commerce_meta_registrar). Appends the encrypted Page token version (CAS on expected version, 0 creates the head) for one binding; provider/asset must equal the binding; requires an active principal holding integration:manage on the store (42501 otherwise); audited. Token plaintext never reaches SQL; scopes_attested is the operator attestation.';

-- ---------------------------------------------------------------------------------------
-- Documentation.
-- ---------------------------------------------------------------------------------------
COMMENT ON TABLE live.claim_window_intervals IS
 'internal/claims: history of OPEN claim windows (one row per generation). Written only by trigger live.track_claim_window_interval (commerce_claims_writer); read by commerce_claims_intake under IS.';
COMMENT ON TABLE live.claim_sources IS
 'internal/claims: binds one Meta object (FB post/live video, IG media) to one live session (T10c §2). Written only by live.put_claim_source and the intake staging counters; read by commerce_runtime under G.';
COMMENT ON TABLE claims.meta_intake IS
 'internal/claims: text-free staging queue, one row per Meta comment (T10c §4). Written by claims.insert_meta_intake (consumer tx) and the intake worker under lease; no comment text, unresolved keyword or raw sender id.';
COMMENT ON TABLE integration.meta_page_credentials IS
 'internal/integrations/metareply: encrypted Meta Page access token versions (AES-256-GCM, AAD meta-page-token/v1). Written by integration.register_meta_page_token; read only by integration.load_meta_page_token; no plaintext, no runtime SELECT.';
COMMENT ON TABLE integration.meta_page_heads IS
 'internal/integrations/metareply: current Page-token version per binding. Written by integration.register_meta_page_token; commerce_claims_writer reads only current_version for the private_reply check.';
COMMENT ON COLUMN claims.meta_intake.actor_key IS 'Pseudonymous personal data (arch §14.4): keyed HMAC of the Meta sender id; never returned by any API. Retention class: actor keys (T14).';
COMMENT ON COLUMN claims.meta_intake.comment_ref IS 'Platform comment id (IR-4): needed as recipient.comment_id of the private reply; not comment text. Retention class: comment refs (T14).';
COMMENT ON COLUMN live.claim_sources.source_object_id IS 'Platform object id of the live post/media (pseudonymous platform data). Retention class: claim sources (T14).';
COMMENT ON COLUMN integration.meta_page_credentials.ciphertext IS 'AES-256-GCM sealed Meta Page access token; only integration.load_meta_page_token (owner commerce_integration_writer) reads it.';

-- Column documentation for the five new tables (PROCESS.md §5 "every new column gets COMMENT ON";
-- MCI02 definer_hygiene_and_COMMENT_ON). Columns documented individually above keep their text.
DO $$
DECLARE r record; c text;
BEGIN
 FOR r IN SELECT * FROM (VALUES
  ('live.claim_window_intervals','tenant_id,store_id,session_id',
   'Scope and parent window (FK live.claim_windows, ON DELETE CASCADE); RLS IS for commerce_claims_intake.'),
  ('live.claim_window_intervals','generation,opened_at,closed_at',
   'One OPEN interval of the window generation (closed_at NULL while open); written only by trigger live.track_claim_window_interval, read by the intake worker to decide window_closed drops (MCI06).'),
  ('live.claim_sources','tenant_id,store_id,id,session_id',
   'Scope, row id and the live session (FK live.claim_windows) this Meta object feeds; written only by live.put_claim_source.'),
  ('live.claim_sources','platform,object,asset_id,binding_id,binding_version',
   'The store''s enabled Meta binding (provider, Page/IG asset, semantic version at bind time) the object belongs to; platform facebook <=> object page.'),
  ('live.claim_sources','private_reply,reply_locale,active,version,principal_id',
   'Merchant settings: automated first private reply on/off and its language, collecting on/off, CAS version (live.put_claim_source), and the merchant who last saved it.'),
  ('live.claim_sources','intake_count,intake_capped,created_at,updated_at',
   'Staging counters maintained by claims.insert_meta_intake (comments staged / skipped at the §4.2 intake cap) and row timestamps; no comment text.'),
  ('claims.meta_intake','tenant_id,store_id,id,inbox_event_id,source_id,session_id',
   'Scope, row id, the meta_inbox event it was staged from (one intake per event) and the claim source/session it maps to.'),
  ('claims.meta_intake','platform,app_id,object,asset_id,live_media,occurred_at,received_at',
   'Delivery facts of the Meta comment (app, Page/IG asset, live-media flag, comment time and receive time); text-free.'),
  ('claims.meta_intake','grammar_version,grammar_kind,offer_id,unknown_keyword,quantity,explicit_quantity',
   'Result of the kw-v1 keyword grammar computed in the consumer transaction (matched offer, quantity); the comment text itself is never stored.'),
  ('claims.meta_intake','state,drop_reason,fail_code,attempts,not_before,applied_event_id,lease_xid,created_at,updated_at',
   'Staging state machine for the intake worker (PENDING -> APPLIED/DROPPED/FAILED, retries bounded at 10, lease by transaction id).'),
  ('integration.meta_page_credentials','tenant_id,store_id,binding_id,provider,asset_id,version',
   'Binding (FK integration.bindings provider/asset) and append-only Page-token version; written only by integration.register_meta_page_token.'),
  ('integration.meta_page_credentials','key_id,nonce,scopes_attested,principal_id,created_at',
   'Sealing key id and AES-GCM nonce of the ciphertext, operator-attested token scopes and the registering principal; never a plaintext token.'),
  ('integration.meta_page_heads','tenant_id,store_id,binding_id,current_version,updated_at',
   'Current Page-token version per binding (CAS head) and its last change; commerce_claims_writer reads only current_version for the private_reply check.')
 ) AS v(tbl,cols,why)
 LOOP
  FOREACH c IN ARRAY string_to_array(r.cols,',') LOOP
   IF col_description(r.tbl::regclass,(SELECT a.attnum FROM pg_attribute a WHERE a.attrelid=r.tbl::regclass AND a.attname=c)) IS NULL THEN
    EXECUTE format('COMMENT ON COLUMN %s.%I IS %L',r.tbl,c,r.why);
   END IF;
  END LOOP;
 END LOOP;
END $$;
