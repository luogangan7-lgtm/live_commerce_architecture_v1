-- 0074 Meta ads core (T15; contracts/meta-ads-v1.md §4.1 + §4.4, FROZEN 2026-09-30; unit ads-core,
-- docs/delivery/units/ads-core.md D1-D14, brief rulings B9/B12-B15 in docs/delivery/units/r2-design-rulings.md).
--
-- Owns: schema ads (store settings, OAuth states, connections, campaign drafts, approvals, remote-object
-- pins, operator settings audit), the NOLOGIN definer role commerce_ads_writer, the widened
-- integration.meta_page_credentials custody CHECKs, the three ads:* permissions, the ads token registrar and
-- loader (integration schema) and every ads.* definer function below.
--
-- Non-goals: no Graph call, River job or network from SQL (River guards are post_river/0015); no CAPI, feed,
-- consent or billing table (0080 is ads-capi's; ads definers call billing.store_standing by name at run time
-- and fail closed until 0080 grants it); no store_grants rows (A-1: explicit provisioning only); no plaintext
-- token anywhere; no budget change of an existing remote object; no re-activation (ruling X7).
--
-- Depends on: 0001/0003 (identity, resolve_access), 0008/0016/0018 (integration.operations ledger and the
-- operation_actor_family CHECK), 0014 (bindings), 0020 (storefront), 0064 (Meta page credential custody,
-- principal_holds), 0062 (audit style).
--
-- Callers (each function's COMMENT repeats its only caller):
--   merchant tx (commerce_runtime, internal/ads.Service through internal/httpapi/ads.go): ads.begin_connect,
--     consume_state, put_connect_result, get_state, connection_version, finish_bind, get_settings, create_draft,
--     update_draft, list_drafts, get_draft, approve_draft, publish_draft, pause_prepare/pause_plan/pause_view,
--     set_capi (report is 0075); integration.register_meta_ads_token
--   dispatcher pool (commerce_worker, internal/ads.Checker + sweepers): ads.check_create/check_activate/
--     check_read, advance_candidates/advance_next/advance_plan, insights_candidates/insights_days/
--     insights_plan, pending_insight_reads, put_insights_day, purge_oauth_states, canonical_draft;
--     integration.load_meta_ads_token
--   operator registrar (commerce_meta_registrar, cmd/meta-admin ads-settings): ads.operator_set_settings
--
-- Security decisions:
--  * Every merchant definer takes the session hash and re-runs identity.resolve_access itself (ads.auth), then
--    requires the result to equal this transaction's app.* GUC scope; no caller-supplied principal exists.
--  * commerce_ads_writer has table privileges only on the ads schema, plus the read/insert rows of contract
--    §4.4. A SELECT grant without a policy reads zero rows under FORCE RLS (allowance sums would fail open),
--    so every grant below has a named policy and MA02 asserts each.
--  * Custom SQLSTATE ADnnn (nnn = HTTP status) with MESSAGE = the frozen error code carries every domain
--    refusal; Go maps them without ever returning a driver message.
--  * Every definer pins search_path=pg_catalog, REVOKEs PUBLIC and filters tenant/store from the resolved
--    scope or the locked row.
--  * The billing standing check (BD5) is a run-time call: until 0080 grants billing.store_standing to
--    commerce_ads_writer the approve/publish/create/activate definers raise (fail closed, never fail open).
--
-- Gaps in contract §4.4 found while writing this file (each is an EXTRA row the integrator must rule and add
-- to §4.4 and MA02; listed in output/ads-core/integrator-hooks.patch header): R-C storefront/catalog reads for
-- PRODUCT_TRAFFIC validation and link freezing (control.storefront_domains, control.storefront_publications,
-- catalog.products, column-level + read policy); post_river/0015 SELECT on river.river_job (job verification,
-- plan_claim_reply pattern).

-- ---------------------------------------------------------------------------------------
-- Permission vocabulary (A-1: no grant rows). Re-derived from the live definition so that a sibling unit's
-- widening (0072, 0078) is never dropped.
-- ---------------------------------------------------------------------------------------
DO $$
DECLARE v_def text;
BEGIN
 SELECT pg_get_constraintdef(c.oid) INTO v_def FROM pg_constraint c
  WHERE c.conrelid='identity.store_grants'::regclass AND c.conname='store_grants_permission_check';
 IF v_def IS NULL OR v_def NOT LIKE '%''integration:read''%' OR v_def LIKE '%ads:read%' OR v_def !~ '::text\]\)' THEN
  RAISE EXCEPTION 'store_grants_permission_check has an unexpected shape: %',v_def;
 END IF;
 v_def:=regexp_replace(v_def,'::text\]\)','::text, ''ads:read''::text, ''ads:manage''::text, ''ads:approve''::text])');
 ALTER TABLE identity.store_grants DROP CONSTRAINT store_grants_permission_check;
 EXECUTE format('ALTER TABLE identity.store_grants ADD CONSTRAINT store_grants_permission_check %s',v_def);
END $$;

-- ---------------------------------------------------------------------------------------
-- Custody widening (§4.1): ads rows reuse integration.meta_page_credentials. The old provider CHECK admits
-- facebook/instagram only; the old nonce CHECK admits AES-GCM's 12 bytes only (ads rows carry the 32-byte
-- HPKE encapsulated key). Both are found by their live definition, asserted, and replaced.
-- ---------------------------------------------------------------------------------------
DO $$
DECLARE r record; v_provider int:=0; v_nonce int:=0;
BEGIN
 FOR r IN SELECT c.conname,pg_get_constraintdef(c.oid) AS def FROM pg_constraint c
  WHERE c.conrelid='integration.meta_page_credentials'::regclass AND c.contype='c' LOOP
  IF r.def LIKE 'CHECK ((provider = ANY (ARRAY[''facebook''::text, ''instagram''::text])))' THEN
   EXECUTE format('ALTER TABLE integration.meta_page_credentials DROP CONSTRAINT %I',r.conname); v_provider:=v_provider+1;
  ELSIF r.def LIKE 'CHECK ((octet_length(nonce) = 12))' THEN
   EXECUTE format('ALTER TABLE integration.meta_page_credentials DROP CONSTRAINT %I',r.conname); v_nonce:=v_nonce+1;
  END IF;
 END LOOP;
 IF v_provider<>1 OR v_nonce<>1 THEN
  RAISE EXCEPTION '0074 expected exactly one provider and one nonce CHECK on integration.meta_page_credentials (found %/%)',v_provider,v_nonce;
 END IF;
END $$;
ALTER TABLE integration.meta_page_credentials ADD CONSTRAINT meta_page_credentials_provider_check
 CHECK (provider IN ('facebook','instagram','meta_ads','meta_dataset'));
ALTER TABLE integration.meta_page_credentials ADD CONSTRAINT meta_page_credentials_nonce_check
 CHECK ((provider IN ('facebook','instagram') AND octet_length(nonce)=12)
     OR (provider IN ('meta_ads','meta_dataset') AND octet_length(nonce)=32));

-- ---------------------------------------------------------------------------------------
-- Role, schema, schema USAGE (§4.4 rows 1, 8, 9; commerce_meta_registrar for the operator definer, ruling B12).
-- ---------------------------------------------------------------------------------------
CREATE ROLE commerce_ads_writer NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
COMMENT ON ROLE commerce_ads_writer IS
 'T15 ads definer owner (migrations/0074). NOLOGIN; owns every ads.* definer and table access under FORCE RLS policies of its own. No runtime login may reach it (platform validatePoolAuthority object-owner rule).';

CREATE SCHEMA ads;
REVOKE ALL ON SCHEMA ads FROM PUBLIC;
COMMENT ON SCHEMA ads IS
 'T15 Meta ads (internal/ads). Every write and every merchant read goes through an ads.* SECURITY DEFINER function owned by commerce_ads_writer; roles hold EXECUTE only, never table privileges (contract 4.4).';
GRANT USAGE ON SCHEMA ads TO commerce_runtime, commerce_worker, commerce_buyer_runtime, commerce_integration_writer,
 commerce_ads_writer, commerce_meta_registrar;
-- ads_writer reads/writes these schemas only through the named grants below.
GRANT USAGE ON SCHEMA identity, integration, payments, control, catalog TO commerce_ads_writer;

-- ---------------------------------------------------------------------------------------
-- Tables (§4.1). All FORCE RLS, PUBLIC revoked, no runtime grants.
-- ---------------------------------------------------------------------------------------
CREATE TABLE ads.store_settings (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, PRIMARY KEY(tenant_id,store_id),
 environment text NOT NULL DEFAULT 'SANDBOX' CHECK(environment IN ('SANDBOX','LIVE')),
 sandbox_ad_account text CHECK(sandbox_ad_account ~ '^[0-9]{1,40}$'),
 max_active_budget_minor bigint NOT NULL DEFAULT 0 CHECK(max_active_budget_minor BETWEEN 0 AND 100000000000),
 allowance_currency text NOT NULL DEFAULT 'TWD' CHECK(allowance_currency IN ('TWD','USD','HKD')),
 capi_enabled boolean NOT NULL DEFAULT false, capi_dataset_binding uuid,
 capi_enabled_by uuid,
 capi_test_event_code text CHECK(capi_test_event_code ~ '^[A-Z0-9]{4,20}$'),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 FOREIGN KEY(tenant_id,store_id) REFERENCES control.stores(tenant_id,id),
 FOREIGN KEY(tenant_id,store_id,capi_dataset_binding) REFERENCES integration.bindings(tenant_id,store_id,id),
 FOREIGN KEY(tenant_id,capi_enabled_by) REFERENCES identity.memberships(tenant_id,principal_id),
 CHECK(NOT capi_enabled OR (capi_dataset_binding IS NOT NULL AND capi_enabled_by IS NOT NULL)),
 CHECK(NOT capi_enabled OR environment='LIVE' OR capi_test_event_code IS NOT NULL));
CREATE TABLE ads.oauth_states (
 id uuid PRIMARY KEY, tenant_id uuid NOT NULL, store_id uuid NOT NULL, principal_id uuid NOT NULL,
 state_hash bytea NOT NULL UNIQUE CHECK(octet_length(state_hash)=32),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(), used_at timestamptz,
 expires_at timestamptz NOT NULL,
 client_business_id text CHECK(client_business_id ~ '^[0-9]{1,40}$'),
 pick_list jsonb CHECK(pick_list IS NULL OR (jsonb_typeof(pick_list)='array' AND octet_length(pick_list::text)<=16384)),
 scopes_attested text[],
 pending_key_id text CHECK(pending_key_id ~ '^[A-Za-z0-9_-]{1,64}$'),
 pending_enc bytea CHECK(octet_length(pending_enc)=32),
 pending_ciphertext bytea CHECK(octet_length(pending_ciphertext) BETWEEN 17 AND 8192),
 CHECK(used_at IS NULL OR used_at >= created_at), CHECK(expires_at > created_at),
 CHECK((pending_ciphertext IS NULL) = (pending_enc IS NULL) AND (pending_enc IS NULL) = (pending_key_id IS NULL)),
 UNIQUE(tenant_id,store_id,id),
 FOREIGN KEY(tenant_id,store_id) REFERENCES control.stores(tenant_id,id),
 FOREIGN KEY(tenant_id,principal_id) REFERENCES identity.memberships(tenant_id,principal_id));
CREATE INDEX oauth_states_expiry ON ads.oauth_states(expires_at) WHERE pending_ciphertext IS NOT NULL;
CREATE TABLE ads.connections (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, binding_id uuid NOT NULL,
 client_business_id text NOT NULL CHECK(client_business_id ~ '^[0-9]{1,40}$'),
 token_version bigint NOT NULL CHECK(token_version>0),
 oauth_state_id uuid NOT NULL REFERENCES ads.oauth_states(id), connected_by uuid NOT NULL,
 connected_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,store_id,binding_id),
 FOREIGN KEY(tenant_id,store_id,binding_id) REFERENCES integration.bindings(tenant_id,store_id,id),
 FOREIGN KEY(tenant_id,connected_by) REFERENCES identity.memberships(tenant_id,principal_id));
CREATE TABLE ads.campaign_drafts (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, id uuid PRIMARY KEY,
 ad_binding_id uuid NOT NULL, ad_binding_version bigint NOT NULL CHECK(ad_binding_version>0),
 identity_binding_id uuid NOT NULL,
 template text NOT NULL CHECK(template IN ('BOOST_POST','PRODUCT_TRAFFIC')),
 source_ref text NOT NULL CHECK(source_ref ~ '^[0-9_]{1,80}$' OR source_ref ~ '^[0-9a-f-]{36}$'),
 currency text NOT NULL CHECK(currency IN ('TWD','USD','HKD')),
 lifetime_budget_minor bigint NOT NULL CHECK(lifetime_budget_minor > 0),
 starts_at timestamptz NOT NULL, ends_at timestamptz NOT NULL CHECK(ends_at > starts_at AND ends_at <= starts_at + interval '30 days'),
 countries text[] NOT NULL CHECK(cardinality(countries) BETWEEN 1 AND 10),
 age_min smallint NOT NULL CHECK(age_min BETWEEN 18 AND 65), age_max smallint NOT NULL CHECK(age_max BETWEEN age_min AND 65),
 revision integer NOT NULL DEFAULT 1 CHECK(revision BETWEEN 1 AND 1000),
 publish_attempt integer NOT NULL DEFAULT 0 CHECK(publish_attempt BETWEEN 0 AND 5),
 ended_at timestamptz, created_by uuid NOT NULL, created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK(currency<>'TWD' OR lifetime_budget_minor % 100 = 0),
 UNIQUE(tenant_id,store_id,id),
 FOREIGN KEY(tenant_id,store_id,ad_binding_id) REFERENCES integration.bindings(tenant_id,store_id,id),
 FOREIGN KEY(tenant_id,store_id,identity_binding_id) REFERENCES integration.bindings(tenant_id,store_id,id),
 FOREIGN KEY(tenant_id,created_by) REFERENCES identity.memberships(tenant_id,principal_id));
CREATE INDEX campaign_drafts_store ON ads.campaign_drafts(tenant_id,store_id,created_at DESC);
CREATE TABLE ads.draft_approvals (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, draft_id uuid NOT NULL, revision integer NOT NULL,
 draft_sha256 bytea NOT NULL CHECK(octet_length(draft_sha256)=32),
 principal_id uuid NOT NULL, approved_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,store_id,draft_id,revision),
 FOREIGN KEY(tenant_id,store_id,draft_id) REFERENCES ads.campaign_drafts(tenant_id,store_id,id),
 FOREIGN KEY(tenant_id,principal_id) REFERENCES identity.memberships(tenant_id,principal_id));
CREATE TABLE ads.remote_objects (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, draft_id uuid NOT NULL, publish_attempt integer NOT NULL,
 kind text NOT NULL CHECK(kind IN ('campaign','adset','creative','ad','preflight','activate','pause')),
 seq integer NOT NULL DEFAULT 1 CHECK(seq BETWEEN 1 AND 50),
 operation_id uuid NOT NULL UNIQUE, remote_id text CHECK(remote_id ~ '^[0-9]{1,40}$'),
 PRIMARY KEY(tenant_id,store_id,draft_id,publish_attempt,kind,seq),
 FOREIGN KEY(tenant_id,store_id,operation_id) REFERENCES integration.operations(tenant_id,store_id,id),
 FOREIGN KEY(tenant_id,store_id,draft_id) REFERENCES ads.campaign_drafts(tenant_id,store_id,id));
-- Operator audit (D7): integration/ops audit tables need a principal, which the operator CLI does not have,
-- so operator changes are recorded here (append-only, ads-owned).
CREATE TABLE ads.operator_events (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 tenant_id uuid NOT NULL, store_id uuid NOT NULL,
 action text NOT NULL CHECK(action='ads.operator_settings_changed'),
 environment text NOT NULL, sandbox_ad_account text, max_active_budget_minor bigint NOT NULL, allowance_currency text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 FOREIGN KEY(tenant_id,store_id) REFERENCES control.stores(tenant_id,id));

ALTER TABLE ads.store_settings ENABLE ROW LEVEL SECURITY;      ALTER TABLE ads.store_settings FORCE ROW LEVEL SECURITY;
ALTER TABLE ads.oauth_states ENABLE ROW LEVEL SECURITY;        ALTER TABLE ads.oauth_states FORCE ROW LEVEL SECURITY;
ALTER TABLE ads.connections ENABLE ROW LEVEL SECURITY;         ALTER TABLE ads.connections FORCE ROW LEVEL SECURITY;
ALTER TABLE ads.campaign_drafts ENABLE ROW LEVEL SECURITY;     ALTER TABLE ads.campaign_drafts FORCE ROW LEVEL SECURITY;
ALTER TABLE ads.draft_approvals ENABLE ROW LEVEL SECURITY;     ALTER TABLE ads.draft_approvals FORCE ROW LEVEL SECURITY;
ALTER TABLE ads.remote_objects ENABLE ROW LEVEL SECURITY;      ALTER TABLE ads.remote_objects FORCE ROW LEVEL SECURITY;
ALTER TABLE ads.operator_events ENABLE ROW LEVEL SECURITY;     ALTER TABLE ads.operator_events FORCE ROW LEVEL SECURITY;
REVOKE ALL ON ads.store_settings, ads.oauth_states, ads.connections, ads.campaign_drafts, ads.draft_approvals,
 ads.remote_objects, ads.operator_events FROM PUBLIC;

-- Frozen columns of a draft change only while no approval exists for the current revision, and every such edit
-- bumps the revision (§4.1). publish_attempt, ended_at and the row identity are outside the freeze.
CREATE FUNCTION ads.guard_draft_update() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF NEW.id<>OLD.id OR NEW.tenant_id<>OLD.tenant_id OR NEW.store_id<>OLD.store_id OR NEW.created_by<>OLD.created_by
  OR NEW.created_at<>OLD.created_at THEN
  RAISE EXCEPTION 'immutable draft identity' USING ERRCODE='23514';
 END IF;
 IF (NEW.ad_binding_id,NEW.ad_binding_version,NEW.identity_binding_id,NEW.template,NEW.source_ref,NEW.currency,
     NEW.lifetime_budget_minor,NEW.starts_at,NEW.ends_at,NEW.countries,NEW.age_min,NEW.age_max)
  IS DISTINCT FROM
    (OLD.ad_binding_id,OLD.ad_binding_version,OLD.identity_binding_id,OLD.template,OLD.source_ref,OLD.currency,
     OLD.lifetime_budget_minor,OLD.starts_at,OLD.ends_at,OLD.countries,OLD.age_min,OLD.age_max) THEN
  IF NEW.revision<>OLD.revision+1 OR OLD.publish_attempt<>0 OR NEW.publish_attempt<>0
   OR EXISTS(SELECT 1 FROM ads.draft_approvals a WHERE a.tenant_id=OLD.tenant_id AND a.store_id=OLD.store_id
    AND a.draft_id=OLD.id AND a.revision=OLD.revision) THEN
   RAISE EXCEPTION 'draft frozen' USING ERRCODE='23514';
  END IF;
 ELSIF NEW.revision<>OLD.revision THEN
  RAISE EXCEPTION 'revision changes only with an edit' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER campaign_drafts_guard BEFORE UPDATE ON ads.campaign_drafts
 FOR EACH ROW EXECUTE FUNCTION ads.guard_draft_update();
COMMENT ON FUNCTION ads.guard_draft_update() IS
 'internal/ads trigger function of ads.campaign_drafts only; no caller EXECUTE. Freezes the approved columns (an edit needs no approval for the current revision, bumps revision by one, and never follows a publish) and pins the row identity.';

-- remote_id is set once, NULL -> value (copied from the operation's provider_reference by the advance sweeper).
CREATE FUNCTION ads.guard_remote_id() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF OLD.remote_id IS NOT NULL AND NEW.remote_id IS DISTINCT FROM OLD.remote_id THEN
  RAISE EXCEPTION 'remote id is set once' USING ERRCODE='23514';
 END IF;
 IF (NEW.tenant_id,NEW.store_id,NEW.draft_id,NEW.publish_attempt,NEW.kind,NEW.seq,NEW.operation_id)
  IS DISTINCT FROM (OLD.tenant_id,OLD.store_id,OLD.draft_id,OLD.publish_attempt,OLD.kind,OLD.seq,OLD.operation_id) THEN
  RAISE EXCEPTION 'immutable remote object identity' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER remote_objects_guard BEFORE UPDATE ON ads.remote_objects
 FOR EACH ROW EXECUTE FUNCTION ads.guard_remote_id();
COMMENT ON FUNCTION ads.guard_remote_id() IS
 'internal/ads trigger function of ads.remote_objects only; no caller EXECUTE. remote_id is written once (NULL to value); the step identity never changes.';
REVOKE ALL ON FUNCTION ads.guard_draft_update() FROM PUBLIC;
REVOKE ALL ON FUNCTION ads.guard_remote_id() FROM PUBLIC;
ALTER FUNCTION ads.guard_draft_update() OWNER TO commerce_ads_writer;
ALTER FUNCTION ads.guard_remote_id() OWNER TO commerce_ads_writer;

-- ---------------------------------------------------------------------------------------
-- Privilege delta (contract §4.4 + brief rulings B12; each grant names the one policy that makes it readable).
-- ---------------------------------------------------------------------------------------
-- commerce_ads_writer on its own schema: definer bodies are the control (0064 page_credential_writer pattern).
GRANT SELECT, INSERT, UPDATE(environment,sandbox_ad_account,max_active_budget_minor,allowance_currency,capi_enabled,
 capi_dataset_binding,capi_enabled_by,capi_test_event_code,updated_at) ON ads.store_settings TO commerce_ads_writer;
GRANT SELECT, INSERT, UPDATE(used_at,client_business_id,pick_list,scopes_attested,pending_key_id,pending_enc,pending_ciphertext)
 ON ads.oauth_states TO commerce_ads_writer;
GRANT SELECT ON ads.connections TO commerce_ads_writer;
GRANT SELECT, INSERT, UPDATE(ad_binding_id,ad_binding_version,identity_binding_id,template,source_ref,currency,
 lifetime_budget_minor,starts_at,ends_at,countries,age_min,age_max,revision,publish_attempt,ended_at) ON ads.campaign_drafts TO commerce_ads_writer;
GRANT SELECT, INSERT ON ads.draft_approvals TO commerce_ads_writer;
GRANT SELECT, INSERT, UPDATE(remote_id) ON ads.remote_objects TO commerce_ads_writer;
GRANT INSERT ON ads.operator_events TO commerce_ads_writer;
GRANT USAGE ON SEQUENCE ads.operator_events_id_seq TO commerce_ads_writer;
CREATE POLICY ads_writer_all ON ads.store_settings FOR ALL TO commerce_ads_writer USING (true) WITH CHECK (true);
CREATE POLICY ads_writer_all ON ads.oauth_states FOR ALL TO commerce_ads_writer USING (true) WITH CHECK (true);
CREATE POLICY ads_writer_read ON ads.connections FOR SELECT TO commerce_ads_writer USING (true);
CREATE POLICY ads_writer_all ON ads.campaign_drafts FOR ALL TO commerce_ads_writer USING (true) WITH CHECK (true);
CREATE POLICY ads_writer_all ON ads.draft_approvals FOR ALL TO commerce_ads_writer USING (true) WITH CHECK (true);
CREATE POLICY ads_writer_all ON ads.remote_objects FOR ALL TO commerce_ads_writer USING (true) WITH CHECK (true);
CREATE POLICY ads_writer_insert ON ads.operator_events FOR INSERT TO commerce_ads_writer WITH CHECK (true);

-- commerce_integration_writer: register_meta_ads_token copies the sealed token and writes the connection under the
-- GUC scope the function pins from identity.resolve_access (0064 meta_page_token_audit pattern). The UPDATE
-- grant exists for SELECT ... FOR SHARE on the state row (source_reply_lock pattern) and for re-connect of the
-- same binding; WITH CHECK (false) forbids any oauth_states mutation.
GRANT SELECT, UPDATE(used_at) ON ads.oauth_states TO commerce_integration_writer;
CREATE POLICY integration_writer_state_read ON ads.oauth_states FOR SELECT TO commerce_integration_writer
 USING (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND principal_id=nullif(current_setting('app.principal_id',true),'')::uuid);
CREATE POLICY integration_writer_state_lock ON ads.oauth_states FOR UPDATE TO commerce_integration_writer
 USING (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND principal_id=nullif(current_setting('app.principal_id',true),'')::uuid)
 WITH CHECK (false);
GRANT SELECT, INSERT, UPDATE(token_version,oauth_state_id,connected_by,connected_at) ON ads.connections TO commerce_integration_writer;
CREATE POLICY integration_writer_connection_read ON ads.connections FOR SELECT TO commerce_integration_writer
 USING (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
CREATE POLICY integration_writer_connection_insert ON ads.connections FOR INSERT TO commerce_integration_writer
 WITH CHECK (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND connected_by=nullif(current_setting('app.principal_id',true),'')::uuid);
CREATE POLICY integration_writer_connection_update ON ads.connections FOR UPDATE TO commerce_integration_writer
 USING (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid)
 WITH CHECK (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND connected_by=nullif(current_setting('app.principal_id',true),'')::uuid);
GRANT EXECUTE ON FUNCTION identity.resolve_access(bytea,uuid,text) TO commerce_integration_writer, commerce_ads_writer;
GRANT EXECUTE ON FUNCTION identity.principal_holds(uuid,uuid,uuid,text[]) TO commerce_ads_writer;

-- commerce_ads_writer cross-domain reads/inserts (§4.4 rows 3-4). A SELECT grant WITHOUT a policy would read zero
-- rows under FORCE RLS and make the allowance sum empty (= fail open); each grant below has its policy.
GRANT SELECT ON integration.operations TO commerce_ads_writer;
CREATE POLICY ads_writer_operation_read ON integration.operations FOR SELECT TO commerce_ads_writer
 USING (provider IN ('meta_ads','meta_dataset'));
GRANT SELECT ON integration.operation_events TO commerce_ads_writer;
CREATE POLICY ads_writer_event_read ON integration.operation_events FOR SELECT TO commerce_ads_writer
 USING (EXISTS (SELECT 1 FROM integration.operations o WHERE o.id=operation_events.operation_id
  AND o.tenant_id=operation_events.tenant_id AND o.store_id=operation_events.store_id
  AND o.provider IN ('meta_ads','meta_dataset')));
GRANT SELECT ON integration.bindings TO commerce_ads_writer;
CREATE POLICY ads_writer_binding_read ON integration.bindings FOR SELECT TO commerce_ads_writer
 USING (provider IN ('meta_ads','meta_dataset','facebook','instagram'));
GRANT INSERT ON integration.operations, integration.operation_events TO commerce_ads_writer;
GRANT USAGE ON SEQUENCE integration.operation_events_id_seq TO commerce_ads_writer;
CREATE POLICY ads_writer_operation_insert ON integration.operations FOR INSERT TO commerce_ads_writer
 WITH CHECK (provider IN ('meta_ads','meta_dataset') AND (action LIKE 'meta.ads.%' OR action='meta.capi.purchase')
  AND actor_kind='MERCHANT' AND purpose='marketing' AND state='READY' AND generation=0);
CREATE POLICY ads_writer_event_insert ON integration.operation_events FOR INSERT TO commerce_ads_writer
 WITH CHECK (state='READY' AND generation=0 AND mode='' AND EXISTS (SELECT 1 FROM integration.operations o
  WHERE o.id=operation_events.operation_id AND o.tenant_id=operation_events.tenant_id AND o.store_id=operation_events.store_id
  AND o.provider IN ('meta_ads','meta_dataset')));
GRANT SELECT ON payments.facts TO commerce_ads_writer;
CREATE POLICY ads_writer_fact_read ON payments.facts FOR SELECT TO commerce_ads_writer USING (kind='CAPTURED');
-- Ruling B12 / R-B: the report nets succeeded refunds. Definers filter tenant/store; the policy cannot know the
-- caller's store (sweepers run without a GUC scope), so it limits the KIND only.
GRANT SELECT ON payments.refund_facts TO commerce_ads_writer;
CREATE POLICY ads_writer_refund_fact_read ON payments.refund_facts FOR SELECT TO commerce_ads_writer USING (kind='SUCCEEDED');
GRANT SELECT(tenant_id,id,active,currency) ON control.stores TO commerce_ads_writer;
CREATE POLICY ads_writer_store_read ON control.stores FOR SELECT TO commerce_ads_writer USING (active);
-- R-C (gap in §4.4): PRODUCT_TRAFFIC validation and link freezing (§5.2, D11) read the published storefront
-- and the product; column-level, active/published rows only.
GRANT SELECT(id,tenant_id,store_id,origin,state) ON control.storefront_domains TO commerce_ads_writer;
CREATE POLICY ads_writer_domain_read ON control.storefront_domains FOR SELECT TO commerce_ads_writer USING (state='ACTIVE');
GRANT SELECT(tenant_id,store_id,published) ON control.storefront_publications TO commerce_ads_writer;
CREATE POLICY ads_writer_publication_read ON control.storefront_publications FOR SELECT TO commerce_ads_writer USING (published);
GRANT SELECT(tenant_id,store_id,id,status) ON catalog.products TO commerce_ads_writer;
CREATE POLICY ads_writer_product_read ON catalog.products FOR SELECT TO commerce_ads_writer USING (status='active');

-- ---------------------------------------------------------------------------------------
-- Internal helpers (no caller EXECUTE beyond the owner's other definers).
-- ---------------------------------------------------------------------------------------
-- ads.deny: one place that maps a frozen error code to its SQLSTATE ADnnn (nnn = HTTP status, B9: 402 billing).
CREATE FUNCTION ads.deny(p_code text) RETURNS void
LANGUAGE plpgsql VOLATILE SET search_path=pg_catalog AS $$
DECLARE v_state text;
BEGIN
 v_state:=CASE
  WHEN p_code='unauthorized' THEN 'AD401'
  WHEN p_code='billing_restricted' THEN 'AD402'
  WHEN p_code='forbidden' THEN 'AD403'
  WHEN p_code='not_found' THEN 'AD404'
  WHEN p_code='state_expired' THEN 'AD410'
  WHEN p_code IN ('invalid_request','not_in_pick_list','budget_below_minimum','not_whole_unit','currency_mismatch',
   'starts_too_soon','source_not_owned','product_not_published') THEN 'AD422'
  ELSE 'AD409' END;
 RAISE EXCEPTION '%',p_code USING ERRCODE=v_state;
END $$;
REVOKE ALL ON FUNCTION ads.deny(text) FROM PUBLIC;
ALTER FUNCTION ads.deny(text) OWNER TO commerce_ads_writer;
COMMENT ON FUNCTION ads.deny(text) IS
 'ads owner; internal helper of the ads.* definers, no caller EXECUTE. Raises MESSAGE=<frozen error code> with SQLSTATE ADnnn (nnn = HTTP status) so Go maps every refusal without a driver message.';

-- ads.auth: hash-authenticated merchant scope (§4.4 "resolve_access ... merchant definers"). Every listed
-- permission must resolve ok for this store, and the result must equal this transaction's app.* GUC scope
-- (platform.WithScope set it from the same session), so a hash of another session can never act here.
CREATE FUNCTION ads.auth(p_hash bytea,p_store uuid,p_permissions text[])
RETURNS TABLE(out_tenant uuid,out_principal uuid)
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_perm text; v record; v_tenant uuid; v_principal uuid;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR p_permissions IS NULL
  OR array_ndims(p_permissions)<>1 OR cardinality(p_permissions) NOT BETWEEN 1 AND 4
  OR current_setting('transaction_isolation')<>'read committed' THEN
  PERFORM ads.deny('invalid_request');
 END IF;
 FOREACH v_perm IN ARRAY p_permissions LOOP
  SELECT * INTO v FROM identity.resolve_access(p_hash,p_store,v_perm);
  IF v.access_status='unauthorized' THEN PERFORM ads.deny('unauthorized');
  ELSIF v.access_status='not_found' THEN PERFORM ads.deny('not_found');
  ELSIF v.access_status<>'ok' THEN PERFORM ads.deny('forbidden'); END IF;
  v_tenant:=v.tenant_id; v_principal:=v.principal_id;
 END LOOP;
 IF v_tenant IS NULL OR v_principal IS NULL
  OR v_tenant IS DISTINCT FROM nullif(current_setting('app.tenant_id',true),'')::uuid
  OR p_store IS DISTINCT FROM nullif(current_setting('app.store_id',true),'')::uuid
  OR v_principal IS DISTINCT FROM nullif(current_setting('app.principal_id',true),'')::uuid THEN
  PERFORM ads.deny('forbidden');
 END IF;
 RETURN QUERY SELECT v_tenant,v_principal;
END $$;
REVOKE ALL ON FUNCTION ads.auth(bytea,uuid,text[]) FROM PUBLIC;
ALTER FUNCTION ads.auth(bytea,uuid,text[]) OWNER TO commerce_ads_writer;
COMMENT ON FUNCTION ads.auth(bytea,uuid,text[]) IS
 'ads owner; internal helper of the merchant ads.* definers, no caller EXECUTE. identity.resolve_access for every listed permission plus equality with the transaction GUC scope; refusals ADnnn unauthorized/not_found/forbidden.';

-- ads.settings_row: the store's settings, created with defaults on first use and row-locked (the allowance lock).
CREATE FUNCTION ads.lock_settings(p_tenant uuid,p_store uuid) RETURNS ads.store_settings
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE s ads.store_settings;
BEGIN
 INSERT INTO ads.store_settings(tenant_id,store_id) VALUES(p_tenant,p_store) ON CONFLICT (tenant_id,store_id) DO NOTHING;
 SELECT * INTO s FROM ads.store_settings x WHERE x.tenant_id=p_tenant AND x.store_id=p_store FOR UPDATE;
 RETURN s;
END $$;
REVOKE ALL ON FUNCTION ads.lock_settings(uuid,uuid) FROM PUBLIC;
ALTER FUNCTION ads.lock_settings(uuid,uuid) OWNER TO commerce_ads_writer;
COMMENT ON FUNCTION ads.lock_settings(uuid,uuid) IS
 'ads owner; internal helper (approve, publish, Check, sweepers), no caller EXECUTE. Creates the default settings row (SANDBOX, NT$0 allowance: O4 ads off) on first use and returns it FOR UPDATE: the per-store lock that serializes allowance decisions (AD6).';

-- ads.canonical_draft (D10, AD5): byte-identical to internal/ads.CanonicalDraft. Identity-binding version is the
-- binding's CURRENT version (not stored on the draft) so a re-pointed identity invalidates an approval.
CREATE FUNCTION ads.canonical_draft(p_draft uuid) RETURNS text
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE d ads.campaign_drafts; v_identity bigint; v_countries text;
BEGIN
 SELECT * INTO d FROM ads.campaign_drafts x WHERE x.id=p_draft;
 IF NOT FOUND THEN RETURN NULL; END IF;
 SELECT b.semantic_version INTO v_identity FROM integration.bindings b
  WHERE b.tenant_id=d.tenant_id AND b.store_id=d.store_id AND b.id=d.identity_binding_id;
 SELECT string_agg(c,',' ORDER BY c COLLATE "C") INTO v_countries FROM unnest(d.countries) c;
 RETURN 'v1|'||d.template||'|'||d.ad_binding_id::text||'|'||d.ad_binding_version::text||'|'||d.identity_binding_id::text
  ||'|'||coalesce(v_identity::text,'0')||'|'||d.source_ref||'|'||d.currency||'|'||d.lifetime_budget_minor::text
  ||'|'||floor(extract(epoch from d.starts_at))::bigint::text||'|'||floor(extract(epoch from d.ends_at))::bigint::text
  ||'|'||v_countries||'|'||d.age_min::text||'|'||d.age_max::text;
END $$;
REVOKE ALL ON FUNCTION ads.canonical_draft(uuid) FROM PUBLIC;
ALTER FUNCTION ads.canonical_draft(uuid) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.canonical_draft(uuid) TO commerce_worker;
COMMENT ON FUNCTION ads.canonical_draft(uuid) IS
 'ads owner; callers internal/ads.Checker and the MA01 vector gate (commerce_worker). Canonical draft string v1|... (D10) whose SHA-256 the approval freezes; byte-identical to internal/ads.CanonicalDraft. Reads the identity binding CURRENT version; NULL for an unknown draft.';

CREATE FUNCTION ads.draft_sha(p_draft uuid) RETURNS bytea
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT sha256(convert_to(ads.canonical_draft(p_draft),'UTF8'))
$$;
REVOKE ALL ON FUNCTION ads.draft_sha(uuid) FROM PUBLIC;
ALTER FUNCTION ads.draft_sha(uuid) OWNER TO commerce_ads_writer;
COMMENT ON FUNCTION ads.draft_sha(uuid) IS
 'ads owner; internal helper (approve, Check), no caller EXECUTE. SHA-256 of ads.canonical_draft.';

-- AD6 left-time: when an op left READY/DISPATCHING (its first event in any other state); infinity while it has not.
CREATE FUNCTION ads.op_left(p_operation uuid) RETURNS timestamptz
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT coalesce((SELECT min(e.created_at) FROM integration.operation_events e
  WHERE e.operation_id=p_operation AND e.state NOT IN ('READY','DISPATCHING')),'infinity'::timestamptz)
$$;
REVOKE ALL ON FUNCTION ads.op_left(uuid) FROM PUBLIC;
ALTER FUNCTION ads.op_left(uuid) OWNER TO commerce_ads_writer;
COMMENT ON FUNCTION ads.op_left(uuid) IS
 'ads owner; internal helper of ads.draft_counts, no caller EXECUTE. First event time of an operation in a state other than READY/DISPATCHING, infinity while it has none (AD6).';

-- AD6: does this draft still count toward the store allowance? (contract AD6 / §5.3 test, one function.)
CREATE FUNCTION ads.draft_counts(p_draft uuid) RETURNS boolean
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE d ads.campaign_drafts; v_max_left timestamptz; v_pause uuid;
BEGIN
 SELECT * INTO d FROM ads.campaign_drafts x WHERE x.id=p_draft;
 IF NOT FOUND THEN RETURN false; END IF;
 IF clock_timestamp()>d.ends_at+interval '1 day' THEN RETURN false; END IF;
 -- every activate op absent or ended BLOCKED_POLICY (the dispatcher sets that state only before any provider call);
 -- a READY, DISPATCHING, UNKNOWN or SUCCEEDED activate always counts.
 IF NOT EXISTS(SELECT 1 FROM ads.remote_objects r JOIN integration.operations o ON o.id=r.operation_id
   WHERE r.draft_id=p_draft AND r.kind='activate' AND o.state<>'BLOCKED_POLICY') THEN
  RETURN false;
 END IF;
 -- a pause SUCCEEDED whose claim (first DISPATCHING event) came after every activate op had left READY/DISPATCHING.
 SELECT max(ads.op_left(r.operation_id)) INTO v_max_left FROM ads.remote_objects r
  WHERE r.draft_id=p_draft AND r.kind='activate';
 FOR v_pause IN SELECT r.operation_id FROM ads.remote_objects r JOIN integration.operations o ON o.id=r.operation_id
   WHERE r.draft_id=p_draft AND r.kind='pause' AND o.state='SUCCEEDED' LOOP
  IF (SELECT min(e.created_at) FROM integration.operation_events e WHERE e.operation_id=v_pause AND e.state='DISPATCHING')>v_max_left
   THEN RETURN false; END IF;
 END LOOP;
 RETURN true;
END $$;
REVOKE ALL ON FUNCTION ads.draft_counts(uuid) FROM PUBLIC;
ALTER FUNCTION ads.draft_counts(uuid) OWNER TO commerce_ads_writer;
COMMENT ON FUNCTION ads.draft_counts(uuid) IS
 'ads owner; internal helper (approve, publish, Check, advance sweeper), no caller EXECUTE. AD6/§5.3 single test: false only when every activate op is absent or BLOCKED_POLICY, or a pause SUCCEEDED was claimed after every activate left READY/DISPATCHING, or now > ends_at + 1 day. READY/DISPATCHING/UNKNOWN/SUCCEEDED activates always count.';

-- Round-2 review P1 (contract 5.3 "pause is always allowed"): a disabled binding makes every MERCHANT op on it
-- STALE_BINDING at claim (integration.claim_operation), so a pause could never reach Meta and a spending campaign
-- would run to end_time with no in-product stop. Fix at the root, without amending the dispatcher: a meta_ads
-- binding cannot be disabled while any draft on it counts (AD6 test = may still spend). The merchant pauses
-- first (its op SUCCEEDED makes the draft non-counting), then disconnects. Narrows 5.3 to "enabled binding or no
-- spending campaign" (integrator ruling R2-ADS-PAUSE-1).
CREATE FUNCTION ads.guard_binding_disable() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 IF EXISTS(SELECT 1 FROM ads.campaign_drafts d WHERE d.ad_binding_id=OLD.id AND ads.draft_counts(d.id)) THEN
  RAISE EXCEPTION 'binding_in_use' USING ERRCODE='PT409';
 END IF;
 RETURN NEW;
END $$;
REVOKE ALL ON FUNCTION ads.guard_binding_disable() FROM PUBLIC;
ALTER FUNCTION ads.guard_binding_disable() OWNER TO commerce_ads_writer;
COMMENT ON FUNCTION ads.guard_binding_disable() IS
 'ads owner; trigger function only (no caller EXECUTE). Refuses (PT409 binding_in_use) disabling a meta_ads binding while a draft on it counts by the AD6 test, so pause stays dispatchable (contract 5.3).';
CREATE TRIGGER bindings_ads_disable_guard BEFORE UPDATE ON integration.bindings
 FOR EACH ROW WHEN (OLD.provider='meta_ads' AND OLD.enabled AND NOT NEW.enabled)
 EXECUTE FUNCTION ads.guard_binding_disable();

-- Sum of the allowance held by OTHER drafts of the store that are approved for their current revision and count.
CREATE FUNCTION ads.allowance_used(p_tenant uuid,p_store uuid,p_except uuid) RETURNS bigint
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT coalesce(sum(d.lifetime_budget_minor),0)::bigint FROM ads.campaign_drafts d
  WHERE d.tenant_id=p_tenant AND d.store_id=p_store AND d.id<>p_except
   AND EXISTS(SELECT 1 FROM ads.draft_approvals a WHERE a.tenant_id=d.tenant_id AND a.store_id=d.store_id
    AND a.draft_id=d.id AND a.revision=d.revision)
   AND ads.draft_counts(d.id)
$$;
REVOKE ALL ON FUNCTION ads.allowance_used(uuid,uuid,uuid) FROM PUBLIC;
ALTER FUNCTION ads.allowance_used(uuid,uuid,uuid) OWNER TO commerce_ads_writer;
COMMENT ON FUNCTION ads.allowance_used(uuid,uuid,uuid) IS
 'ads owner; internal helper (approve, Check, advance sweeper), no caller EXECUTE. I05: minor units of one currency (drafts share the store allowance_currency by ads.normalize_input). Sums other drafts approved for their current revision that ads.draft_counts still counts; callers hold the ads.lock_settings row lock.';

-- BD5/§5.2: RESTRICTED blocks growth actions only. Late-bound name: raises (fails closed) until 0080 grants
-- billing.store_standing to commerce_ads_writer (ads-capi); never fails open.
CREATE FUNCTION ads.restricted(p_tenant uuid,p_store uuid) RETURNS boolean
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 -- billing.store_standing: BD5 (customers-billing-v1) standing GOOD|GRACE|UNBILLED|RESTRICTED; pilot stores are UNBILLED.
 RETURN billing.store_standing(p_tenant,p_store)='RESTRICTED';
END $$;
REVOKE ALL ON FUNCTION ads.restricted(uuid,uuid) FROM PUBLIC;
ALTER FUNCTION ads.restricted(uuid,uuid) OWNER TO commerce_ads_writer;
COMMENT ON FUNCTION ads.restricted(uuid,uuid) IS
 'ads owner; internal helper (approve, publish, Check), no caller EXECUTE. True iff billing.store_standing = RESTRICTED. The billing name resolves at call time, so a missing function or grant raises: growth actions fail closed until 0080.';

-- ---------------------------------------------------------------------------------------
-- Connect flow (contract §2 steps 1-3). The callback's network exchange happens BETWEEN consume_state and
-- put_connect_result, never inside a transaction (state is single-use: a failed exchange burns it).
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION ads.begin_connect(p_hash bytea,p_store uuid,p_state_hash bytea)
RETURNS TABLE(out_state uuid,out_expires timestamptz)
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE a record; v_id uuid:=gen_random_uuid(); v_exp timestamptz;
BEGIN
 IF p_state_hash IS NULL OR octet_length(p_state_hash)<>32 THEN PERFORM ads.deny('invalid_request'); END IF;
 SELECT * INTO a FROM ads.auth(p_hash,p_store,ARRAY['ads:manage','integration:manage']);
 -- at most 10 live unconsumed states per store keeps the table bounded without a separate sweeper rule.
 IF (SELECT count(*) FROM ads.oauth_states s WHERE s.tenant_id=a.out_tenant AND s.store_id=p_store
   AND s.used_at IS NULL AND s.expires_at>clock_timestamp())>=10 THEN
  PERFORM ads.deny('conflict');
 END IF;
 v_exp:=clock_timestamp()+interval '10 minutes';
 INSERT INTO ads.oauth_states(id,tenant_id,store_id,principal_id,state_hash,expires_at)
  VALUES(v_id,a.out_tenant,p_store,a.out_principal,p_state_hash,v_exp);
 RETURN QUERY SELECT v_id,v_exp;
END $$;
REVOKE ALL ON FUNCTION ads.begin_connect(bytea,uuid,bytea) FROM PUBLIC;
ALTER FUNCTION ads.begin_connect(bytea,uuid,bytea) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.begin_connect(bytea,uuid,bytea) TO commerce_runtime;
COMMENT ON FUNCTION ads.begin_connect(bytea,uuid,bytea) IS
 'ads owner; only caller internal/ads.Service.Connect (commerce_runtime, ads:manage + integration:manage). Stores the SHA-256 of the OAuth state for this principal and store (10 min, single use); at most 10 live states per store.';

CREATE FUNCTION ads.consume_state(p_hash bytea,p_store uuid,p_state_hash bytea) RETURNS uuid
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE a record; s ads.oauth_states;
BEGIN
 IF p_state_hash IS NULL OR octet_length(p_state_hash)<>32 THEN PERFORM ads.deny('state_mismatch'); END IF;
 SELECT * INTO a FROM ads.auth(p_hash,p_store,ARRAY['ads:manage','integration:manage']);
 SELECT * INTO s FROM ads.oauth_states x WHERE x.state_hash=p_state_hash FOR UPDATE;
 -- One uniform refusal for unknown, foreign or used state: no oracle for another principal's state.
 IF NOT FOUND OR s.tenant_id<>a.out_tenant OR s.store_id<>p_store OR s.principal_id<>a.out_principal OR s.used_at IS NOT NULL THEN
  PERFORM ads.deny('state_mismatch');
 END IF;
 IF s.expires_at<=clock_timestamp() THEN PERFORM ads.deny('state_expired'); END IF;
 UPDATE ads.oauth_states x SET used_at=clock_timestamp() WHERE x.id=s.id;
 RETURN s.id;
END $$;
REVOKE ALL ON FUNCTION ads.consume_state(bytea,uuid,bytea) FROM PUBLIC;
ALTER FUNCTION ads.consume_state(bytea,uuid,bytea) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.consume_state(bytea,uuid,bytea) TO commerce_runtime;
COMMENT ON FUNCTION ads.consume_state(bytea,uuid,bytea) IS
 'ads owner; only caller internal/ads.Service.Callback (commerce_runtime). Marks the state used iff it belongs to this principal and store and has not expired or been used (state_mismatch / state_expired otherwise); returns its id.';

CREATE FUNCTION ads.put_connect_result(p_hash bytea,p_store uuid,p_state uuid,p_client_business text,p_picks jsonb,
 p_scopes text[],p_key_id text,p_enc bytea,p_ciphertext bytea) RETURNS void
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE a record; s ads.oauth_states; e jsonb;
BEGIN
 IF p_state IS NULL OR p_client_business IS NULL OR p_client_business !~ '^[0-9]{1,40}$' OR p_picks IS NULL
  OR jsonb_typeof(p_picks)<>'array' OR jsonb_array_length(p_picks)>200 OR octet_length(p_picks::text)>16384
  OR p_scopes IS NULL OR array_ndims(p_scopes)<>1 OR cardinality(p_scopes) NOT BETWEEN 1 AND 16
  OR array_to_string(p_scopes,',') !~ '^[a-z_]{1,64}(,[a-z_]{1,64}){0,15}$'
  OR p_key_id IS NULL OR p_key_id !~ '^[A-Za-z0-9_-]{1,64}$' OR p_enc IS NULL OR octet_length(p_enc)<>32
  OR p_ciphertext IS NULL OR octet_length(p_ciphertext) NOT BETWEEN 17 AND 8192 THEN
  PERFORM ads.deny('invalid_request');
 END IF;
 FOR e IN SELECT * FROM jsonb_array_elements(p_picks) LOOP
  IF jsonb_typeof(e)<>'object' OR e->>'kind' NOT IN ('ad_account','dataset') OR e->>'id' IS NULL OR e->>'id' !~ '^[0-9]{1,40}$' THEN
   PERFORM ads.deny('invalid_request');
  END IF;
 END LOOP;
 SELECT * INTO a FROM ads.auth(p_hash,p_store,ARRAY['ads:manage','integration:manage']);
 SELECT * INTO s FROM ads.oauth_states x WHERE x.id=p_state AND x.tenant_id=a.out_tenant AND x.store_id=p_store
  AND x.principal_id=a.out_principal FOR UPDATE;
 IF NOT FOUND OR s.used_at IS NULL OR s.client_business_id IS NOT NULL THEN PERFORM ads.deny('state_mismatch'); END IF;
 IF s.expires_at<=clock_timestamp() THEN PERFORM ads.deny('state_expired'); END IF;
 UPDATE ads.oauth_states x SET client_business_id=p_client_business,pick_list=p_picks,scopes_attested=p_scopes,
  pending_key_id=p_key_id,pending_enc=p_enc,pending_ciphertext=p_ciphertext WHERE x.id=s.id;
END $$;
REVOKE ALL ON FUNCTION ads.put_connect_result(bytea,uuid,uuid,text,jsonb,text[],text,bytea,bytea) FROM PUBLIC;
ALTER FUNCTION ads.put_connect_result(bytea,uuid,uuid,text,jsonb,text[],text,bytea,bytea) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.put_connect_result(bytea,uuid,uuid,text,jsonb,text[],text,bytea,bytea) TO commerce_runtime;
COMMENT ON FUNCTION ads.put_connect_result(bytea,uuid,uuid,text,jsonb,text[],text,bytea,bytea) IS
 'ads owner; only caller internal/ads.Service.Callback (commerce_runtime) after the code exchange. Stores client_business_id, the pick list (ids and names only), attested scopes and the HPKE-sealed token on the consumed state, once. The plaintext token never reaches SQL.';

CREATE FUNCTION ads.get_state(p_hash bytea,p_store uuid,p_state uuid) RETURNS jsonb
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE a record; s ads.oauth_states;
BEGIN
 SELECT * INTO a FROM ads.auth(p_hash,p_store,ARRAY['ads:manage','integration:manage']);
 SELECT * INTO s FROM ads.oauth_states x WHERE x.id=p_state AND x.tenant_id=a.out_tenant AND x.store_id=p_store
  AND x.principal_id=a.out_principal;
 IF NOT FOUND THEN PERFORM ads.deny('state_mismatch'); END IF;
 IF s.expires_at<=clock_timestamp() THEN PERFORM ads.deny('state_expired'); END IF;
 IF s.client_business_id IS NULL OR s.pick_list IS NULL THEN PERFORM ads.deny('not_found'); END IF;
 RETURN jsonb_build_object('state_id',s.id,'expires_at',to_char(s.expires_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
  'client_business_id',s.client_business_id,'picks',s.pick_list);
END $$;
REVOKE ALL ON FUNCTION ads.get_state(bytea,uuid,uuid) FROM PUBLIC;
ALTER FUNCTION ads.get_state(bytea,uuid,uuid) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.get_state(bytea,uuid,uuid) TO commerce_runtime;
COMMENT ON FUNCTION ads.get_state(bytea,uuid,uuid) IS
 'ads owner; only caller the GET ads/meta/states route (commerce_runtime, D5). Returns the pick list of a state of this principal and store while now < expires_at; never the sealed token.';

CREATE FUNCTION ads.finish_bind(p_hash bytea,p_store uuid,p_state uuid) RETURNS void
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE a record;
BEGIN
 SELECT * INTO a FROM ads.auth(p_hash,p_store,ARRAY['ads:manage','integration:manage']);
 UPDATE ads.oauth_states x SET pending_key_id=NULL,pending_enc=NULL,pending_ciphertext=NULL
  WHERE x.id=p_state AND x.tenant_id=a.out_tenant AND x.store_id=p_store AND x.principal_id=a.out_principal;
 IF NOT FOUND THEN PERFORM ads.deny('state_mismatch'); END IF;
END $$;
REVOKE ALL ON FUNCTION ads.finish_bind(bytea,uuid,uuid) FROM PUBLIC;
ALTER FUNCTION ads.finish_bind(bytea,uuid,uuid) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.finish_bind(bytea,uuid,uuid) TO commerce_runtime;
COMMENT ON FUNCTION ads.finish_bind(bytea,uuid,uuid) IS
 'ads owner; only caller internal/ads.Service.Bind (commerce_runtime) after every binding of the state has its token copy. Deletes the pending sealed token (contract 2 step 3: deleted on bind or at expires_at).';

CREATE FUNCTION ads.connection_version(p_hash bytea,p_store uuid,p_binding uuid) RETURNS bigint
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE a record; v_version bigint;
BEGIN
 SELECT * INTO a FROM ads.auth(p_hash,p_store,ARRAY['ads:manage']);
 SELECT c.token_version INTO v_version FROM ads.connections c WHERE c.tenant_id=a.out_tenant AND c.store_id=p_store AND c.binding_id=p_binding;
 RETURN coalesce(v_version,0);
END $$;
REVOKE ALL ON FUNCTION ads.connection_version(bytea,uuid,uuid) FROM PUBLIC;
ALTER FUNCTION ads.connection_version(bytea,uuid,uuid) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.connection_version(bytea,uuid,uuid) TO commerce_runtime;
COMMENT ON FUNCTION ads.connection_version(bytea,uuid,uuid) IS
 'ads owner; only caller internal/ads.Service.Bind (commerce_runtime, ads:manage) when a connect re-uses an existing meta_ads/meta_dataset binding. The binding''s current token version (0 = never connected), so the registrar CAS can be supplied; kept in ads.connections by integration.register_meta_ads_token; never key material.';

CREATE FUNCTION ads.purge_oauth_states() RETURNS integer
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_rows integer;
BEGIN
 UPDATE ads.oauth_states x SET pending_key_id=NULL,pending_enc=NULL,pending_ciphertext=NULL
  WHERE x.pending_ciphertext IS NOT NULL AND x.expires_at<=clock_timestamp();
 GET DIAGNOSTICS v_rows=ROW_COUNT;
 RETURN v_rows;
END $$;
REVOKE ALL ON FUNCTION ads.purge_oauth_states() FROM PUBLIC;
ALTER FUNCTION ads.purge_oauth_states() OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.purge_oauth_states() TO commerce_worker;
COMMENT ON FUNCTION ads.purge_oauth_states() IS
 'ads owner; only caller the ads_oauth_purge_v1 periodic job (commerce_worker). Deletes pending sealed token material of every state at or after expires_at; the state row and its pick list remain for the connection FK.';

-- ---------------------------------------------------------------------------------------
-- Token custody (§4.1): registrar and loader. Owner commerce_integration_writer (owns the credential tables).
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION integration.register_meta_ads_token(p_hash bytea,p_store uuid,p_state uuid,p_binding uuid,p_expected_version bigint)
RETURNS bigint LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v record; v_tenant uuid; v_principal uuid; s ads.oauth_states; b record; v_head bigint; v_next bigint; v_kind text;
 v_prev text;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR p_state IS NULL OR p_binding IS NULL
  OR p_expected_version IS NULL OR p_expected_version<0 OR p_expected_version>=9223372036854775807
  OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid_request' USING ERRCODE='AD422';
 END IF;
 -- Authenticate the merchant session by hash (0040/0037 pattern): integration:manage AND ads:manage.
 SELECT * INTO v FROM identity.resolve_access(p_hash,p_store,'integration:manage');
 IF v.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='AD401'; END IF;
 IF v.access_status='not_found' THEN RAISE EXCEPTION 'not_found' USING ERRCODE='AD404'; END IF;
 IF v.access_status<>'ok' THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='AD403'; END IF;
 v_tenant:=v.tenant_id; v_principal:=v.principal_id;
 SELECT * INTO v FROM identity.resolve_access(p_hash,p_store,'ads:manage');
 IF v.access_status<>'ok' OR v.tenant_id IS DISTINCT FROM v_tenant OR v.principal_id IS DISTINCT FROM v_principal THEN
  RAISE EXCEPTION 'forbidden' USING ERRCODE='AD403'; END IF;
 IF v_tenant IS DISTINCT FROM nullif(current_setting('app.tenant_id',true),'')::uuid
  OR p_store IS DISTINCT FROM nullif(current_setting('app.store_id',true),'')::uuid
  OR v_principal IS DISTINCT FROM nullif(current_setting('app.principal_id',true),'')::uuid THEN
  RAISE EXCEPTION 'forbidden' USING ERRCODE='AD403'; END IF;
 -- Pin the audit/policy scope (as register_meta_page_token); the principal was authenticated above.
 PERFORM set_config('app.tenant_id',v_tenant::text,true);
 PERFORM set_config('app.store_id',p_store::text,true);
 PERFORM set_config('app.principal_id',v_principal::text,true);
 -- The state must be this principal's, consumed, unexpired and still hold the sealed token.
 SELECT * INTO s FROM ads.oauth_states x WHERE x.id=p_state AND x.tenant_id=v_tenant AND x.store_id=p_store
  AND x.principal_id=v_principal FOR SHARE;
 IF NOT FOUND OR s.used_at IS NULL OR s.pending_ciphertext IS NULL OR s.client_business_id IS NULL THEN
  RAISE EXCEPTION 'state_mismatch' USING ERRCODE='AD409'; END IF;
 IF s.expires_at<=clock_timestamp() THEN RAISE EXCEPTION 'state_expired' USING ERRCODE='AD410'; END IF;
 SELECT x.id,x.provider,x.external_asset_id,x.enabled INTO b FROM integration.bindings x
  WHERE x.tenant_id=v_tenant AND x.store_id=p_store AND x.id=p_binding FOR SHARE;
 IF NOT FOUND OR b.provider NOT IN ('meta_ads','meta_dataset') OR NOT b.enabled THEN
  RAISE EXCEPTION 'binding_disabled' USING ERRCODE='AD409'; END IF;
 v_kind:=CASE b.provider WHEN 'meta_ads' THEN 'ad_account' ELSE 'dataset' END;
 IF NOT EXISTS(SELECT 1 FROM jsonb_array_elements(s.pick_list) e WHERE e->>'kind'=v_kind AND e->>'id'=b.external_asset_id) THEN
  RAISE EXCEPTION 'not_in_pick_list' USING ERRCODE='AD422'; END IF;
 IF NOT ('ads_management'=ANY(s.scopes_attested)) OR (b.provider='meta_dataset' AND NOT ('ads_read'=ANY(s.scopes_attested))) THEN
  RAISE EXCEPTION 'forbidden' USING ERRCODE='AD403'; END IF;
 -- U7 guard: a binding keeps its client business; a token for another business needs a new binding.
 SELECT c.client_business_id INTO v_prev FROM ads.connections c
  WHERE c.tenant_id=v_tenant AND c.store_id=p_store AND c.binding_id=p_binding;
 IF FOUND AND v_prev<>s.client_business_id THEN RAISE EXCEPTION 'client_business_changed' USING ERRCODE='AD409'; END IF;
 IF p_expected_version=0 THEN
  IF EXISTS(SELECT 1 FROM integration.meta_page_heads h WHERE h.tenant_id=v_tenant AND h.store_id=p_store AND h.binding_id=p_binding) THEN
   RAISE EXCEPTION 'conflict' USING ERRCODE='AD409'; END IF;
  INSERT INTO integration.meta_page_heads(tenant_id,store_id,binding_id,current_version) VALUES(v_tenant,p_store,p_binding,1);
  v_next:=1;
 ELSE
  SELECT h.current_version INTO v_head FROM integration.meta_page_heads h
   WHERE h.tenant_id=v_tenant AND h.store_id=p_store AND h.binding_id=p_binding FOR UPDATE;
  IF NOT FOUND OR v_head<>p_expected_version THEN RAISE EXCEPTION 'conflict' USING ERRCODE='AD409'; END IF;
  v_next:=p_expected_version+1;
  UPDATE integration.meta_page_heads h SET current_version=v_next,updated_at=clock_timestamp()
   WHERE h.tenant_id=v_tenant AND h.store_id=p_store AND h.binding_id=p_binding;
 END IF;
 -- The sealed pending copy moves server-side: cmd/api never re-handles the ciphertext (contract 2 step 3).
 INSERT INTO integration.meta_page_credentials(tenant_id,store_id,binding_id,provider,asset_id,version,key_id,nonce,
  ciphertext,scopes_attested,principal_id)
 VALUES(v_tenant,p_store,p_binding,b.provider,b.external_asset_id,v_next,s.pending_key_id,s.pending_enc,
  s.pending_ciphertext,s.scopes_attested,v_principal);
 INSERT INTO ads.connections(tenant_id,store_id,binding_id,client_business_id,token_version,oauth_state_id,connected_by)
  VALUES(v_tenant,p_store,p_binding,s.client_business_id,v_next,s.id,v_principal)
  ON CONFLICT (tenant_id,store_id,binding_id) DO UPDATE
   SET token_version=EXCLUDED.token_version,oauth_state_id=EXCLUDED.oauth_state_id,connected_by=EXCLUDED.connected_by,
    connected_at=clock_timestamp();
 RETURN v_next;
END $$;
ALTER FUNCTION integration.register_meta_ads_token(bytea,uuid,uuid,uuid,bigint) OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION integration.register_meta_ads_token(bytea,uuid,uuid,uuid,bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION integration.register_meta_ads_token(bytea,uuid,uuid,uuid,bigint) TO commerce_runtime;
COMMENT ON FUNCTION integration.register_meta_ads_token(bytea,uuid,uuid,uuid,bigint) IS
 'integration owner; only caller internal/ads.Service.Bind (commerce_runtime). Authenticates the merchant session hash (integration:manage + ads:manage, equal to the transaction scope), requires the consumed unexpired state of the same principal and store, the binding''s provider/asset in the state pick list, the attested scopes, and an unchanged client business; CAS-appends the HPKE-sealed token copy (nonce = 32-byte encapsulated key) and upserts ads.connections. No caller-supplied principal; the audit row is written by Go in the same transaction.';

CREATE FUNCTION integration.load_meta_ads_token(p_operation uuid,p_generation bigint,p_lease_token bytea)
RETURNS TABLE(tenant_id uuid,store_id uuid,binding_id uuid,provider text,asset_id text,version bigint,key_id text,
 nonce bytea,ciphertext bytea,scopes_attested text[])
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_binding uuid; b record; o record; v_head bigint;
BEGIN
 IF p_operation IS NULL OR p_generation IS NULL OR p_generation<1 OR p_lease_token IS NULL OR octet_length(p_lease_token)<>32 THEN
  RAISE EXCEPTION 'invalid Meta credential load' USING ERRCODE='22023';
 END IF;
 -- Only ads/CAPI operations of a MERCHANT actor on their own provider: a Page-token operation can never read here.
 SELECT x.binding_id INTO v_binding FROM integration.operations x
  WHERE x.id=p_operation AND x.actor_kind='MERCHANT'
   AND ((x.action LIKE 'meta.ads.%' AND x.provider='meta_ads') OR (x.action='meta.capi.purchase' AND x.provider='meta_dataset'));
 IF NOT FOUND THEN RAISE EXCEPTION 'Meta credential unavailable' USING ERRCODE='P0002'; END IF;
 -- Binding before operation (dispatcher lock order).
 SELECT z.id,z.provider,z.external_asset_id INTO b FROM integration.bindings z WHERE z.id=v_binding FOR SHARE;
 SELECT x.tenant_id,x.store_id,x.binding_id,x.provider,x.external_asset_id,x.state,x.lease_mode,x.generation,x.lease_until,
  x.lease_token_hash INTO o FROM integration.operations x WHERE x.id=p_operation FOR SHARE;
 -- A-10: exactly (DISPATCHING, dispatch) or (UNKNOWN, reconcile), same generation, unexpired lease, matching token.
 IF NOT FOUND OR NOT ((o.state='DISPATCHING' AND o.lease_mode='dispatch') OR (o.state='UNKNOWN' AND o.lease_mode='reconcile'))
  OR o.generation<>p_generation OR o.lease_until IS NULL OR o.lease_until<=clock_timestamp()
  OR o.lease_token_hash IS DISTINCT FROM sha256(p_lease_token) THEN
  RAISE EXCEPTION 'Meta credential lease conflict' USING ERRCODE='40001';
 END IF;
 IF b.id IS NULL OR b.id<>o.binding_id OR b.provider<>o.provider OR b.external_asset_id<>o.external_asset_id THEN
  RAISE EXCEPTION 'Meta credential binding mismatch' USING ERRCODE='PT409';
 END IF;
 SELECT h.current_version INTO v_head FROM integration.meta_page_heads h
  WHERE h.tenant_id=o.tenant_id AND h.store_id=o.store_id AND h.binding_id=o.binding_id;
 IF NOT FOUND THEN RETURN; END IF;   -- no credential: zero rows -> credential_unavailable at the dispatcher
 IF o.lease_until<=clock_timestamp() THEN RAISE EXCEPTION 'Meta credential lease conflict' USING ERRCODE='40001'; END IF;
 RETURN QUERY SELECT c.tenant_id,c.store_id,c.binding_id,c.provider,c.asset_id,c.version,c.key_id,c.nonce,c.ciphertext,c.scopes_attested
  FROM integration.meta_page_credentials c WHERE c.tenant_id=o.tenant_id AND c.store_id=o.store_id
   AND c.binding_id=o.binding_id AND c.version=v_head;
END $$;
ALTER FUNCTION integration.load_meta_ads_token(uuid,bigint,bytea) OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION integration.load_meta_ads_token(uuid,bigint,bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION integration.load_meta_ads_token(uuid,bigint,bytea) TO commerce_worker;
COMMENT ON FUNCTION integration.load_meta_ads_token(uuid,bigint,bytea) IS
 'integration owner; only caller the dispatcher LoadSecret hook of internal/integrations/meta_ads (commerce_worker) and never Check. Lease-fenced clone of load_meta_page_token for meta.ads.* and meta.capi.purchase operations: (DISPATCHING,dispatch) or (UNKNOWN,reconcile), same generation, unexpired lease, matching token hash; returns the current head sealed credential of the operation''s frozen binding; zero rows = no credential; never plaintext. The only reader of ads ciphertext.';

-- ---------------------------------------------------------------------------------------
-- Drafts (contract §2 steps 4-6, §5.1, §5.2). Local validation lives here so the DB is the backstop of the Go
-- validator (internal/ads.ValidateInput); every refusal is a frozen error code.
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION ads.ts(p_ts timestamptz) RETURNS text
LANGUAGE sql IMMUTABLE SET search_path=pg_catalog AS $$
 SELECT to_char(p_ts AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"')
$$;
REVOKE ALL ON FUNCTION ads.ts(timestamptz) FROM PUBLIC;
ALTER FUNCTION ads.ts(timestamptz) OWNER TO commerce_ads_writer;
COMMENT ON FUNCTION ads.ts(timestamptz) IS
 'ads owner; internal helper of ads.draft_json and friends, no caller EXECUTE. RFC 3339 UTC with microseconds.';

CREATE FUNCTION ads.normalize_input(p_tenant uuid,p_store uuid,p_input jsonb)
RETURNS TABLE(n_ad uuid,n_ad_version bigint,n_identity uuid,n_template text,n_source text,n_currency text,
 n_budget bigint,n_starts timestamptz,n_ends timestamptz,n_countries text[],n_amin int,n_amax int)
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_keys text[]:=ARRAY['ad_binding_id','identity_binding_id','template','source_ref','currency','lifetime_budget_minor',
  'starts_at','ends_at','countries','age_min','age_max'];
 k text; ab record; ib record; st ads.store_settings; c text; v_c text[]; v_allow text;
BEGIN
 IF p_input IS NULL OR jsonb_typeof(p_input)<>'object' THEN PERFORM ads.deny('invalid_request'); END IF;
 FOREACH k IN ARRAY v_keys LOOP
  IF NOT (p_input ? k) OR jsonb_typeof(p_input->k)='null' THEN PERFORM ads.deny('invalid_request'); END IF;
 END LOOP;
 IF EXISTS(SELECT 1 FROM jsonb_object_keys(p_input) x WHERE x<>ALL(v_keys)) THEN PERFORM ads.deny('invalid_request'); END IF;
 IF jsonb_typeof(p_input->'ad_binding_id')<>'string' OR jsonb_typeof(p_input->'identity_binding_id')<>'string'
  OR jsonb_typeof(p_input->'template')<>'string' OR jsonb_typeof(p_input->'source_ref')<>'string'
  OR jsonb_typeof(p_input->'currency')<>'string' OR jsonb_typeof(p_input->'lifetime_budget_minor')<>'number'
  OR jsonb_typeof(p_input->'starts_at')<>'string' OR jsonb_typeof(p_input->'ends_at')<>'string'
  OR jsonb_typeof(p_input->'countries')<>'array' OR jsonb_typeof(p_input->'age_min')<>'number'
  OR jsonb_typeof(p_input->'age_max')<>'number'
  OR (p_input->>'lifetime_budget_minor') !~ '^[0-9]{1,15}$' OR (p_input->>'age_min') !~ '^[0-9]{1,3}$'
  OR (p_input->>'age_max') !~ '^[0-9]{1,3}$' THEN
  PERFORM ads.deny('invalid_request');
 END IF;
 n_ad:=(p_input->>'ad_binding_id')::uuid; n_identity:=(p_input->>'identity_binding_id')::uuid;
 n_template:=p_input->>'template'; n_source:=p_input->>'source_ref'; n_currency:=p_input->>'currency';
 n_budget:=(p_input->>'lifetime_budget_minor')::bigint;
 n_starts:=date_trunc('second',(p_input->>'starts_at')::timestamptz); n_ends:=date_trunc('second',(p_input->>'ends_at')::timestamptz);
 n_amin:=(p_input->>'age_min')::int; n_amax:=(p_input->>'age_max')::int;
 IF n_template NOT IN ('BOOST_POST','PRODUCT_TRAFFIC') OR n_source !~ '^([0-9_]{1,80}|[0-9a-f-]{36})$'
  OR n_currency !~ '^[A-Z]{3}$' THEN PERFORM ads.deny('invalid_request'); END IF;
 IF jsonb_array_length(p_input->'countries') NOT BETWEEN 1 AND 10 THEN PERFORM ads.deny('invalid_request'); END IF;
 v_c:=ARRAY[]::text[];
 FOR c IN SELECT jsonb_array_elements_text(p_input->'countries') LOOP
  IF c !~ '^[A-Z]{2}$' OR c=ANY(v_c) THEN PERFORM ads.deny('invalid_request'); END IF;
  v_c:=v_c||c;
 END LOOP;
 SELECT array_agg(x ORDER BY x COLLATE "C") INTO n_countries FROM unnest(v_c) x;
 IF n_amin NOT BETWEEN 18 AND 65 OR n_amax NOT BETWEEN n_amin AND 65 THEN PERFORM ads.deny('invalid_request'); END IF;
 SELECT * INTO st FROM ads.store_settings s WHERE s.tenant_id=p_tenant AND s.store_id=p_store;
 v_allow:=coalesce(st.allowance_currency,'TWD');
 -- I05: one currency per store allowance; the draft must be in it (the account currency is re-checked by preflight).
 IF n_currency<>v_allow THEN PERFORM ads.deny('currency_mismatch'); END IF;
 IF n_currency='TWD' AND n_budget%100<>0 THEN PERFORM ads.deny('not_whole_unit'); END IF;
 -- ponytail: minimum is one whole currency unit; min_daily_budget x days needs an `md=` key in the preflight result
 -- grammar (contract 3), which the frozen grammar lacks. Add when MA-S1 shows Meta rejecting a small budget.
 IF n_budget<100 THEN PERFORM ads.deny('budget_below_minimum'); END IF;
 IF n_starts<clock_timestamp()+interval '10 minutes' THEN PERFORM ads.deny('starts_too_soon'); END IF;
 IF n_ends<=n_starts OR n_ends>n_starts+interval '30 days' THEN PERFORM ads.deny('invalid_request'); END IF;
 SELECT x.id,x.provider,x.external_asset_id,x.semantic_version,x.enabled INTO ab FROM integration.bindings x
  WHERE x.tenant_id=p_tenant AND x.store_id=p_store AND x.id=n_ad;
 IF NOT FOUND OR ab.provider<>'meta_ads' THEN PERFORM ads.deny('invalid_request'); END IF;
 IF NOT ab.enabled THEN PERFORM ads.deny('binding_disabled'); END IF;
 SELECT x.id,x.provider,x.external_asset_id,x.enabled INTO ib FROM integration.bindings x
  WHERE x.tenant_id=p_tenant AND x.store_id=p_store AND x.id=n_identity;
 IF NOT FOUND OR ib.provider NOT IN ('facebook','instagram') THEN PERFORM ads.deny('invalid_request'); END IF;
 IF NOT ib.enabled THEN PERFORM ads.deny('binding_disabled'); END IF;
 n_ad_version:=ab.semantic_version;
 IF n_template='BOOST_POST' THEN
  IF ib.provider='facebook' THEN
   IF n_source !~ '^[0-9]{1,40}_[0-9]{1,40}$' OR split_part(n_source,'_',1)<>ib.external_asset_id THEN
    PERFORM ads.deny('source_not_owned'); END IF;
  ELSIF n_source !~ '^[0-9]{1,40}$' THEN PERFORM ads.deny('source_not_owned'); END IF;
 ELSE
  -- PRODUCT_TRAFFIC needs a Page identity and a published product behind an ACTIVE storefront domain (D11).
  IF ib.provider<>'facebook' THEN PERFORM ads.deny('invalid_request'); END IF;
  IF n_source !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' THEN PERFORM ads.deny('product_not_published'); END IF;
  IF NOT EXISTS(SELECT 1 FROM catalog.products p WHERE p.tenant_id=p_tenant AND p.store_id=p_store AND p.id=n_source::uuid AND p.status='active')
   OR NOT EXISTS(SELECT 1 FROM control.storefront_publications p WHERE p.tenant_id=p_tenant AND p.store_id=p_store AND p.published)
   OR NOT EXISTS(SELECT 1 FROM control.storefront_domains d WHERE d.tenant_id=p_tenant AND d.store_id=p_store AND d.state='ACTIVE') THEN
   PERFORM ads.deny('product_not_published'); END IF;
 END IF;
 RETURN NEXT;
END $$;
REVOKE ALL ON FUNCTION ads.normalize_input(uuid,uuid,jsonb) FROM PUBLIC;
ALTER FUNCTION ads.normalize_input(uuid,uuid,jsonb) OWNER TO commerce_ads_writer;
COMMENT ON FUNCTION ads.normalize_input(uuid,uuid,jsonb) IS
 'ads owner; internal helper of create_draft/update_draft, no caller EXECUTE. Strict DraftInput parse (exact keys, types) and the contract 5.2 local rules that need the database: currency = store allowance currency, TWD whole NT$ (I05), budget >= 1 unit, start >= now+10min, <= 30 days, bindings enabled and of the right provider, source ownership, PRODUCT_TRAFFIC product/storefront published. Refusals are frozen error codes.';

CREATE FUNCTION ads.draft_status(p_draft uuid) RETURNS text
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE d ads.campaign_drafts; v_act timestamptz; v_pause timestamptz; v_es text; v_ad boolean;
BEGIN
 SELECT * INTO d FROM ads.campaign_drafts x WHERE x.id=p_draft;
 IF NOT FOUND THEN RETURN NULL; END IF;
 IF d.ended_at IS NOT NULL THEN RETURN 'ENDED'; END IF;
 IF NOT EXISTS(SELECT 1 FROM ads.draft_approvals a WHERE a.draft_id=d.id AND a.revision=d.revision) AND d.publish_attempt=0 THEN
  RETURN 'DRAFT'; END IF;
 IF d.publish_attempt=0 THEN RETURN 'APPROVED'; END IF;
 IF EXISTS(SELECT 1 FROM ads.remote_objects r JOIN integration.operations o ON o.id=r.operation_id
   WHERE r.draft_id=d.id AND r.publish_attempt=d.publish_attempt AND o.state='UNKNOWN') THEN RETURN 'UNKNOWN'; END IF;
 SELECT max(o.updated_at) INTO v_act FROM ads.remote_objects r JOIN integration.operations o ON o.id=r.operation_id
  WHERE r.draft_id=d.id AND r.kind='activate' AND o.state='SUCCEEDED';
 IF v_act IS NOT NULL THEN
  SELECT i.effective_status INTO v_es FROM ads.insights_daily i WHERE i.draft_id=d.id ORDER BY i.day DESC LIMIT 1;
  IF v_es IN ('DISAPPROVED','WITH_ISSUES') THEN RETURN 'REJECTED'; END IF;
  SELECT max(o.updated_at) INTO v_pause FROM ads.remote_objects r JOIN integration.operations o ON o.id=r.operation_id
   WHERE r.draft_id=d.id AND r.kind='pause' AND o.state='SUCCEEDED';
  IF v_pause IS NOT NULL AND v_pause>v_act THEN RETURN 'PAUSED'; END IF;
  RETURN 'ACTIVE';
 END IF;
 IF EXISTS(SELECT 1 FROM ads.remote_objects r JOIN integration.operations o ON o.id=r.operation_id
   WHERE r.draft_id=d.id AND r.publish_attempt=d.publish_attempt AND r.kind IN ('campaign','adset','creative','ad')
    AND o.state IN ('FAILED_FINAL','BLOCKED_POLICY','STALE_BINDING','CANCELLED')) THEN RETURN 'FAILED'; END IF;
 SELECT EXISTS(SELECT 1 FROM ads.remote_objects r JOIN integration.operations o ON o.id=r.operation_id
   WHERE r.draft_id=d.id AND r.publish_attempt=d.publish_attempt AND r.kind='ad' AND o.state='SUCCEEDED') INTO v_ad;
 IF v_ad AND NOT EXISTS(SELECT 1 FROM ads.remote_objects r JOIN integration.operations o ON o.id=r.operation_id
   WHERE r.draft_id=d.id AND r.publish_attempt=d.publish_attempt AND r.kind IN ('preflight','activate')
    AND o.state IN ('READY','DISPATCHING','ACKNOWLEDGED')) THEN RETURN 'REMOTE_PAUSED'; END IF;
 RETURN 'SUBMITTING';
END $$;
REVOKE ALL ON FUNCTION ads.draft_status(uuid) FROM PUBLIC;
ALTER FUNCTION ads.draft_status(uuid) OWNER TO commerce_ads_writer;
COMMENT ON FUNCTION ads.draft_status(uuid) IS
 'ads owner; internal helper of ads.draft_json, no caller EXECUTE. Derived, display-only status (contract 5.1): no job selects drafts by it. ENDED > UNKNOWN > REJECTED (latest insights effective_status) > ACTIVE/PAUSED > FAILED > REMOTE_PAUSED > SUBMITTING > APPROVED > DRAFT.';

CREATE FUNCTION ads.draft_campaign(p_draft uuid) RETURNS text
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT r.remote_id FROM ads.remote_objects r WHERE r.draft_id=p_draft AND r.kind='campaign' AND r.remote_id IS NOT NULL
  ORDER BY r.publish_attempt DESC LIMIT 1
$$;
REVOKE ALL ON FUNCTION ads.draft_campaign(uuid) FROM PUBLIC;
ALTER FUNCTION ads.draft_campaign(uuid) OWNER TO commerce_ads_writer;
COMMENT ON FUNCTION ads.draft_campaign(uuid) IS
 'ads owner; internal helper (insights, pause planning), no caller EXECUTE. The pinned campaign id of the latest attempt that has one.';

CREATE FUNCTION ads.draft_json(p_tenant uuid,p_store uuid,p_draft uuid) RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE d ads.campaign_drafts; v_remote jsonb:='{}'::jsonb; v_out jsonb; v_ops jsonb; v_acct text; v_cid text; r record;
BEGIN
 SELECT * INTO d FROM ads.campaign_drafts x WHERE x.tenant_id=p_tenant AND x.store_id=p_store AND x.id=p_draft;
 IF NOT FOUND THEN PERFORM ads.deny('not_found'); END IF;
 FOR r IN SELECT r2.kind,r2.remote_id FROM ads.remote_objects r2 WHERE r2.draft_id=d.id AND r2.publish_attempt=d.publish_attempt
   AND r2.kind IN ('campaign','adset','creative','ad') AND r2.remote_id IS NOT NULL LOOP
  v_remote:=v_remote||jsonb_build_object(r.kind||'_id',r.remote_id);
 END LOOP;
 v_out:=jsonb_build_object('id',d.id,'ad_binding_id',d.ad_binding_id,'identity_binding_id',d.identity_binding_id,
  'template',d.template,'source_ref',d.source_ref,'currency',d.currency,'lifetime_budget_minor',d.lifetime_budget_minor,
  'starts_at',ads.ts(d.starts_at),'ends_at',ads.ts(d.ends_at),'countries',to_jsonb(d.countries),
  'age_min',d.age_min,'age_max',d.age_max,'revision',d.revision,'publish_attempt',d.publish_attempt,
  'status',ads.draft_status(d.id),'created_at',ads.ts(d.created_at),'remote',v_remote);
 IF EXISTS(SELECT 1 FROM ads.draft_approvals a WHERE a.draft_id=d.id AND a.revision=d.revision) THEN
  v_out:=v_out||jsonb_build_object('approved_revision',d.revision); END IF;
 IF d.ended_at IS NOT NULL THEN v_out:=v_out||jsonb_build_object('ended_at',ads.ts(d.ended_at)); END IF;
 SELECT coalesce(jsonb_agg(jsonb_build_object('kind',q.kind,'seq',q.seq,'attempt',q.publish_attempt,'state',q.state,
   'updated_at',ads.ts(q.updated_at))||CASE WHEN q.result_code<>'' THEN jsonb_build_object('code',q.result_code) ELSE '{}'::jsonb END
   ORDER BY q.publish_attempt,q.ord,q.seq),'[]'::jsonb) INTO v_ops
  FROM (SELECT r3.kind,r3.seq,r3.publish_attempt,o.state,o.updated_at,o.result_code,
         array_position(ARRAY['campaign','adset','creative','ad','preflight','activate','pause'],r3.kind) AS ord
        FROM ads.remote_objects r3 JOIN integration.operations o ON o.id=r3.operation_id
        WHERE r3.draft_id=d.id ORDER BY r3.publish_attempt DESC,ord DESC,r3.seq DESC LIMIT 60) q;
 v_out:=v_out||jsonb_build_object('ops',v_ops);
 v_cid:=ads.draft_campaign(d.id);
 IF v_cid IS NOT NULL THEN
  SELECT b.external_asset_id INTO v_acct FROM integration.bindings b WHERE b.tenant_id=d.tenant_id AND b.store_id=d.store_id AND b.id=d.ad_binding_id;
  -- Ads Manager deep link (UI convention, not an API contract; Meta may change it): campaign list filtered to the id.
  v_out:=v_out||jsonb_build_object('ads_manager_url','https://adsmanager.facebook.com/adsmanager/manage/campaigns?act='||v_acct||'&selected_campaign_ids='||v_cid);
 END IF;
 RETURN v_out;
END $$;
REVOKE ALL ON FUNCTION ads.draft_json(uuid,uuid,uuid) FROM PUBLIC;
ALTER FUNCTION ads.draft_json(uuid,uuid,uuid) OWNER TO commerce_ads_writer;
COMMENT ON FUNCTION ads.draft_json(uuid,uuid,uuid) IS
 'ads owner; internal helper of the merchant draft definers, no caller EXECUTE. The frozen Draft JSON (DraftInput + id, revision, publish_attempt, derived status, approved_revision?, ended_at?, created_at, remote ids of the current attempt, ops of the last 60 remote steps, ads_manager_url?). Tenant and store are filtered explicitly.';

CREATE FUNCTION ads.create_draft(p_hash bytea,p_store uuid,p_input jsonb) RETURNS jsonb
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE a record; n record; v_id uuid:=gen_random_uuid();
BEGIN
 SELECT * INTO a FROM ads.auth(p_hash,p_store,ARRAY['ads:manage']);
 SELECT * INTO n FROM ads.normalize_input(a.out_tenant,p_store,p_input);
 INSERT INTO ads.campaign_drafts(tenant_id,store_id,id,ad_binding_id,ad_binding_version,identity_binding_id,template,source_ref,
  currency,lifetime_budget_minor,starts_at,ends_at,countries,age_min,age_max,created_by)
 VALUES(a.out_tenant,p_store,v_id,n.n_ad,n.n_ad_version,n.n_identity,n.n_template,n.n_source,n.n_currency,n.n_budget,
  n.n_starts,n.n_ends,n.n_countries,n.n_amin,n.n_amax,a.out_principal);
 RETURN ads.draft_json(a.out_tenant,p_store,v_id);
END $$;
REVOKE ALL ON FUNCTION ads.create_draft(bytea,uuid,jsonb) FROM PUBLIC;
ALTER FUNCTION ads.create_draft(bytea,uuid,jsonb) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.create_draft(bytea,uuid,jsonb) TO commerce_runtime;
COMMENT ON FUNCTION ads.create_draft(bytea,uuid,jsonb) IS
 'ads owner; only caller internal/ads.Service.CreateDraft (commerce_runtime, ads:manage). Validates DraftInput (ads.normalize_input), freezes the ad binding version, inserts revision 1 and returns the Draft.';

CREATE FUNCTION ads.update_draft(p_hash bytea,p_store uuid,p_draft uuid,p_expected_revision integer,p_input jsonb) RETURNS jsonb
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE a record; n record; d ads.campaign_drafts;
BEGIN
 SELECT * INTO a FROM ads.auth(p_hash,p_store,ARRAY['ads:manage']);
 IF p_expected_revision IS NULL OR p_expected_revision<1 THEN PERFORM ads.deny('invalid_request'); END IF;
 SELECT * INTO d FROM ads.campaign_drafts x WHERE x.tenant_id=a.out_tenant AND x.store_id=p_store AND x.id=p_draft FOR UPDATE;
 IF NOT FOUND THEN PERFORM ads.deny('not_found'); END IF;
 IF d.revision<>p_expected_revision THEN PERFORM ads.deny('revision_changed'); END IF;
 -- §4.1: edits only while no approval for the current revision exists; a published draft is never edited.
 IF d.publish_attempt<>0 OR d.ended_at IS NOT NULL
  OR EXISTS(SELECT 1 FROM ads.draft_approvals x WHERE x.draft_id=d.id AND x.revision=d.revision) THEN
  PERFORM ads.deny('draft_approved'); END IF;
 SELECT * INTO n FROM ads.normalize_input(a.out_tenant,p_store,p_input);
 UPDATE ads.campaign_drafts x SET ad_binding_id=n.n_ad,ad_binding_version=n.n_ad_version,identity_binding_id=n.n_identity,
  template=n.n_template,source_ref=n.n_source,currency=n.n_currency,lifetime_budget_minor=n.n_budget,starts_at=n.n_starts,
  ends_at=n.n_ends,countries=n.n_countries,age_min=n.n_amin,age_max=n.n_amax,revision=d.revision+1 WHERE x.id=d.id;
 RETURN ads.draft_json(a.out_tenant,p_store,d.id);
END $$;
REVOKE ALL ON FUNCTION ads.update_draft(bytea,uuid,uuid,integer,jsonb) FROM PUBLIC;
ALTER FUNCTION ads.update_draft(bytea,uuid,uuid,integer,jsonb) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.update_draft(bytea,uuid,uuid,integer,jsonb) TO commerce_runtime;
COMMENT ON FUNCTION ads.update_draft(bytea,uuid,uuid,integer,jsonb) IS
 'ads owner; only caller internal/ads.Service.UpdateDraft (commerce_runtime, ads:manage). Revision CAS (revision_changed), refuses a draft that is approved for its current revision, published or ended (draft_approved), then bumps the revision.';

CREATE FUNCTION ads.list_drafts(p_hash bytea,p_store uuid) RETURNS jsonb
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE a record; v_items jsonb:='[]'::jsonb; v_id uuid;
BEGIN
 SELECT * INTO a FROM ads.auth(p_hash,p_store,ARRAY['ads:read']);
 FOR v_id IN SELECT x.id FROM ads.campaign_drafts x WHERE x.tenant_id=a.out_tenant AND x.store_id=p_store
   ORDER BY x.created_at DESC,x.id LIMIT 100 LOOP
  v_items:=v_items||jsonb_build_array(ads.draft_json(a.out_tenant,p_store,v_id));
 END LOOP;
 RETURN jsonb_build_object('items',v_items);
END $$;
REVOKE ALL ON FUNCTION ads.list_drafts(bytea,uuid) FROM PUBLIC;
ALTER FUNCTION ads.list_drafts(bytea,uuid) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.list_drafts(bytea,uuid) TO commerce_runtime;
COMMENT ON FUNCTION ads.list_drafts(bytea,uuid) IS
 'ads owner; only caller internal/ads.Service.ListDrafts (commerce_runtime, ads:read). The newest 100 drafts of the caller''s store as Draft JSON.';

CREATE FUNCTION ads.get_draft(p_hash bytea,p_store uuid,p_draft uuid) RETURNS jsonb
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE a record;
BEGIN
 SELECT * INTO a FROM ads.auth(p_hash,p_store,ARRAY['ads:read']);
 RETURN ads.draft_json(a.out_tenant,p_store,p_draft);
END $$;
REVOKE ALL ON FUNCTION ads.get_draft(bytea,uuid,uuid) FROM PUBLIC;
ALTER FUNCTION ads.get_draft(bytea,uuid,uuid) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.get_draft(bytea,uuid,uuid) TO commerce_runtime;
COMMENT ON FUNCTION ads.get_draft(bytea,uuid,uuid) IS
 'ads owner; only caller internal/ads.Service.GetDraft (commerce_runtime, ads:read). One Draft of the caller''s store; not_found otherwise.';

-- ---------------------------------------------------------------------------------------
-- Approve / publish / pause / end (contract §2 steps 6-8, §5.3).
-- ---------------------------------------------------------------------------------------
-- The DraftInput of a stored draft (ads.normalize_input re-validates approval-time facts against it).
CREATE FUNCTION ads.draft_input(p_draft ads.campaign_drafts) RETURNS jsonb
LANGUAGE sql IMMUTABLE SET search_path=pg_catalog AS $$
 SELECT jsonb_build_object('ad_binding_id',p_draft.ad_binding_id,'identity_binding_id',p_draft.identity_binding_id,
  'template',p_draft.template,'source_ref',p_draft.source_ref,'currency',p_draft.currency,
  'lifetime_budget_minor',p_draft.lifetime_budget_minor,'starts_at',ads.ts(p_draft.starts_at),'ends_at',ads.ts(p_draft.ends_at),
  'countries',to_jsonb(p_draft.countries),'age_min',p_draft.age_min,'age_max',p_draft.age_max)
$$;
REVOKE ALL ON FUNCTION ads.draft_input(ads.campaign_drafts) FROM PUBLIC;
ALTER FUNCTION ads.draft_input(ads.campaign_drafts) OWNER TO commerce_ads_writer;
COMMENT ON FUNCTION ads.draft_input(ads.campaign_drafts) IS
 'ads owner; internal helper of ads.approve_draft, no caller EXECUTE. Rebuilds the DraftInput JSON of a stored draft so approval re-runs ads.normalize_input.';

CREATE FUNCTION ads.approve_draft(p_hash bytea,p_store uuid,p_draft uuid,p_revision integer) RETURNS jsonb
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE a record; st ads.store_settings; d ads.campaign_drafts; n record;
BEGIN
 SELECT * INTO a FROM ads.auth(p_hash,p_store,ARRAY['ads:approve']);
 IF p_revision IS NULL OR p_revision<1 THEN PERFORM ads.deny('invalid_request'); END IF;
 -- Lock order: store settings row (the allowance lock) before the draft row, as Check and the sweeper.
 st:=ads.lock_settings(a.out_tenant,p_store);
 SELECT * INTO d FROM ads.campaign_drafts x WHERE x.tenant_id=a.out_tenant AND x.store_id=p_store AND x.id=p_draft FOR UPDATE;
 IF NOT FOUND THEN PERFORM ads.deny('not_found'); END IF;
 IF d.revision<>p_revision THEN PERFORM ads.deny('revision_changed'); END IF;
 IF EXISTS(SELECT 1 FROM ads.draft_approvals x WHERE x.draft_id=d.id AND x.revision=d.revision) THEN
  RETURN ads.draft_json(a.out_tenant,p_store,d.id);      -- idempotent: already approved for this revision
 END IF;
 IF d.publish_attempt<>0 OR d.ended_at IS NOT NULL THEN PERFORM ads.deny('invalid_request'); END IF;
 -- BD5: RESTRICTED blocks approve (HTTP 402, ruling B9). Raises (fails closed) until 0080.
 IF ads.restricted(a.out_tenant,p_store) THEN PERFORM ads.deny('billing_restricted'); END IF;
 SELECT * INTO n FROM ads.normalize_input(a.out_tenant,p_store,ads.draft_input(d));
 IF n.n_ad_version<>d.ad_binding_version THEN PERFORM ads.deny('binding_disabled'); END IF;
 -- I05: minor units of one currency (normalize_input proved d.currency = allowance_currency); AD6 allowance under the
 -- store lock; the draft always counts itself.
 IF ads.allowance_used(a.out_tenant,p_store,d.id)+d.lifetime_budget_minor>st.max_active_budget_minor THEN
  PERFORM ads.deny('over_allowance'); END IF;
 INSERT INTO ads.draft_approvals(tenant_id,store_id,draft_id,revision,draft_sha256,principal_id)
  VALUES(a.out_tenant,p_store,d.id,d.revision,ads.draft_sha(d.id),a.out_principal);
 RETURN ads.draft_json(a.out_tenant,p_store,d.id);
END $$;
REVOKE ALL ON FUNCTION ads.approve_draft(bytea,uuid,uuid,integer) FROM PUBLIC;
ALTER FUNCTION ads.approve_draft(bytea,uuid,uuid,integer) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.approve_draft(bytea,uuid,uuid,integer) TO commerce_runtime;
COMMENT ON FUNCTION ads.approve_draft(bytea,uuid,uuid,integer) IS
 'ads owner; only caller internal/ads.Service.Approve (commerce_runtime, ads:approve). Under the store settings lock: revision CAS (revision_changed), billing standing (billing_restricted 402), re-validation of the frozen draft, AD6 allowance (over_allowance), then appends the approval that freezes SHA-256 of ads.canonical_draft. Idempotent per revision.';

-- The River job must be exactly the row inserted in this transaction on the ads lane (xmin needs table-level SELECT,
-- granted in post_river/0015). Same check as integration.plan_claim_reply (0064), for queue ads.
CREATE FUNCTION ads.verify_job(p_job bigint,p_op uuid,p_priority integer) RETURNS void
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE j record;
BEGIN
 SELECT r.id INTO j FROM river.river_job r WHERE r.id=p_job AND r.kind='external_operation_v1' AND r.queue='ads'
  AND r.priority=p_priority AND r.unique_key IS NULL AND r.args=jsonb_build_object('operation_id',p_op::text,'version',1)
  AND r.xmin=pg_current_xact_id()::xid;
 IF NOT FOUND THEN RAISE EXCEPTION 'invalid ads plan' USING ERRCODE='22023'; END IF;
END $$;
REVOKE ALL ON FUNCTION ads.verify_job(bigint,uuid,integer) FROM PUBLIC;
ALTER FUNCTION ads.verify_job(bigint,uuid,integer) OWNER TO commerce_ads_writer;
COMMENT ON FUNCTION ads.verify_job(bigint,uuid,integer) IS
 'ads owner; internal helper of every ads planner, no caller EXECUTE. Raises 22023 unless river.river_job row p_job is exactly this transaction''s external_operation_v1 job of p_op on queue ads at p_priority with no unique key. Needs the post_river/0015 table SELECT; before it exists every plan fails closed.';

-- Plan one remote step: verifies the caller's River job is exactly this transaction's row (plan_claim_reply
-- pattern), then inserts the READY operation, its event and the remote_objects step. Priority lane: pause = 1.
CREATE FUNCTION ads.plan_op(p_d ads.campaign_drafts,p_kind text,p_seq integer,p_attempt integer,p_op uuid,p_job bigint,
 p_principal uuid,p_request jsonb) RETURNS void
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_action text; v_prio integer; b record; v_key text;
BEGIN
 v_action:='meta.ads.'||CASE p_kind WHEN 'campaign' THEN 'create_campaign' WHEN 'adset' THEN 'create_adset'
  WHEN 'creative' THEN 'create_creative' WHEN 'ad' THEN 'create_ad' WHEN 'preflight' THEN 'preflight_account'
  WHEN 'activate' THEN 'activate' WHEN 'pause' THEN 'pause' END;
 v_prio:=CASE WHEN p_kind='pause' THEN 1 ELSE 3 END;
 IF v_action='meta.ads.' OR p_seq NOT BETWEEN 1 AND 50 OR p_attempt<1 OR p_op IS NULL OR p_job IS NULL OR p_job<=0
  OR p_principal IS NULL OR p_request IS NULL OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid ads plan' USING ERRCODE='22023';
 END IF;
 PERFORM ads.verify_job(p_job,p_op,v_prio);
 SELECT x.id,x.provider,x.external_asset_id,x.semantic_version,x.enabled INTO b FROM integration.bindings x
  WHERE x.tenant_id=p_d.tenant_id AND x.store_id=p_d.store_id AND x.id=p_d.ad_binding_id;
 -- Everything except pause needs the binding the draft was approved on; pause reduces spend and uses whatever the
 -- binding is now (a disabled binding is then STALE_BINDING at the dispatcher, never a call on another asset).
 IF NOT FOUND OR b.provider<>'meta_ads' OR (p_kind<>'pause' AND (NOT b.enabled OR b.semantic_version<>p_d.ad_binding_version)) THEN
  PERFORM ads.deny('binding_disabled');
 END IF;
 v_key:='ads:'||p_d.id::text||':'||p_attempt::text||':'||p_kind||':'||p_seq::text;
 INSERT INTO integration.operations(tenant_id,store_id,id,principal_id,binding_id,binding_version,provider,external_asset_id,
  purpose,action,semantic_key,request_hash,request,job_id)
 VALUES(p_d.tenant_id,p_d.store_id,p_op,p_principal,b.id,b.semantic_version,'meta_ads',b.external_asset_id,'marketing',
  v_action,v_key,sha256(convert_to(p_request::text,'UTF8')),p_request,p_job);
 INSERT INTO integration.operation_events(tenant_id,store_id,operation_id,generation,state,mode,reason_code)
  VALUES(p_d.tenant_id,p_d.store_id,p_op,0,'READY','','operation_planned');
 INSERT INTO ads.remote_objects(tenant_id,store_id,draft_id,publish_attempt,kind,seq,operation_id)
  VALUES(p_d.tenant_id,p_d.store_id,p_d.id,p_attempt,p_kind,p_seq,p_op);
END $$;
REVOKE ALL ON FUNCTION ads.plan_op(ads.campaign_drafts,text,integer,integer,uuid,bigint,uuid,jsonb) FROM PUBLIC;
ALTER FUNCTION ads.plan_op(ads.campaign_drafts,text,integer,integer,uuid,bigint,uuid,jsonb) OWNER TO commerce_ads_writer;
COMMENT ON FUNCTION ads.plan_op(ads.campaign_drafts,text,integer,integer,uuid,bigint,uuid,jsonb) IS
 'ads owner; internal helper of every ads planner (publish, pause, advance sweeper), no caller EXECUTE. Verifies the River job is this transaction''s external_operation_v1 row on queue ads with the right priority (pause 1, else 3), then inserts the READY marketing operation (semantic key ads:<draft>:<attempt>:<kind>:<seq>), its event and the remote_objects step. Any failed precondition is an invariant breach (22023).';

-- The frozen op request (D11). No token, no PII. Called by planners only.
CREATE FUNCTION ads.request_for(p_d ads.campaign_drafts,p_kind text,p_attempt integer,p_seq integer,p_op uuid)
RETURNS jsonb LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_base jsonb; v_campaign text; v_adset text; v_creative text; ib record; v_page text; v_origin text; v_tag text;
BEGIN
 v_base:=jsonb_build_object('v',1,'draft_id',p_d.id,'attempt',p_attempt);
 v_tag:='lc-'||p_op::text;
 IF p_kind IN ('adset','creative','ad','preflight','activate','pause') THEN
  SELECT r.remote_id INTO v_campaign FROM ads.remote_objects r
   WHERE r.draft_id=p_d.id AND r.publish_attempt=p_attempt AND r.kind='campaign' AND r.seq=1;
 END IF;
 IF p_kind='campaign' THEN
  RETURN v_base||jsonb_build_object('name',v_tag,'objective',CASE p_d.template WHEN 'BOOST_POST' THEN 'OUTCOME_ENGAGEMENT' ELSE 'OUTCOME_TRAFFIC' END,
   'currency',p_d.currency,
   -- ponytail: D12 spend_cap only for USD drafts >= USD 100 (F10 minimum, no FX here); TWD/HKD rely on the lifetime budget. MA-S1 may revise.
   'spend_cap_minor',CASE WHEN p_d.currency='USD' AND p_d.lifetime_budget_minor>=10000 THEN p_d.lifetime_budget_minor ELSE 0 END);
 ELSIF p_kind='adset' THEN
  IF v_campaign IS NULL THEN RAISE EXCEPTION 'invalid ads plan' USING ERRCODE='22023'; END IF;
  RETURN v_base||jsonb_build_object('name',v_tag,'campaign_id',v_campaign,'template',p_d.template,'currency',p_d.currency,
   'lifetime_budget_minor',p_d.lifetime_budget_minor,
   'start_time',to_char(p_d.starts_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"'),
   'end_time',to_char(p_d.ends_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"'),
   'countries',to_jsonb(p_d.countries),'age_min',p_d.age_min,'age_max',p_d.age_max);
 ELSIF p_kind='creative' THEN
  SELECT x.provider,x.external_asset_id INTO ib FROM integration.bindings x
   WHERE x.tenant_id=p_d.tenant_id AND x.store_id=p_d.store_id AND x.id=p_d.identity_binding_id;
  IF NOT FOUND THEN RAISE EXCEPTION 'invalid ads plan' USING ERRCODE='22023'; END IF;
  v_base:=v_base||jsonb_build_object('name',v_tag,'template',p_d.template);
  IF ib.provider='facebook' THEN v_page:=ib.external_asset_id;
  ELSE
   -- An Instagram identity has no Page id of its own; use the store's first enabled Page binding when it has one
   -- (the frozen key is required, ads-graph treats an absent page_id as optional next to instagram_user_id).
   SELECT x.external_asset_id INTO v_page FROM integration.bindings x WHERE x.tenant_id=p_d.tenant_id
    AND x.store_id=p_d.store_id AND x.provider='facebook' AND x.enabled ORDER BY x.created_at,x.id LIMIT 1;
  END IF;
  IF v_page IS NOT NULL THEN v_base:=v_base||jsonb_build_object('page_id',v_page); END IF;
  IF p_d.template='BOOST_POST' AND ib.provider='facebook' THEN
   RETURN v_base||jsonb_build_object('object_story_id',p_d.source_ref);
  ELSIF p_d.template='BOOST_POST' THEN
   RETURN v_base||jsonb_build_object('source_instagram_media_id',p_d.source_ref,'instagram_user_id',ib.external_asset_id);
  END IF;
  -- D11: PRODUCT_TRAFFIC freezes link_url at create_creative planning from the ACTIVE storefront domain.
  SELECT d.origin INTO v_origin FROM control.storefront_domains d WHERE d.tenant_id=p_d.tenant_id AND d.store_id=p_d.store_id
   AND d.state='ACTIVE' ORDER BY d.id LIMIT 1;
  IF v_origin IS NULL THEN PERFORM ads.deny('product_not_published'); END IF;
  RETURN v_base||jsonb_build_object('link_url',v_origin||'/products/'||p_d.source_ref);
 ELSIF p_kind='ad' THEN
  SELECT r.remote_id INTO v_adset FROM ads.remote_objects r WHERE r.draft_id=p_d.id AND r.publish_attempt=p_attempt AND r.kind='adset' AND r.seq=1;
  SELECT r.remote_id INTO v_creative FROM ads.remote_objects r WHERE r.draft_id=p_d.id AND r.publish_attempt=p_attempt AND r.kind='creative' AND r.seq=1;
  IF v_adset IS NULL OR v_creative IS NULL THEN RAISE EXCEPTION 'invalid ads plan' USING ERRCODE='22023'; END IF;
  RETURN v_base||jsonb_build_object('name',v_tag,'adset_id',v_adset,'creative_id',v_creative);
 ELSIF p_kind='preflight' THEN
  RETURN v_base||jsonb_build_object('seq',p_seq);
 ELSIF p_kind IN ('activate','pause') THEN
  IF v_campaign IS NULL THEN RAISE EXCEPTION 'invalid ads plan' USING ERRCODE='22023'; END IF;
  RETURN v_base||jsonb_build_object('seq',p_seq,'campaign_id',v_campaign);
 END IF;
 RAISE EXCEPTION 'invalid ads plan' USING ERRCODE='22023';
END $$;
REVOKE ALL ON FUNCTION ads.request_for(ads.campaign_drafts,text,integer,integer,uuid) FROM PUBLIC;
ALTER FUNCTION ads.request_for(ads.campaign_drafts,text,integer,integer,uuid) OWNER TO commerce_ads_writer;
COMMENT ON FUNCTION ads.request_for(ads.campaign_drafts,text,integer,integer,uuid) IS
 'ads owner; internal helper of ads.publish_draft/pause_plan/advance_plan, no caller EXECUTE. Builds the frozen op request JSON (D11) from the draft and pinned remote ids: internal ids only, no token, no PII; objective BOOST_POST -> OUTCOME_ENGAGEMENT, PRODUCT_TRAFFIC -> OUTCOME_TRAFFIC; spend_cap only for USD >= 10000 minor (D12); link_url frozen at creative planning.';

CREATE FUNCTION ads.approver(p_draft uuid) RETURNS uuid
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT a.principal_id FROM ads.draft_approvals a JOIN ads.campaign_drafts d ON d.id=a.draft_id AND d.tenant_id=a.tenant_id
  AND d.store_id=a.store_id AND a.revision=d.revision WHERE d.id=p_draft
$$;
REVOKE ALL ON FUNCTION ads.approver(uuid) FROM PUBLIC;
ALTER FUNCTION ads.approver(uuid) OWNER TO commerce_ads_writer;
COMMENT ON FUNCTION ads.approver(uuid) IS
 'ads owner; internal helper (planners, Check), no caller EXECUTE. Principal of the approval of the draft''s current revision, or NULL.';

CREATE FUNCTION ads.publish_draft(p_hash bytea,p_store uuid,p_draft uuid,p_attempt integer,p_op uuid,p_job bigint) RETURNS jsonb
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE a record; d ads.campaign_drafts; v_approver uuid; b record; v_attempt integer;
BEGIN
 SELECT * INTO a FROM ads.auth(p_hash,p_store,ARRAY['ads:approve']);
 IF p_attempt IS NULL OR p_attempt<0 THEN PERFORM ads.deny('invalid_request'); END IF;
 PERFORM ads.lock_settings(a.out_tenant,p_store);
 SELECT * INTO d FROM ads.campaign_drafts x WHERE x.tenant_id=a.out_tenant AND x.store_id=p_store AND x.id=p_draft FOR UPDATE;
 IF NOT FOUND THEN PERFORM ads.deny('not_found'); END IF;
 -- CAS on the body's publish_attempt: a concurrent publish moved it, so exactly one wins (attempt_changed).
 IF d.publish_attempt<>p_attempt OR d.publish_attempt>=5 THEN PERFORM ads.deny('attempt_changed'); END IF;
 v_approver:=ads.approver(d.id);
 -- X7: any pause op (like ended_at) makes this draft final; a new attempt could never activate (check_activate pause_requested),
 -- so refuse it up front. Resume = copy into a new draft.
 IF v_approver IS NULL OR d.ended_at IS NOT NULL
  OR EXISTS(SELECT 1 FROM ads.remote_objects r WHERE r.draft_id=d.id AND r.kind='pause') THEN PERFORM ads.deny('invalid_request'); END IF;
 IF ads.restricted(a.out_tenant,p_store) THEN PERFORM ads.deny('billing_restricted'); END IF;
 -- §5.3: a re-publish needs every earlier attempt non-spending by the AD6 test (a still-counting draft may be live).
 IF d.publish_attempt>=1 AND ads.draft_counts(d.id) THEN PERFORM ads.deny('prior_attempt_not_paused'); END IF;
 SELECT x.enabled,x.semantic_version INTO b FROM integration.bindings x
  WHERE x.tenant_id=d.tenant_id AND x.store_id=d.store_id AND x.id=d.ad_binding_id;
 IF NOT FOUND OR NOT b.enabled OR b.semantic_version<>d.ad_binding_version THEN PERFORM ads.deny('binding_disabled'); END IF;
 v_attempt:=d.publish_attempt+1;
 UPDATE ads.campaign_drafts x SET publish_attempt=v_attempt WHERE x.id=d.id;
 d.publish_attempt:=v_attempt;
 -- Ops carry the approver as principal (contract 2 step 7); Check re-verifies the approver still holds ads:approve.
 PERFORM ads.plan_op(d,'campaign',1,v_attempt,p_op,p_job,v_approver,ads.request_for(d,'campaign',v_attempt,1,p_op));
 RETURN ads.draft_json(a.out_tenant,p_store,d.id);
END $$;
REVOKE ALL ON FUNCTION ads.publish_draft(bytea,uuid,uuid,integer,uuid,bigint) FROM PUBLIC;
ALTER FUNCTION ads.publish_draft(bytea,uuid,uuid,integer,uuid,bigint) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.publish_draft(bytea,uuid,uuid,integer,uuid,bigint) TO commerce_runtime;
COMMENT ON FUNCTION ads.publish_draft(bytea,uuid,uuid,integer,uuid,bigint) IS
 'ads owner; only caller internal/ads.Service.Publish (commerce_runtime, ads:approve, after inserting the ads-lane River job in the same transaction). CAS on publish_attempt (attempt_changed), approval present, billing standing (billing_restricted 402), earlier attempts non-spending (prior_attempt_not_paused), binding unchanged; bumps the attempt and plans create_campaign.';

CREATE FUNCTION ads.pause_prepare(p_hash bytea,p_store uuid,p_draft uuid,p_end boolean) RETURNS boolean
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE a record; d ads.campaign_drafts; v_campaign text;
BEGIN
 SELECT * INTO a FROM ads.auth(p_hash,p_store,ARRAY[CASE WHEN p_end THEN 'ads:approve' ELSE 'ads:manage' END]);
 SELECT * INTO d FROM ads.campaign_drafts x WHERE x.tenant_id=a.out_tenant AND x.store_id=p_store AND x.id=p_draft FOR UPDATE;
 IF NOT FOUND THEN PERFORM ads.deny('not_found'); END IF;
 IF p_end AND d.ended_at IS NULL THEN UPDATE ads.campaign_drafts x SET ended_at=clock_timestamp() WHERE x.id=d.id; END IF;
 IF d.publish_attempt=0 THEN RETURN false; END IF;               -- never published: nothing remote to pause
 v_campaign:=(SELECT r.remote_id FROM ads.remote_objects r WHERE r.draft_id=d.id AND r.publish_attempt=d.publish_attempt AND r.kind='campaign');
 IF v_campaign IS NULL THEN
  -- Campaign not pinned yet (create in flight or UNKNOWN): no op can name it. Ending the draft stops any later
  -- activate (Check: ended_at) and the advance sweeper plans the pause once the reconcile pins the campaign (X7:
  -- a paused draft never re-activates, so pausing and ending are the same for spend).
  UPDATE ads.campaign_drafts x SET ended_at=coalesce(x.ended_at,clock_timestamp()) WHERE x.id=d.id;
  RETURN false;
 END IF;
 -- A pause already in flight is the request; a second one adds nothing (idempotent by state, not by key).
 IF EXISTS(SELECT 1 FROM ads.remote_objects r JOIN integration.operations o ON o.id=r.operation_id
   WHERE r.draft_id=d.id AND r.publish_attempt=d.publish_attempt AND r.kind='pause' AND o.state IN ('READY','DISPATCHING','UNKNOWN','ACKNOWLEDGED')) THEN
  RETURN false;
 END IF;
 RETURN true;
END $$;
REVOKE ALL ON FUNCTION ads.pause_prepare(bytea,uuid,uuid,boolean) FROM PUBLIC;
ALTER FUNCTION ads.pause_prepare(bytea,uuid,uuid,boolean) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.pause_prepare(bytea,uuid,uuid,boolean) TO commerce_runtime;
COMMENT ON FUNCTION ads.pause_prepare(bytea,uuid,uuid,boolean) IS
 'ads owner; only caller internal/ads.Service.Pause/End (commerce_runtime; pause needs ads:manage, end needs ads:approve). Locks the draft, sets ended_at for end (and for a pause before the campaign is pinned), and answers whether a pause op must now be planned (false when never published or one is already in flight).';

CREATE FUNCTION ads.pause_plan(p_hash bytea,p_store uuid,p_draft uuid,p_end boolean,p_op uuid,p_job bigint) RETURNS jsonb
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE a record; d ads.campaign_drafts; v_seq integer;
BEGIN
 SELECT * INTO a FROM ads.auth(p_hash,p_store,ARRAY[CASE WHEN p_end THEN 'ads:approve' ELSE 'ads:manage' END]);
 SELECT * INTO d FROM ads.campaign_drafts x WHERE x.tenant_id=a.out_tenant AND x.store_id=p_store AND x.id=p_draft FOR UPDATE;
 IF NOT FOUND OR d.publish_attempt=0 THEN PERFORM ads.deny('not_found'); END IF;
 SELECT coalesce(max(r.seq),0)+1 INTO v_seq FROM ads.remote_objects r
  WHERE r.draft_id=d.id AND r.publish_attempt=d.publish_attempt AND r.kind='pause';
 IF v_seq>50 THEN PERFORM ads.deny('conflict'); END IF;
 -- Pause needs no approver re-check (contract 5.3); the op is the requesting principal's, priority lane 1.
 PERFORM ads.plan_op(d,'pause',v_seq,d.publish_attempt,p_op,p_job,a.out_principal,
  ads.request_for(d,'pause',d.publish_attempt,v_seq,p_op));
 RETURN ads.draft_json(a.out_tenant,p_store,d.id);
END $$;
REVOKE ALL ON FUNCTION ads.pause_plan(bytea,uuid,uuid,boolean,uuid,bigint) FROM PUBLIC;
ALTER FUNCTION ads.pause_plan(bytea,uuid,uuid,boolean,uuid,bigint) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.pause_plan(bytea,uuid,uuid,boolean,uuid,bigint) TO commerce_runtime;
COMMENT ON FUNCTION ads.pause_plan(bytea,uuid,uuid,boolean,uuid,bigint) IS
 'ads owner; only caller internal/ads.Service.Pause/End (commerce_runtime) after pause_prepare answered true and the priority-1 ads-lane River job was inserted in the same transaction. Plans pause seq+1 of the current attempt for the requesting principal.';

CREATE FUNCTION ads.pause_view(p_hash bytea,p_store uuid,p_draft uuid,p_end boolean) RETURNS jsonb
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE a record;
BEGIN
 SELECT * INTO a FROM ads.auth(p_hash,p_store,ARRAY[CASE WHEN p_end THEN 'ads:approve' ELSE 'ads:manage' END]);
 RETURN ads.draft_json(a.out_tenant,p_store,p_draft);
END $$;
REVOKE ALL ON FUNCTION ads.pause_view(bytea,uuid,uuid,boolean) FROM PUBLIC;
ALTER FUNCTION ads.pause_view(bytea,uuid,uuid,boolean) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.pause_view(bytea,uuid,uuid,boolean) TO commerce_runtime;
COMMENT ON FUNCTION ads.pause_view(bytea,uuid,uuid,boolean) IS
 'ads owner; only caller internal/ads.Service.Pause/End (commerce_runtime) when pause_prepare answered false: the Draft JSON for a caller who holds the pause/end permission but maybe not ads:read.';

-- ---------------------------------------------------------------------------------------
-- Dispatcher Check (contract §6.1): PG-only, no network, no secret. Each returns '' (allowed) or the frozen
-- BLOCKED_POLICY code; the Go Checker maps a code to core.DenyPolicy and returns nil in reconcile mode (X2).
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION ads.check_create(p_operation uuid) RETURNS text
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE o record; d ads.campaign_drafts; v_approval record; st ads.store_settings;
BEGIN
 SELECT x.tenant_id,x.store_id,x.action,x.request,x.external_asset_id INTO o FROM integration.operations x
  WHERE x.id=p_operation AND x.provider='meta_ads' AND x.actor_kind='MERCHANT';
 IF NOT FOUND OR o.action NOT IN ('meta.ads.create_campaign','meta.ads.create_adset','meta.ads.create_creative','meta.ads.create_ad') THEN
  RETURN 'unknown_action'; END IF;
 IF coalesce(o.request->>'draft_id','') !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  OR coalesce(o.request->>'attempt','') !~ '^[0-9]{1,2}$' THEN RETURN 'invalid_request'; END IF;
 SELECT * INTO d FROM ads.campaign_drafts x WHERE x.tenant_id=o.tenant_id AND x.store_id=o.store_id AND x.id=(o.request->>'draft_id')::uuid;
 IF NOT FOUND THEN RETURN 'draft_missing'; END IF;
 IF (o.request->>'attempt')::integer<>d.publish_attempt THEN RETURN 'attempt_stale'; END IF;
 SELECT a.draft_sha256,a.principal_id INTO v_approval FROM ads.draft_approvals a WHERE a.draft_id=d.id AND a.revision=d.revision;
 IF NOT FOUND THEN RETURN 'not_approved'; END IF;
 -- AD5: the approval freezes SHA-256 of the canonical draft; any drift (edit, identity binding version) fails here.
 IF v_approval.draft_sha256 IS DISTINCT FROM ads.draft_sha(d.id) THEN RETURN 'approval_stale'; END IF;
 IF NOT identity.principal_holds(d.tenant_id,d.store_id,v_approval.principal_id,ARRAY['ads:approve']) THEN RETURN 'approver_revoked'; END IF;
 SELECT * INTO st FROM ads.store_settings s WHERE s.tenant_id=d.tenant_id AND s.store_id=d.store_id;
 -- AD9: SANDBOX refuses every mutating action unless the bound ad account is the operator's sandbox account (zero HTTP).
 IF coalesce(st.environment,'SANDBOX')='SANDBOX' AND (st.sandbox_ad_account IS NULL OR o.external_asset_id<>st.sandbox_ad_account) THEN
  RETURN 'sandbox_account_required'; END IF;
 IF ads.restricted(d.tenant_id,d.store_id) THEN RETURN 'billing_restricted'; END IF;
 RETURN '';
END $$;
REVOKE ALL ON FUNCTION ads.check_create(uuid) FROM PUBLIC;
ALTER FUNCTION ads.check_create(uuid) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.check_create(uuid) TO commerce_worker;
COMMENT ON FUNCTION ads.check_create(uuid) IS
 'ads owner; only caller internal/ads.Checker (dispatcher Check, dispatch mode, commerce_worker). For create_* ops: current attempt, approval hash current (AD5), approver still holds ads:approve, SANDBOX account rule (AD9), billing not RESTRICTED. Returns the BLOCKED_POLICY code or empty; reads only PG.';

CREATE FUNCTION ads.check_activate(p_operation uuid) RETURNS text
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE o record; d ads.campaign_drafts; st ads.store_settings; v_approval record; p record; v_ref text;
BEGIN
 SELECT x.tenant_id,x.store_id,x.action,x.request,x.external_asset_id INTO o FROM integration.operations x
  WHERE x.id=p_operation AND x.provider='meta_ads' AND x.actor_kind='MERCHANT';
 IF NOT FOUND OR o.action<>'meta.ads.activate' THEN RETURN 'unknown_action'; END IF;
 IF coalesce(o.request->>'draft_id','') !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  OR coalesce(o.request->>'attempt','') !~ '^[0-9]{1,2}$' THEN RETURN 'invalid_request'; END IF;
 -- AD6: the store row lock serializes concurrent activates; lock order is settings before draft everywhere.
 st:=ads.lock_settings(o.tenant_id,o.store_id);
 SELECT * INTO d FROM ads.campaign_drafts x WHERE x.tenant_id=o.tenant_id AND x.store_id=o.store_id AND x.id=(o.request->>'draft_id')::uuid;
 IF NOT FOUND THEN RETURN 'draft_missing'; END IF;
 -- X7 / pause-vs-activate: any pause op (any attempt, any state) or an ended draft forbids activation, zero HTTP.
 IF d.ended_at IS NOT NULL OR EXISTS(SELECT 1 FROM ads.remote_objects r WHERE r.draft_id=d.id AND r.kind='pause') THEN
  RETURN 'pause_requested'; END IF;
 IF (o.request->>'attempt')::integer<>d.publish_attempt THEN RETURN 'attempt_stale'; END IF;
 SELECT a.draft_sha256,a.principal_id INTO v_approval FROM ads.draft_approvals a WHERE a.draft_id=d.id AND a.revision=d.revision;
 IF NOT FOUND THEN RETURN 'not_approved'; END IF;
 IF v_approval.draft_sha256 IS DISTINCT FROM ads.draft_sha(d.id) THEN RETURN 'approval_stale'; END IF;
 IF NOT identity.principal_holds(d.tenant_id,d.store_id,v_approval.principal_id,ARRAY['ads:approve']) THEN RETURN 'approver_revoked'; END IF;
 IF ads.restricted(d.tenant_id,d.store_id) THEN RETURN 'billing_restricted'; END IF;
 IF d.currency<>st.allowance_currency THEN RETURN 'currency_mismatch'; END IF;
 -- I05: minor units of the allowance currency; this operation's own draft counts itself (its activate op is DISPATCHING).
 IF ads.allowance_used(d.tenant_id,d.store_id,d.id)+d.lifetime_budget_minor>st.max_active_budget_minor THEN RETURN 'over_allowance'; END IF;
 IF st.environment='SANDBOX' AND (st.sandbox_ad_account IS NULL OR o.external_asset_id<>st.sandbox_ad_account) THEN
  RETURN 'sandbox_account_required'; END IF;
 -- Fresh preflight of THIS attempt (<= 10 min): account active, currency = draft currency, funded in LIVE.
 SELECT q.provider_reference,q.updated_at INTO p FROM ads.remote_objects r JOIN integration.operations q ON q.id=r.operation_id
  WHERE r.draft_id=d.id AND r.publish_attempt=d.publish_attempt AND r.kind='preflight' AND q.state='SUCCEEDED'
  ORDER BY r.seq DESC LIMIT 1;
 IF NOT FOUND OR p.updated_at<clock_timestamp()-interval '10 minutes' THEN RETURN 'preflight_stale'; END IF;
 v_ref:=p.provider_reference;
 IF coalesce(substring(v_ref from '(?:^|;)st=([0-9]+)'),'')<>'1'
  OR coalesce(substring(v_ref from '(?:^|;)cur=([A-Z]{3})'),'')<>d.currency
  OR (st.environment='LIVE' AND coalesce(substring(v_ref from '(?:^|;)fund=([01])'),'0')<>'1') THEN
  RETURN 'account_not_ready'; END IF;
 RETURN '';
END $$;
REVOKE ALL ON FUNCTION ads.check_activate(uuid) FROM PUBLIC;
ALTER FUNCTION ads.check_activate(uuid) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.check_activate(uuid) TO commerce_worker;
COMMENT ON FUNCTION ads.check_activate(uuid) IS
 'ads owner; only caller internal/ads.Checker (dispatcher Check, dispatch mode, commerce_worker). The only spend switch: no pause op and not ended (pause_requested), approval hash + approver (AD5), billing, AD6 allowance under the store settings row lock (over_allowance), AD9 sandbox rule, fresh ready preflight of the same attempt (preflight_stale / account_not_ready). Returns the BLOCKED_POLICY code or empty; reads only PG.';

CREATE FUNCTION ads.check_read(p_operation uuid) RETURNS text
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE o record;
BEGIN
 SELECT x.action INTO o FROM integration.operations x WHERE x.id=p_operation AND x.provider='meta_ads' AND x.actor_kind='MERCHANT';
 -- pause, preflight_account and read_insights only read or reduce spend: the binding fence is the dispatcher's.
 IF NOT FOUND OR o.action NOT IN ('meta.ads.pause','meta.ads.preflight_account','meta.ads.read_insights') THEN RETURN 'unknown_action'; END IF;
 RETURN '';
END $$;
REVOKE ALL ON FUNCTION ads.check_read(uuid) FROM PUBLIC;
ALTER FUNCTION ads.check_read(uuid) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.check_read(uuid) TO commerce_worker;
COMMENT ON FUNCTION ads.check_read(uuid) IS
 'ads owner; only caller internal/ads.Checker (commerce_worker). pause, preflight_account and read_insights are allowed without an approver or billing check (spend-reducing or read-only); anything else is unknown_action.';

-- ---------------------------------------------------------------------------------------
-- Advance sweeper (contract §6.3). No network, no lock across I/O: each draft is one short transaction
-- (advance_next decides and books, the caller inserts the River job, advance_plan re-decides and plans).
-- ---------------------------------------------------------------------------------------
-- AD7: auto-pause is due when the campaign may be spending and reported spend reached the approved budget, or its
-- latest reported effective_status shows rejection. Never raises a budget, never re-activates.
CREATE FUNCTION ads.auto_pause_due(p_draft uuid,p_campaign text) RETURNS boolean
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE d ads.campaign_drafts; v_spend bigint; v_es text;
BEGIN
 SELECT * INTO d FROM ads.campaign_drafts x WHERE x.id=p_draft;
 IF NOT FOUND THEN RETURN false; END IF;
 IF NOT EXISTS(SELECT 1 FROM ads.remote_objects r JOIN integration.operations o ON o.id=r.operation_id
   WHERE r.draft_id=d.id AND r.kind='activate' AND o.state IN ('SUCCEEDED','UNKNOWN','DISPATCHING')) THEN RETURN false; END IF;
 -- I05: same-currency minor units only.
 SELECT coalesce(sum(i.spend_minor),0)::bigint INTO v_spend FROM ads.insights_daily i
  WHERE i.draft_id=d.id AND i.campaign_remote_id=p_campaign AND i.currency=d.currency;
 IF v_spend>=d.lifetime_budget_minor THEN RETURN true; END IF;
 SELECT i.effective_status INTO v_es FROM ads.insights_daily i WHERE i.draft_id=d.id AND i.campaign_remote_id=p_campaign
  ORDER BY i.day DESC LIMIT 1;
 -- ponytail: effective_status values from the Marketing API reference; MA-S1 confirms them and whether an account-level
 -- status (disabled account) needs its own insights key.
 RETURN coalesce(v_es,'') IN ('DISAPPROVED','WITH_ISSUES');
END $$;
REVOKE ALL ON FUNCTION ads.auto_pause_due(uuid,text) FROM PUBLIC;
ALTER FUNCTION ads.auto_pause_due(uuid,text) OWNER TO commerce_ads_writer;
COMMENT ON FUNCTION ads.auto_pause_due(uuid,text) IS
 'ads owner; internal helper of ads.advance_decide, no caller EXECUTE. AD7: true iff an activate may have taken effect and ingested spend >= the approved lifetime budget (same currency) or the latest effective_status is DISAPPROVED/WITH_ISSUES.';

CREATE FUNCTION ads.advance_decide(p_d ads.campaign_drafts,p_st ads.store_settings)
RETURNS TABLE(out_kind text,out_seq integer)
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_now timestamptz:=clock_timestamp(); v_campaign text; v_any_pause boolean; k text; op record; lp record; v_claim timestamptz;
 act record; pf record; v_ready boolean; v_ref text; v_seq integer;
BEGIN
 IF p_d.publish_attempt=0 THEN RETURN; END IF;
 SELECT r.remote_id INTO v_campaign FROM ads.remote_objects r
  WHERE r.draft_id=p_d.id AND r.publish_attempt=p_d.publish_attempt AND r.kind='campaign' AND r.seq=1;
 v_any_pause:=EXISTS(SELECT 1 FROM ads.remote_objects r WHERE r.draft_id=p_d.id AND r.kind='pause');
 -- (1) pause phase: needs the campaign id of the current attempt.
 IF v_campaign IS NOT NULL THEN
  SELECT r.seq,r.operation_id,o.state,o.created_at INTO lp FROM ads.remote_objects r JOIN integration.operations o ON o.id=r.operation_id
   WHERE r.draft_id=p_d.id AND r.publish_attempt=p_d.publish_attempt AND r.kind='pause' ORDER BY r.seq DESC LIMIT 1;
  IF NOT FOUND THEN
   IF p_d.ended_at IS NOT NULL OR ads.auto_pause_due(p_d.id,v_campaign) THEN
    out_kind:='pause'; out_seq:=1; RETURN NEXT; RETURN; END IF;
  ELSE
   -- An activate that reached SUCCEEDED/UNKNOWN after the latest pause was claimed may have overtaken it: pause again.
   SELECT min(e.created_at) INTO v_claim FROM integration.operation_events e WHERE e.operation_id=lp.operation_id AND e.state='DISPATCHING';
   IF v_claim IS NOT NULL AND lp.seq<50 AND EXISTS(SELECT 1 FROM ads.remote_objects r2 JOIN integration.operations o2 ON o2.id=r2.operation_id
     WHERE r2.draft_id=p_d.id AND r2.kind='activate' AND o2.state IN ('SUCCEEDED','UNKNOWN') AND o2.updated_at>v_claim) THEN
    out_kind:='pause'; out_seq:=lp.seq+1; RETURN NEXT; RETURN; END IF;
   -- §5.3: a pause not SUCCEEDED within 15 min is planned again (next seq, never the same key), with an ops alert.
   IF lp.state<>'SUCCEEDED' AND lp.seq<50 AND lp.created_at<v_now-interval '15 minutes' THEN
    out_kind:='pause_retry'; out_seq:=lp.seq+1; RETURN NEXT; RETURN; END IF;
  END IF;
 END IF;
 -- X7: nothing else is ever planned for a draft that has a pause op or has ended.
 IF v_any_pause OR p_d.ended_at IS NOT NULL THEN RETURN; END IF;
 -- (2) creation chain campaign -> adset -> creative -> ad (each planned after the previous is SUCCEEDED and pinned).
 FOREACH k IN ARRAY ARRAY['campaign','adset','creative','ad'] LOOP
  SELECT o.state,r.remote_id INTO op FROM ads.remote_objects r JOIN integration.operations o ON o.id=r.operation_id
   WHERE r.draft_id=p_d.id AND r.publish_attempt=p_d.publish_attempt AND r.kind=k AND r.seq=1;
  IF NOT FOUND THEN
   IF k='campaign' THEN RETURN; END IF;       -- ads.publish_draft plans it
   out_kind:=k; out_seq:=1; RETURN NEXT; RETURN;
  END IF;
  IF op.state<>'SUCCEEDED' OR op.remote_id IS NULL THEN RETURN; END IF;   -- in flight, UNKNOWN (reconcile), failed or not pinned yet
 END LOOP;
 -- (3) activation phase.
 IF EXISTS(SELECT 1 FROM ads.remote_objects r JOIN integration.operations o ON o.id=r.operation_id
   WHERE r.draft_id=p_d.id AND r.kind='activate' AND o.state IN ('READY','DISPATCHING','UNKNOWN','ACKNOWLEDGED','SUCCEEDED')) THEN RETURN; END IF;
 SELECT r.seq,o.state,o.result_code INTO act FROM ads.remote_objects r JOIN integration.operations o ON o.id=r.operation_id
  WHERE r.draft_id=p_d.id AND r.publish_attempt=p_d.publish_attempt AND r.kind='activate' ORDER BY r.seq DESC LIMIT 1;
 IF FOUND AND (act.state<>'BLOCKED_POLICY' OR act.result_code NOT IN ('preflight_stale','account_not_ready','over_allowance')) THEN
  RETURN;      -- FAILED/STALE, or a block only a merchant action can clear (billing, approval, sandbox, pause)
 END IF;
 v_seq:=coalesce(act.seq,0)+1;
 IF v_seq>50 THEN RETURN; END IF;
 SELECT r.seq,o.state,o.updated_at,o.provider_reference INTO pf FROM ads.remote_objects r JOIN integration.operations o ON o.id=r.operation_id
  WHERE r.draft_id=p_d.id AND r.publish_attempt=p_d.publish_attempt AND r.kind='preflight' ORDER BY r.seq DESC LIMIT 1;
 IF NOT FOUND THEN out_kind:='preflight'; out_seq:=1; RETURN NEXT; RETURN; END IF;
 IF pf.state IN ('READY','DISPATCHING','UNKNOWN','ACKNOWLEDGED') THEN RETURN; END IF;
 IF pf.state<>'SUCCEEDED' THEN
  -- rate-limited or blocked read: a read has no effect, so plan the next seq after a minute.
  IF pf.updated_at<v_now-interval '1 minute' AND pf.seq<50 THEN out_kind:='preflight'; out_seq:=pf.seq+1; RETURN NEXT; END IF;
  RETURN;
 END IF;
 v_ref:=pf.provider_reference;
 v_ready:=coalesce(substring(v_ref from '(?:^|;)st=([0-9]+)'),'')='1'
  AND coalesce(substring(v_ref from '(?:^|;)cur=([A-Z]{3})'),'')=p_d.currency
  AND (p_st.environment<>'LIVE' OR coalesce(substring(v_ref from '(?:^|;)fund=([01])'),'0')='1');
 -- Check accepts a preflight <= 10 min old; plan a new one after 8 (queue latency margin), or after 5 when not ready.
 IF (pf.updated_at<v_now-interval '8 minutes' OR (NOT v_ready AND pf.updated_at<v_now-interval '5 minutes')) AND pf.seq<50 THEN
  out_kind:='preflight'; out_seq:=pf.seq+1; RETURN NEXT; RETURN; END IF;
 IF NOT v_ready THEN RETURN; END IF;
 -- Room test incl. seq 1 and activates planned earlier in the same run (they are READY, hence counted): I05 minor units.
 IF ads.allowance_used(p_d.tenant_id,p_d.store_id,p_d.id)+p_d.lifetime_budget_minor>p_st.max_active_budget_minor THEN RETURN; END IF;
 out_kind:='activate'; out_seq:=v_seq; RETURN NEXT;
END $$;
REVOKE ALL ON FUNCTION ads.advance_decide(ads.campaign_drafts,ads.store_settings) FROM PUBLIC;
ALTER FUNCTION ads.advance_decide(ads.campaign_drafts,ads.store_settings) OWNER TO commerce_ads_writer;
COMMENT ON FUNCTION ads.advance_decide(ads.campaign_drafts,ads.store_settings) IS
 'ads owner; internal helper of ads.advance_next/advance_plan, no caller EXECUTE. The single decision of the advance sweeper (contract 6.3): pause (ended, AD7 auto-pause, late activate, 15-minute retry), then the create chain, then preflight, then activate only when the fresh ready preflight and the AD6 room test hold. Never plans anything for a draft that has a pause op or has ended (X7).';

CREATE FUNCTION ads.advance_candidates(p_limit integer, p_offset integer) RETURNS SETOF uuid
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 IF p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 500 OR p_offset IS NULL OR p_offset<0 THEN RAISE EXCEPTION 'invalid ads sweep' USING ERRCODE='22023'; END IF;
 -- ponytail: sequential scan of published drafts; add an index on publish_attempt>0 when a store holds thousands.
 RETURN QUERY SELECT d.id FROM ads.campaign_drafts d
  WHERE d.publish_attempt>0 AND (
   clock_timestamp()<d.ends_at+interval '3 days'
   OR EXISTS(SELECT 1 FROM ads.remote_objects r JOIN integration.operations o ON o.id=r.operation_id
    WHERE r.draft_id=d.id AND (o.state IN ('READY','DISPATCHING','UNKNOWN','ACKNOWLEDGED')
     OR (r.kind IN ('campaign','adset','creative','ad') AND o.state='SUCCEEDED' AND r.remote_id IS NULL)))
   OR (ads.draft_campaign(d.id) IS NOT NULL AND ads.draft_counts(d.id)))
  ORDER BY (SELECT a.approved_at FROM ads.draft_approvals a WHERE a.draft_id=d.id AND a.revision=d.revision) NULLS LAST,d.id
  LIMIT p_limit OFFSET p_offset;
END $$;
REVOKE ALL ON FUNCTION ads.advance_candidates(integer,integer) FROM PUBLIC;
ALTER FUNCTION ads.advance_candidates(integer,integer) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.advance_candidates(integer,integer) TO commerce_worker;
COMMENT ON FUNCTION ads.advance_candidates(integer,integer) IS
 'ads owner; only caller the ads_publish_advance_v1 periodic job (commerce_worker). Published drafts to look at, in approved_at order (the AD6 room test is order-dependent), selected by approvals, pinned ids and operation states, never by derived status (contract 6.3): inside ends_at + 3 days, or with a non-terminal or unpinned operation, or still counting toward the allowance. Drafts never published (attempt 0) have nothing to advance. Paged by (p_limit, p_offset) over one stable order: the caller lists every page before advancing any draft (advancing shifts the set), so no published draft is starved by a fixed first page.';

CREATE FUNCTION ads.advance_next(p_draft uuid) RETURNS text
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE d ads.campaign_drafts; st ads.store_settings; dec record;
BEGIN
 SELECT * INTO d FROM ads.campaign_drafts x WHERE x.id=p_draft;
 IF NOT FOUND THEN RETURN ''; END IF;
 st:=ads.lock_settings(d.tenant_id,d.store_id);
 SELECT * INTO d FROM ads.campaign_drafts x WHERE x.id=p_draft FOR UPDATE;
 -- Pin remote ids from SUCCEEDED create ops (set-once trigger). A SUCCEEDED create with a malformed reference stays
 -- unpinned: the chain then stops visibly instead of planning a step against a bad id.
 UPDATE ads.remote_objects r SET remote_id=o.provider_reference FROM integration.operations o
  WHERE o.id=r.operation_id AND r.draft_id=d.id AND r.kind IN ('campaign','adset','creative','ad') AND r.remote_id IS NULL
   AND o.state='SUCCEEDED' AND o.provider_reference ~ '^[0-9]{1,40}$';
 -- ENDED by time (contract 5.1); the pause follows from advance_decide.
 IF d.ended_at IS NULL AND clock_timestamp()>d.ends_at THEN
  UPDATE ads.campaign_drafts x SET ended_at=clock_timestamp() WHERE x.id=d.id;
  d.ended_at:=clock_timestamp();
 END IF;
 SELECT * INTO dec FROM ads.advance_decide(d,st);
 RETURN coalesce(dec.out_kind,'');
END $$;
REVOKE ALL ON FUNCTION ads.advance_next(uuid) FROM PUBLIC;
ALTER FUNCTION ads.advance_next(uuid) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.advance_next(uuid) TO commerce_worker;
COMMENT ON FUNCTION ads.advance_next(uuid) IS
 'ads owner; only caller the ads_publish_advance_v1 job (commerce_worker), first call of a per-draft transaction. Takes the store settings lock then the draft lock (held to commit), pins remote ids of SUCCEEDED creates, ends drafts past ends_at, and returns the kind of the next step to plan (adset|creative|ad|preflight|activate|pause|pause_retry) or empty. Plans nothing itself: the caller inserts the ads-lane River job first.';

CREATE FUNCTION ads.advance_plan(p_draft uuid,p_kind text,p_op uuid,p_job bigint) RETURNS void
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE d ads.campaign_drafts; st ads.store_settings; dec record; v_kind text; v_principal uuid;
BEGIN
 IF p_kind IS NULL OR p_kind NOT IN ('adset','creative','ad','preflight','activate','pause','pause_retry') THEN
  RAISE EXCEPTION 'invalid ads plan' USING ERRCODE='22023'; END IF;
 SELECT * INTO d FROM ads.campaign_drafts x WHERE x.id=p_draft;
 IF NOT FOUND THEN RAISE EXCEPTION 'invalid ads plan' USING ERRCODE='22023'; END IF;
 st:=ads.lock_settings(d.tenant_id,d.store_id);
 SELECT * INTO d FROM ads.campaign_drafts x WHERE x.id=p_draft FOR UPDATE;
 -- Same transaction as advance_next, locks still held: the decision cannot have moved. A mismatch is a caller bug.
 SELECT * INTO dec FROM ads.advance_decide(d,st);
 IF dec.out_kind IS DISTINCT FROM p_kind THEN RAISE EXCEPTION 'ads plan changed' USING ERRCODE='22023'; END IF;
 v_kind:=CASE WHEN p_kind='pause_retry' THEN 'pause' ELSE p_kind END;
 v_principal:=coalesce(ads.approver(d.id),d.created_by);
 PERFORM ads.plan_op(d,v_kind,dec.out_seq,d.publish_attempt,p_op,p_job,v_principal,
  ads.request_for(d,v_kind,d.publish_attempt,dec.out_seq,p_op));
END $$;
REVOKE ALL ON FUNCTION ads.advance_plan(uuid,text,uuid,bigint) FROM PUBLIC;
ALTER FUNCTION ads.advance_plan(uuid,text,uuid,bigint) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.advance_plan(uuid,text,uuid,bigint) TO commerce_worker;
COMMENT ON FUNCTION ads.advance_plan(uuid,text,uuid,bigint) IS
 'ads owner; only caller the ads_publish_advance_v1 job (commerce_worker) after advance_next returned p_kind and the caller inserted the ads-lane River job (priority 1 for pause, else 3) in the same transaction. Re-decides under the same locks, refuses a changed decision (22023), then plans the step with the approver (or the draft creator) as principal; the plan_op job check is the pattern of integration.plan_claim_reply.';

-- ---------------------------------------------------------------------------------------
-- Insights planner (contract §6.2, D9): plans reads only; ingestion is put_insights_day (0075).
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION ads.insights_candidates(p_limit integer) RETURNS SETOF uuid
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 IF p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 500 THEN RAISE EXCEPTION 'invalid ads sweep' USING ERRCODE='22023'; END IF;
 RETURN QUERY SELECT d.id FROM ads.campaign_drafts d
  WHERE d.publish_attempt>0 AND clock_timestamp()<d.ends_at+interval '3 days' AND ads.draft_campaign(d.id) IS NOT NULL
  ORDER BY d.id LIMIT p_limit;
END $$;
REVOKE ALL ON FUNCTION ads.insights_candidates(integer) FROM PUBLIC;
ALTER FUNCTION ads.insights_candidates(integer) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.insights_candidates(integer) TO commerce_worker;
COMMENT ON FUNCTION ads.insights_candidates(integer) IS
 'ads owner; only caller the ads_insights_plan_v1 hourly job (commerce_worker). Drafts with a pinned campaign id and ends_at + 3 days not yet passed, regardless of derived status (UNKNOWN, PAUSED, REJECTED included).';

-- D9: every hour while the draft still counts toward the allowance (AD6), else only in the store-local 04:00 hour.
-- No store timezone column exists, so the store-local zone is Asia/Taipei (brief D9 default).
CREATE FUNCTION ads.insights_days(p_draft uuid) RETURNS date[]
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE d ads.campaign_drafts; v_now timestamptz:=clock_timestamp(); v_today date; v_hour integer; v_days date[]:=ARRAY[]::date[]; x date;
 v_hourkey text;
BEGIN
 SELECT * INTO d FROM ads.campaign_drafts y WHERE y.id=p_draft;
 IF NOT FOUND OR ads.draft_campaign(d.id) IS NULL THEN RETURN v_days; END IF;
 v_today:=(v_now AT TIME ZONE 'Asia/Taipei')::date; v_hour:=extract(hour from v_now AT TIME ZONE 'Asia/Taipei')::integer;
 IF v_hour<>4 AND NOT ads.draft_counts(d.id) THEN RETURN v_days; END IF;
 v_hourkey:=to_char(v_now AT TIME ZONE 'UTC','YYYYMMDDHH24');
 FOR x IN SELECT g::date FROM generate_series(greatest((d.starts_at AT TIME ZONE 'Asia/Taipei')::date,v_today-3)::timestamp,v_today::timestamp,interval '1 day') g LOOP
  IF NOT EXISTS(SELECT 1 FROM integration.operations o WHERE o.tenant_id=d.tenant_id AND o.store_id=d.store_id
    AND o.semantic_key='ads:ins:'||d.id::text||':'||to_char(x,'YYYY-MM-DD')||':'||v_hourkey) THEN
   v_days:=v_days||x;
  END IF;
 END LOOP;
 RETURN v_days;
END $$;
REVOKE ALL ON FUNCTION ads.insights_days(uuid) FROM PUBLIC;
ALTER FUNCTION ads.insights_days(uuid) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.insights_days(uuid) TO commerce_worker;
COMMENT ON FUNCTION ads.insights_days(uuid) IS
 'ads owner; only caller the ads_insights_plan_v1 job (commerce_worker). The store-local days [max(start, today-3), today] still to be read this UTC hour (semantic key not yet used): every hour while AD6 counts the draft, else only in the 04:00 Asia/Taipei hour.';

CREATE FUNCTION ads.insights_plan(p_draft uuid,p_day date,p_op uuid,p_job bigint) RETURNS void
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE d ads.campaign_drafts; b record; v_campaign text; v_request jsonb; v_key text; v_principal uuid; v_hourkey text;
BEGIN
 IF p_day IS NULL OR p_op IS NULL OR p_job IS NULL OR p_job<=0 OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid ads plan' USING ERRCODE='22023'; END IF;
 SELECT * INTO d FROM ads.campaign_drafts x WHERE x.id=p_draft;
 IF NOT FOUND THEN RAISE EXCEPTION 'invalid ads plan' USING ERRCODE='22023'; END IF;
 v_campaign:=ads.draft_campaign(d.id);
 IF v_campaign IS NULL THEN RAISE EXCEPTION 'invalid ads plan' USING ERRCODE='22023'; END IF;
 PERFORM ads.verify_job(p_job,p_op,3);
 SELECT x.id,x.semantic_version,x.external_asset_id,x.enabled INTO b FROM integration.bindings x
  WHERE x.tenant_id=d.tenant_id AND x.store_id=d.store_id AND x.id=d.ad_binding_id AND x.provider='meta_ads';
 IF NOT FOUND OR NOT b.enabled THEN RAISE EXCEPTION 'invalid ads plan' USING ERRCODE='22023'; END IF;
 v_principal:=coalesce(ads.approver(d.id),d.created_by);
 v_hourkey:=to_char(clock_timestamp() AT TIME ZONE 'UTC','YYYYMMDDHH24');
 v_key:='ads:ins:'||d.id::text||':'||to_char(p_day,'YYYY-MM-DD')||':'||v_hourkey;
 v_request:=jsonb_build_object('v',1,'draft_id',d.id,'campaign_id',v_campaign,'day',to_char(p_day,'YYYY-MM-DD'));
 INSERT INTO integration.operations(tenant_id,store_id,id,principal_id,binding_id,binding_version,provider,external_asset_id,
  purpose,action,semantic_key,request_hash,request,job_id)
 VALUES(d.tenant_id,d.store_id,p_op,v_principal,b.id,b.semantic_version,'meta_ads',b.external_asset_id,'marketing',
  'meta.ads.read_insights',v_key,sha256(convert_to(v_request::text,'UTF8')),v_request,p_job);
 INSERT INTO integration.operation_events(tenant_id,store_id,operation_id,generation,state,mode,reason_code)
  VALUES(d.tenant_id,d.store_id,p_op,0,'READY','','operation_planned');
 INSERT INTO ads.insight_reads(tenant_id,store_id,operation_id,draft_id,day) VALUES(d.tenant_id,d.store_id,p_op,d.id,p_day);
END $$;
REVOKE ALL ON FUNCTION ads.insights_plan(uuid,date,uuid,bigint) FROM PUBLIC;
ALTER FUNCTION ads.insights_plan(uuid,date,uuid,bigint) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.insights_plan(uuid,date,uuid,bigint) TO commerce_worker;
COMMENT ON FUNCTION ads.insights_plan(uuid,date,uuid,bigint) IS
 'ads owner; only caller the ads_insights_plan_v1 job (commerce_worker) after inserting the priority-3 ads-lane River job in the same transaction. Plans one read_insights operation for the pinned campaign and store-local day (semantic key ads:ins:<draft>:<day>:<yyyymmddhh>) and its insight_reads marker; no token, no network.';

-- ---------------------------------------------------------------------------------------
-- Settings (merchant read + CAPI switch) and the operator definer (D7 / ruling B12).
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION ads.capi_json(p_st ads.store_settings) RETURNS jsonb
LANGUAGE sql IMMUTABLE SET search_path=pg_catalog AS $$
 SELECT jsonb_build_object('enabled',p_st.capi_enabled)
  ||CASE WHEN p_st.capi_dataset_binding IS NOT NULL THEN jsonb_build_object('dataset_binding_id',p_st.capi_dataset_binding) ELSE '{}'::jsonb END
  ||CASE WHEN p_st.capi_test_event_code IS NOT NULL THEN jsonb_build_object('test_event_code',p_st.capi_test_event_code) ELSE '{}'::jsonb END
$$;
REVOKE ALL ON FUNCTION ads.capi_json(ads.store_settings) FROM PUBLIC;
ALTER FUNCTION ads.capi_json(ads.store_settings) OWNER TO commerce_ads_writer;
COMMENT ON FUNCTION ads.capi_json(ads.store_settings) IS
 'ads owner; internal helper of ads.get_settings/set_capi, no caller EXECUTE. The capi object of the settings JSON.';

CREATE FUNCTION ads.get_settings(p_hash bytea,p_store uuid) RETURNS jsonb
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE a record; st ads.store_settings; v_conn jsonb; v_ident jsonb; v_out jsonb;
BEGIN
 SELECT * INTO a FROM ads.auth(p_hash,p_store,ARRAY['ads:read']);
 SELECT * INTO st FROM ads.store_settings s WHERE s.tenant_id=a.out_tenant AND s.store_id=p_store;
 IF NOT FOUND THEN
  st.tenant_id:=a.out_tenant; st.store_id:=p_store; st.environment:='SANDBOX'; st.max_active_budget_minor:=0;
  st.allowance_currency:='TWD'; st.capi_enabled:=false;      -- O4: allowance NT$0 (ads off) until the operator sets it
 END IF;
 SELECT coalesce(jsonb_agg(jsonb_build_object('binding_id',c.binding_id,'provider',b.provider,'asset_id',b.external_asset_id,
   'client_business_id',c.client_business_id,'enabled',b.enabled,'connected_at',ads.ts(c.connected_at)) ORDER BY c.connected_at,c.binding_id),'[]'::jsonb)
  INTO v_conn FROM ads.connections c JOIN integration.bindings b ON b.tenant_id=c.tenant_id AND b.store_id=c.store_id AND b.id=c.binding_id
  WHERE c.tenant_id=a.out_tenant AND c.store_id=p_store;
 SELECT coalesce(jsonb_agg(jsonb_build_object('binding_id',b.id,'provider',b.provider,'asset_id',b.external_asset_id) ORDER BY b.created_at,b.id),'[]'::jsonb)
  INTO v_ident FROM integration.bindings b WHERE b.tenant_id=a.out_tenant AND b.store_id=p_store AND b.enabled
   AND b.provider IN ('facebook','instagram');
 v_out:=jsonb_build_object('environment',st.environment,'allowance_currency',st.allowance_currency,
  'max_active_budget_minor',st.max_active_budget_minor,'capi',ads.capi_json(st),'connections',v_conn,'identities',v_ident);
 IF st.sandbox_ad_account IS NOT NULL THEN v_out:=v_out||jsonb_build_object('sandbox_ad_account',st.sandbox_ad_account); END IF;
 RETURN v_out;
END $$;
REVOKE ALL ON FUNCTION ads.get_settings(bytea,uuid) FROM PUBLIC;
ALTER FUNCTION ads.get_settings(bytea,uuid) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.get_settings(bytea,uuid) TO commerce_runtime;
COMMENT ON FUNCTION ads.get_settings(bytea,uuid) IS
 'ads owner; only caller the GET ads/settings route (commerce_runtime, ads:read, D5). Environment, allowance, sandbox account, CAPI switch, this store''s ads connections and its enabled Page/Instagram identities; defaults (SANDBOX, NT$0) when the operator has set nothing; no token material.';

CREATE FUNCTION ads.set_capi(p_hash bytea,p_store uuid,p_enabled boolean,p_dataset_binding uuid,p_test_event_code text) RETURNS jsonb
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE a record; st ads.store_settings; b record;
BEGIN
 IF p_enabled IS NULL OR (p_test_event_code IS NOT NULL AND p_test_event_code !~ '^[A-Z0-9]{4,20}$') THEN
  PERFORM ads.deny('invalid_request'); END IF;
 SELECT * INTO a FROM ads.auth(p_hash,p_store,ARRAY['ads:manage']);
 st:=ads.lock_settings(a.out_tenant,p_store);
 IF p_dataset_binding IS NOT NULL THEN
  SELECT x.provider,x.enabled INTO b FROM integration.bindings x WHERE x.tenant_id=a.out_tenant AND x.store_id=p_store AND x.id=p_dataset_binding;
  IF NOT FOUND OR b.provider<>'meta_dataset' THEN PERFORM ads.deny('invalid_request'); END IF;
  IF NOT b.enabled THEN PERFORM ads.deny('binding_disabled'); END IF;
  IF NOT EXISTS(SELECT 1 FROM ads.connections c WHERE c.tenant_id=a.out_tenant AND c.store_id=p_store AND c.binding_id=p_dataset_binding) THEN
   PERFORM ads.deny('invalid_request'); END IF;
 END IF;
 IF p_enabled AND (p_dataset_binding IS NULL OR (st.environment<>'LIVE' AND p_test_event_code IS NULL)) THEN
  PERFORM ads.deny('invalid_request'); END IF;
 -- capi_enabled_by = the resolved principal (the actor of every CAPI op, §6.4); never a caller-supplied id.
 UPDATE ads.store_settings x SET capi_enabled=p_enabled,capi_dataset_binding=p_dataset_binding,capi_test_event_code=p_test_event_code,
  capi_enabled_by=a.out_principal,updated_at=clock_timestamp() WHERE x.tenant_id=a.out_tenant AND x.store_id=p_store
  RETURNING * INTO st;
 RETURN ads.capi_json(st);
END $$;
REVOKE ALL ON FUNCTION ads.set_capi(bytea,uuid,boolean,uuid,text) FROM PUBLIC;
ALTER FUNCTION ads.set_capi(bytea,uuid,boolean,uuid,text) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.set_capi(bytea,uuid,boolean,uuid,text) TO commerce_runtime;
COMMENT ON FUNCTION ads.set_capi(bytea,uuid,boolean,uuid,text) IS
 'ads owner; only caller internal/ads.Service.SetCapi (commerce_runtime, ads:manage). The merchant''s only write to ads.store_settings: capi_enabled, dataset binding (a connected meta_dataset binding of the store), test event code, and capi_enabled_by = the resolved principal. environment, allowance and sandbox account are operator-only.';

CREATE FUNCTION ads.operator_set_settings(p_tenant uuid,p_store uuid,p_environment text,p_sandbox_ad_account text,
 p_max_active_budget_minor bigint,p_allowance_currency text) RETURNS timestamptz
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE st ads.store_settings; v_updated timestamptz;
BEGIN
 IF p_tenant IS NULL OR p_store IS NULL
  OR (p_environment IS NOT NULL AND p_environment NOT IN ('SANDBOX','LIVE'))
  OR (p_sandbox_ad_account IS NOT NULL AND p_sandbox_ad_account<>'' AND p_sandbox_ad_account !~ '^[0-9]{1,40}$')
  OR (p_max_active_budget_minor IS NOT NULL AND p_max_active_budget_minor NOT BETWEEN 0 AND 100000000000)
  OR (p_allowance_currency IS NOT NULL AND p_allowance_currency NOT IN ('TWD','USD','HKD'))
  OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid ads settings' USING ERRCODE='22023';
 END IF;
 IF NOT EXISTS(SELECT 1 FROM control.stores s WHERE s.tenant_id=p_tenant AND s.id=p_store) THEN
  RAISE EXCEPTION 'ads store unavailable' USING ERRCODE='P0002'; END IF;
 st:=ads.lock_settings(p_tenant,p_store);
 -- I05: one currency per store allowance; it cannot move under existing drafts of another currency.
 IF p_allowance_currency IS NOT NULL AND p_allowance_currency<>st.allowance_currency
  AND EXISTS(SELECT 1 FROM ads.campaign_drafts d WHERE d.tenant_id=p_tenant AND d.store_id=p_store AND d.currency<>p_allowance_currency) THEN
  RAISE EXCEPTION 'ads allowance currency in use' USING ERRCODE='23514'; END IF;
 -- NULL keeps the current value; '' clears the sandbox account.
 UPDATE ads.store_settings x SET environment=coalesce(p_environment,x.environment),
  sandbox_ad_account=CASE WHEN p_sandbox_ad_account IS NULL THEN x.sandbox_ad_account WHEN p_sandbox_ad_account='' THEN NULL ELSE p_sandbox_ad_account END,
  max_active_budget_minor=coalesce(p_max_active_budget_minor,x.max_active_budget_minor),
  allowance_currency=coalesce(p_allowance_currency,x.allowance_currency),updated_at=clock_timestamp()
  WHERE x.tenant_id=p_tenant AND x.store_id=p_store RETURNING * INTO st;
 INSERT INTO ads.operator_events(tenant_id,store_id,action,environment,sandbox_ad_account,max_active_budget_minor,allowance_currency)
  VALUES(p_tenant,p_store,'ads.operator_settings_changed',st.environment,st.sandbox_ad_account,st.max_active_budget_minor,st.allowance_currency);
 v_updated:=st.updated_at;
 RETURN v_updated;
END $$;
REVOKE ALL ON FUNCTION ads.operator_set_settings(uuid,uuid,text,text,bigint,text) FROM PUBLIC;
ALTER FUNCTION ads.operator_set_settings(uuid,uuid,text,text,bigint,text) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.operator_set_settings(uuid,uuid,text,text,bigint,text) TO commerce_meta_registrar;
COMMENT ON FUNCTION ads.operator_set_settings(uuid,uuid,text,text,bigint,text) IS
 'ads owner; only caller cmd/meta-admin ads-settings (commerce_meta_registrar, ruling B12). Sets environment (LIVE only after owner approval in chat), the sandbox ad account, the per-store active-budget allowance (O4: default 0 = ads off) and its currency; NULL keeps a value, empty sandbox account clears it. Audited in ads.operator_events (no principal exists for the operator); returns updated_at.';

-- ---------------------------------------------------------------------------------------
-- Documentation (PROCESS §5: every new table, column, function, role, policy names owner, roles, non-goals).
-- ---------------------------------------------------------------------------------------
COMMENT ON TABLE ads.store_settings IS
 'internal/ads: per-store ads switches. environment/sandbox_ad_account/max_active_budget_minor/allowance_currency written only by ads.operator_set_settings (cmd/meta-admin); capi_* only by ads.set_capi (merchant). Read by ads definers of commerce_ads_writer; no runtime grant. Non-goal: no billing or consent state.';
COMMENT ON TABLE ads.oauth_states IS
 'internal/ads: single-use FLfB OAuth state per principal and store (10 min) holding the pick list and the HPKE-sealed pending token until bind or expires_at. Written by ads.begin_connect/consume_state/put_connect_result/finish_bind/purge_oauth_states; read by integration.register_meta_ads_token. Non-goal: never a plaintext token.';
COMMENT ON TABLE ads.connections IS
 'internal/ads: one row per ads binding (meta_ads, meta_dataset) with the client business the token belongs to (U7 guard). Written only by integration.register_meta_ads_token (commerce_integration_writer).';
COMMENT ON TABLE ads.campaign_drafts IS
 'internal/ads: merchant campaign drafts (one campaign, one ad set, one ad each). Frozen columns change only while no approval exists for the current revision (trigger ads.guard_draft_update). Written only by ads.* definers; the derived status is display-only.';
COMMENT ON TABLE ads.draft_approvals IS
 'internal/ads: append-only approvals freezing SHA-256 of ads.canonical_draft per draft revision (AD5). Written only by ads.approve_draft; re-verified by every create/activate Check.';
COMMENT ON TABLE ads.remote_objects IS
 'internal/ads: one row per remote publish step (campaign, adset, creative, ad, preflight, activate, pause) linking its integration.operations row; remote_id is set once from the operation''s provider_reference by ads.advance_next. Written only by ads planners.';
COMMENT ON TABLE ads.operator_events IS
 'internal/ads: append-only audit of operator changes to ads.store_settings (no principal exists for the operator CLI). Written only by ads.operator_set_settings.';
COMMENT ON COLUMN ads.oauth_states.pending_ciphertext IS 'HPKE-sealed Meta BISU token (X25519-HKDF-SHA256 / AES-256-GCM); deleted on bind or at expires_at; read only by integration.register_meta_ads_token.';
COMMENT ON COLUMN ads.oauth_states.state_hash IS 'SHA-256 of the OAuth state parameter; the raw state exists only in the merchant browser URL.';

DO $$
DECLARE r record; c text;
BEGIN
 FOR r IN SELECT * FROM (VALUES
  ('ads.store_settings','tenant_id,store_id,updated_at','Scope (FK control.stores) and last change; written only by ads definers.'),
  ('ads.store_settings','environment,sandbox_ad_account,max_active_budget_minor,allowance_currency','Operator-only (ads.operator_set_settings): SANDBOX/LIVE gate (AD9), the sandbox ad account that is the only mutable target in SANDBOX, the per-store ceiling of active budgets in minor units (O4 default 0 = ads off) and its currency (I05).'),
  ('ads.store_settings','capi_enabled,capi_dataset_binding,capi_enabled_by,capi_test_event_code','Merchant CAPI switch (ads.set_capi): the dataset binding, the resolved principal that acts for CAPI operations, and the Test Events code SANDBOX requires.'),
  ('ads.oauth_states','id,tenant_id,store_id,principal_id,created_at,used_at,expires_at','State identity, the principal and store it is bound to, and its single-use/10-minute lifetime.'),
  ('ads.oauth_states','client_business_id,pick_list,scopes_attested','Result of the code exchange: the Meta client business, selectable ad accounts/datasets (ids and names only) and the attested permission scopes.'),
  ('ads.oauth_states','pending_key_id,pending_enc','HPKE key id and 32-byte encapsulated key of the pending sealed token.'),
  ('ads.connections','tenant_id,store_id,binding_id,client_business_id,token_version,oauth_state_id,connected_by,connected_at','The ads binding, the Meta client business it was connected under (a different business needs a new binding), the current token version (mirrors integration.meta_page_heads for the CAS), the consumed state and the connecting principal.'),
  ('ads.campaign_drafts','tenant_id,store_id,id,created_by,created_at','Scope, draft id and creator.'),
  ('ads.campaign_drafts','ad_binding_id,ad_binding_version,identity_binding_id','The meta_ads binding and its frozen semantic version, and the Page/Instagram identity binding whose CURRENT version enters the approval hash.'),
  ('ads.campaign_drafts','template,source_ref,currency,lifetime_budget_minor,starts_at,ends_at,countries,age_min,age_max','The approved campaign definition: template, existing post/media or product id, currency, lifetime budget in minor units (TWD whole NT$, I05), schedule (<= 30 days) and targeting.'),
  ('ads.campaign_drafts','revision,publish_attempt,ended_at','Edit counter, publish attempt counter (CAS, max 5) and the end marker after which nothing is planned (X7).'),
  ('ads.draft_approvals','tenant_id,store_id,draft_id,revision,draft_sha256,principal_id,approved_at','The approval of one draft revision: SHA-256 of the canonical draft (D10) and the approving principal.'),
  ('ads.remote_objects','tenant_id,store_id,draft_id,publish_attempt,kind,seq,operation_id,remote_id','One publish step of an attempt: its kind and seq, the operation that runs it and the remote object id pinned from its result.'),
  ('ads.operator_events','id,tenant_id,store_id,action,environment,sandbox_ad_account,max_active_budget_minor,allowance_currency,created_at','Operator change record: the resulting settings and when.')
 ) AS v(tbl,cols,why)
 LOOP
  FOREACH c IN ARRAY string_to_array(r.cols,',') LOOP
   IF col_description(r.tbl::regclass,(SELECT a.attnum FROM pg_attribute a WHERE a.attrelid=r.tbl::regclass AND a.attname=c)) IS NULL THEN
    EXECUTE format('COMMENT ON COLUMN %s.%I IS %L',r.tbl,c,r.why);
   END IF;
  END LOOP;
 END LOOP;
END $$;

DO $$
DECLARE r record;
BEGIN
 FOR r IN SELECT * FROM (VALUES
  ('ads_writer_all','ads.store_settings'),('ads_writer_all','ads.oauth_states'),('ads_writer_read','ads.connections'),
  ('ads_writer_all','ads.campaign_drafts'),('ads_writer_all','ads.draft_approvals'),('ads_writer_all','ads.remote_objects'),
  ('ads_writer_insert','ads.operator_events'),
  ('integration_writer_state_read','ads.oauth_states'),('integration_writer_state_lock','ads.oauth_states'),
  ('integration_writer_connection_read','ads.connections'),('integration_writer_connection_insert','ads.connections'),
  ('integration_writer_connection_update','ads.connections'),
  ('ads_writer_operation_read','integration.operations'),('ads_writer_operation_insert','integration.operations'),
  ('ads_writer_event_read','integration.operation_events'),('ads_writer_event_insert','integration.operation_events'),
  ('ads_writer_binding_read','integration.bindings'),('ads_writer_fact_read','payments.facts'),
  ('ads_writer_refund_fact_read','payments.refund_facts'),('ads_writer_store_read','control.stores'),
  ('ads_writer_domain_read','control.storefront_domains'),('ads_writer_publication_read','control.storefront_publications'),
  ('ads_writer_product_read','catalog.products')
 ) AS v(pol,tbl)
 LOOP
  EXECUTE format('COMMENT ON POLICY %I ON %s IS %L',r.pol,r.tbl,
   'T15 ads (migrations/0074): policy of commerce_ads_writer or the integration writer for the ads definers only; removing it makes the corresponding definer read zero rows (allowance sums fail open), so MA02 asserts it exists. Owner package internal/ads; non-goal: no merchant or buyer access.');
 END LOOP;
END $$;
