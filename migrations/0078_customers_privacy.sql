-- 0078 customers + owner-level privacy (contracts/customers-billing-v1.md §3.1 FROZEN 2026-09-30, rulings C-1/C-2/C-3/C-6/C-7;
-- docs/delivery/units/customers-core.md defaults D1-D14).
--
-- Owns: schema customers (append-only consent_events and privacy_actions), the NOLOGIN definer role
-- commerce_privacy_writer, the single consent gate customers.consent_allows, the buyer/merchant consent,
-- export and erasure definers, the merchant customer projection identity.read_merchant_customers and the
-- BD7 finance summary identity.read_finance_summary / export_finance_summary, plus the permission
-- vocabulary customers:read / customers:privacy.
--
-- Non-goals: no identity merge (CD1/CD2), no marketing send or CAPI call (R3), no grant row for any
-- principal (billing 0079 owns create_initial_store, C-5), no write to actor_key, claims.meta_intake,
-- live.claim_sources or social.* (CD7: a redeemed link is not identity proof), no reference to any role
-- created by a later-shipping file (C-7: R3 grants consent_allows to commerce_ads_writer in 0080), no order,
-- payment, refund or shipment write (orders, facts and snapshots referenced by orders are retained, U08).
--
-- Depends on: 0006 (buyer.owners/capability_sessions/events), 0012/0013 (destination snapshots, checkout.orders,
-- buyer definer pattern), 0027 + 0063 (commerce_auth read projection, merchant_access_denied, export audit pattern),
-- 0060/0064 (claims.bundles, platform vocabulary), 0061/0062 (stripe_sessions, stripe_refunds, refund_facts),
-- 0065 (permission vocabulary fixed point).
--
-- Callers: internal/customers (all customers.* and identity.read_merchant_customers), internal/reporting
-- (finance summary). commerce_runtime (merchant) and commerce_buyer_runtime (buyer) reach this only through the
-- definers below; no login role reads customers.* directly.

-- ---------------------------------------------------------------------------------------
-- Preconditions: fail closed when an upstream migration or grant is missing.
-- ---------------------------------------------------------------------------------------
DO $$
DECLARE v_def text; v_list text[]; v_perm text; v_role text;
BEGIN
 IF to_regclass('claims.bundles') IS NULL OR to_regclass('claims.meta_intake') IS NULL
  OR to_regclass('fulfillment.manual_shipment_heads') IS NULL OR to_regclass('payments.stripe_refunds') IS NULL
  OR to_regclass('payments.stripe_sessions') IS NULL OR to_regclass('storefront.destination_snapshots') IS NULL THEN
  RAISE EXCEPTION '0078 requires 0060, 0061, 0062, 0063, 0064 (claims, payments, fulfilment tables)';
 END IF;
 IF to_regprocedure('identity.merchant_access_denied(bytea,uuid,text[],uuid,uuid,bigint)') IS NULL
  OR to_regprocedure('identity.resolve_access(bytea,uuid,text)') IS NULL
  OR to_regprocedure('buyer.resolve_scope(bytea,uuid)') IS NULL THEN
  RAISE EXCEPTION '0078 requires identity.merchant_access_denied (0063), identity.resolve_access (0003) and buyer.resolve_scope (0006)';
 END IF;
 FOREACH v_role IN ARRAY ARRAY['commerce_auth','commerce_runtime','commerce_buyer_runtime','commerce_buyer_writer','commerce_checkout_writer'] LOOP
  IF to_regrole(v_role) IS NULL THEN RAISE EXCEPTION '0078 requires role %',v_role; END IF;
 END LOOP;
 -- Re-derive the CURRENT permission constraint (0065 is the fixed point) instead of rebuilding it from an older
 -- file, and extend it only when a value is absent: R3 0074 may apply after 0079 (C-7), so no value may be dropped.
 SELECT pg_get_constraintdef(c.oid) INTO STRICT v_def FROM pg_constraint c
  WHERE c.conrelid='identity.store_grants'::regclass AND c.conname='store_grants_permission_check';
 SELECT array_agg(t.m[1] ORDER BY t.ord) INTO v_list
  FROM regexp_matches(v_def,'''([a-z_]+:[a-z_]+)''::text','g') WITH ORDINALITY AS t(m,ord);
 IF v_list IS NULL OR NOT ('orders:read'=ANY(v_list) AND 'integration:execute'=ANY(v_list) AND 'orders:export'=ANY(v_list)) THEN
  RAISE EXCEPTION '0078 applied out of order or unexpected store_grants_permission_check: %',v_def;
 END IF;
 FOREACH v_perm IN ARRAY ARRAY['customers:read','customers:privacy'] LOOP
  IF NOT v_perm=ANY(v_list) THEN v_list:=v_list||v_perm; END IF;
 END LOOP;
 ALTER TABLE identity.store_grants DROP CONSTRAINT store_grants_permission_check;
 EXECUTE format('ALTER TABLE identity.store_grants ADD CONSTRAINT store_grants_permission_check CHECK (permission IN (%s))',
  (SELECT string_agg(quote_literal(p),',' ORDER BY ord) FROM unnest(v_list) WITH ORDINALITY AS u(p,ord)));
END $$;

-- ---------------------------------------------------------------------------------------
-- Role, schema, tables (append-only: no UPDATE/DELETE grant to anyone, plus a guard trigger).
-- ---------------------------------------------------------------------------------------
CREATE ROLE commerce_privacy_writer NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
CREATE SCHEMA customers;
REVOKE ALL ON SCHEMA customers FROM PUBLIC;
-- Callers need schema USAGE to resolve the definer names; that is not table access (no table grant below).
GRANT USAGE ON SCHEMA customers TO commerce_privacy_writer, commerce_auth, commerce_runtime, commerce_buyer_runtime;

CREATE TABLE customers.consent_events (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, owner_id uuid NOT NULL, id uuid NOT NULL DEFAULT gen_random_uuid(),
 purpose text NOT NULL CHECK (purpose IN ('marketing_messages','ads_personalization')),
 channel text NOT NULL,
 granted boolean NOT NULL,
 source text NOT NULL CHECK (source IN ('buyer_checkout','buyer_settings','merchant_recorded','erasure')),
 policy_version text NOT NULL CHECK (policy_version ~ '^[a-z0-9][a-z0-9._-]{0,39}$'),
 principal_id uuid,                                   -- merchant_recorded only
 request_key uuid NOT NULL,                           -- caller idempotency key (Go derives the uuid from the header)
 occurred_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (tenant_id,store_id,id),
 UNIQUE (tenant_id,store_id,owner_id,request_key,purpose,channel),   -- erasure writes one row per pair under its key
 FOREIGN KEY (tenant_id,store_id,owner_id) REFERENCES buyer.owners(tenant_id,store_id,id),
 FOREIGN KEY (tenant_id,principal_id) REFERENCES identity.memberships(tenant_id,principal_id),
 CHECK ((purpose='marketing_messages' AND channel='meta_dm') OR (purpose='ads_personalization' AND channel='meta_ads')),
 CHECK (NOT granted OR source IN ('buyer_checkout','buyer_settings')),     -- only the buyer grants (CD4)
 CHECK ((source='merchant_recorded')=(principal_id IS NOT NULL)));
-- One buyer/merchant request = one row, always recorded even when the state is unchanged (I02).
CREATE UNIQUE INDEX consent_one_key ON customers.consent_events(tenant_id,store_id,owner_id,request_key)
 WHERE source<>'erasure';
CREATE INDEX consent_current ON customers.consent_events(tenant_id,store_id,owner_id,purpose,channel,occurred_at DESC,id DESC);

CREATE TABLE customers.privacy_actions (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, id uuid NOT NULL DEFAULT gen_random_uuid(),
 owner_id uuid NOT NULL,
 kind text NOT NULL CHECK (kind IN ('EXPORT','ERASURE')),
 via text NOT NULL CHECK (via IN ('buyer','merchant','restore')),      -- restore: tombstone re-inserted by replay_erasures
 principal_id uuid,
 request_key uuid NOT NULL,
 summary jsonb NOT NULL CHECK (jsonb_typeof(summary)='object' AND octet_length(summary::text)<=1024), -- counts only, no PII
 completed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (tenant_id,store_id,id),
 UNIQUE (tenant_id,store_id,owner_id,request_key),
 FOREIGN KEY (tenant_id,store_id,owner_id) REFERENCES buyer.owners(tenant_id,store_id,id),
 FOREIGN KEY (tenant_id,principal_id) REFERENCES identity.memberships(tenant_id,principal_id),
 CHECK ((via='merchant')=(principal_id IS NOT NULL)));
CREATE UNIQUE INDEX privacy_one_erasure ON customers.privacy_actions(tenant_id,store_id,owner_id) WHERE kind='ERASURE';
-- Per-owner order reads: customer list/detail aggregates, erasure refusal checks.
CREATE INDEX orders_by_owner ON checkout.orders(tenant_id,store_id,owner_id,created_at DESC,id DESC);
-- Per-owner bound bundles (list claims_count/platforms, erasure relabel); claims_bundle_page is per session.
CREATE INDEX bundles_by_owner ON claims.bundles(tenant_id,store_id,owner_id) WHERE owner_id IS NOT NULL;

ALTER TABLE customers.consent_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE customers.consent_events FORCE ROW LEVEL SECURITY;
ALTER TABLE customers.privacy_actions ENABLE ROW LEVEL SECURITY;
ALTER TABLE customers.privacy_actions FORCE ROW LEVEL SECURITY;
REVOKE ALL ON customers.consent_events, customers.privacy_actions FROM PUBLIC;

-- Defence in depth for "append-only": the table owner is not a login, but a stray UPDATE/DELETE still fails.
CREATE FUNCTION customers.guard_append_only() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 RAISE EXCEPTION 'customers consent and privacy history is append-only' USING ERRCODE='42501';
END $$;
REVOKE ALL ON FUNCTION customers.guard_append_only() FROM PUBLIC;
CREATE TRIGGER consent_events_append_only BEFORE UPDATE OR DELETE ON customers.consent_events
 FOR EACH ROW EXECUTE FUNCTION customers.guard_append_only();
CREATE TRIGGER privacy_actions_append_only BEFORE UPDATE OR DELETE ON customers.privacy_actions
 FOR EACH ROW EXECUTE FUNCTION customers.guard_append_only();

-- ---------------------------------------------------------------------------------------
-- Grants and policies for commerce_privacy_writer (C-6: one writer, column-level grants on other domains).
-- SELECT policies on customers.*, buyer.owners and buyer.capability_sessions are USING(true): consent_allows
-- must work from any GUC state (R3 sweeper) and the buyer erasure retry must find a revoked session before any
-- scope exists; every definer filters tenant/store/owner explicitly. Every WRITE (and every read on another
-- domain's table) is scoped to the definer's authenticated GUCs (the 0063 writer pattern).
-- ---------------------------------------------------------------------------------------
GRANT USAGE ON SCHEMA identity, control, ops, buyer, checkout, storefront, claims, payments TO commerce_privacy_writer;
GRANT EXECUTE ON FUNCTION identity.resolve_access(bytea,uuid,text) TO commerce_privacy_writer;
GRANT EXECUTE ON FUNCTION buyer.resolve_scope(bytea,uuid) TO commerce_privacy_writer;

GRANT SELECT, INSERT ON customers.consent_events, customers.privacy_actions TO commerce_privacy_writer;
CREATE POLICY privacy_consent_read ON customers.consent_events FOR SELECT TO commerce_privacy_writer USING (true);
CREATE POLICY privacy_action_read ON customers.privacy_actions FOR SELECT TO commerce_privacy_writer USING (true);
CREATE POLICY privacy_consent_insert ON customers.consent_events FOR INSERT TO commerce_privacy_writer
 WITH CHECK (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
CREATE POLICY privacy_action_insert ON customers.privacy_actions FOR INSERT TO commerce_privacy_writer
 WITH CHECK (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);

-- buyer.owners: read everywhere (consent_allows checks active), lock + deactivate only inside the scope.
GRANT SELECT ON buyer.owners TO commerce_privacy_writer;
GRANT UPDATE (active) ON buyer.owners TO commerce_privacy_writer;
CREATE POLICY privacy_owner_read ON buyer.owners FOR SELECT TO commerce_privacy_writer USING (true);
CREATE POLICY privacy_owner_update ON buyer.owners FOR UPDATE TO commerce_privacy_writer
 USING (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid)
 WITH CHECK (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);

GRANT SELECT ON buyer.capability_sessions TO commerce_privacy_writer;
GRANT UPDATE (revoked_at) ON buyer.capability_sessions TO commerce_privacy_writer;
CREATE POLICY privacy_session_read ON buyer.capability_sessions FOR SELECT TO commerce_privacy_writer USING (true);
CREATE POLICY privacy_session_update ON buyer.capability_sessions FOR UPDATE TO commerce_privacy_writer
 USING (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid)
 WITH CHECK (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
GRANT INSERT ON buyer.capability_events TO commerce_privacy_writer;
CREATE POLICY privacy_event_insert ON buyer.capability_events FOR INSERT TO commerce_privacy_writer
 WITH CHECK (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);

GRANT SELECT (tenant_id,store_id,owner_id,id,kind,recipient_name,phone) ON storefront.destination_snapshots TO commerce_privacy_writer;
GRANT UPDATE (recipient_name,phone,region,city,postal_code,line1,line2) ON storefront.destination_snapshots TO commerce_privacy_writer;
CREATE POLICY privacy_destination_read ON storefront.destination_snapshots FOR SELECT TO commerce_privacy_writer
 USING (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
CREATE POLICY privacy_destination_update ON storefront.destination_snapshots FOR UPDATE TO commerce_privacy_writer
 USING (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid)
 WITH CHECK (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);

GRANT SELECT (tenant_id,store_id,owner_id,id,destination_id,commercial_state,expires_at) ON checkout.orders TO commerce_privacy_writer;
CREATE POLICY privacy_order_read ON checkout.orders FOR SELECT TO commerce_privacy_writer
 USING (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);

GRANT SELECT (tenant_id,store_id,id,session_id,platform,owner_id,bound_at,label,line_count) ON claims.bundles TO commerce_privacy_writer;
GRANT UPDATE (label) ON claims.bundles TO commerce_privacy_writer;
CREATE POLICY privacy_bundle_read ON claims.bundles FOR SELECT TO commerce_privacy_writer
 USING (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
CREATE POLICY privacy_bundle_update ON claims.bundles FOR UPDATE TO commerce_privacy_writer
 USING (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid)
 WITH CHECK (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);

GRANT SELECT (tenant_id,store_id,owner_id,expires_at) ON payments.stripe_sessions TO commerce_privacy_writer;
CREATE POLICY privacy_stripe_session_read ON payments.stripe_sessions FOR SELECT TO commerce_privacy_writer
 USING (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
GRANT SELECT (tenant_id,store_id,id,owner_id) ON payments.stripe_refunds TO commerce_privacy_writer;
CREATE POLICY privacy_stripe_refund_read ON payments.stripe_refunds FOR SELECT TO commerce_privacy_writer
 USING (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
GRANT SELECT (tenant_id,store_id,refund_id,kind) ON payments.refund_facts TO commerce_privacy_writer;
CREATE POLICY privacy_refund_fact_read ON payments.refund_facts FOR SELECT TO commerce_privacy_writer
 USING (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);

-- Store display name for the buyer export envelope only (D8); scoped like every other cross-domain read.
GRANT SELECT (tenant_id,id,name) ON control.stores TO commerce_privacy_writer;
CREATE POLICY privacy_store_read ON control.stores FOR SELECT TO commerce_privacy_writer
 USING (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND id=nullif(current_setting('app.store_id',true),'')::uuid);

-- Audit rows: only the three customers.* merchant actions, scoped to the authenticated GUCs.
GRANT INSERT ON ops.audit_events TO commerce_privacy_writer;
CREATE POLICY privacy_audit_insert ON ops.audit_events FOR INSERT TO commerce_privacy_writer
 WITH CHECK (action IN ('customers.consent_withdrawn','customers.exported','customers.erased')
  AND tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND principal_id=nullif(current_setting('app.principal_id',true),'')::uuid);

-- ---------------------------------------------------------------------------------------
-- Grants for the commerce_auth read definers (0027 pattern: NOLOGIN definer owner, unscoped SELECT policy,
-- the definer filters tenant/store itself after fresh authorization).
-- ---------------------------------------------------------------------------------------
GRANT USAGE ON SCHEMA buyer, claims, customers TO commerce_auth;
GRANT SELECT (tenant_id,store_id,id,active,created_at) ON buyer.owners TO commerce_auth;
CREATE POLICY auth_customer_owner_read ON buyer.owners FOR SELECT TO commerce_auth USING (true);
GRANT SELECT (tenant_id,store_id,id,session_id,platform,owner_id,bound_at,line_count) ON claims.bundles TO commerce_auth;
CREATE POLICY auth_customer_bundle_read ON claims.bundles FOR SELECT TO commerce_auth USING (true);
GRANT SELECT ON customers.consent_events, customers.privacy_actions TO commerce_auth;
CREATE POLICY auth_consent_read ON customers.consent_events FOR SELECT TO commerce_auth USING (true);
CREATE POLICY auth_privacy_action_read ON customers.privacy_actions FOR SELECT TO commerce_auth USING (true);
-- Finance day of a CAPTURED fact = received_at in Asia/Taipei (0027 granted every other facts column).
GRANT SELECT (received_at) ON payments.facts TO commerce_auth;
CREATE POLICY auth_finance_export_audit ON ops.audit_events FOR INSERT TO commerce_auth
 WITH CHECK (action='finance.exported'
  AND tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND principal_id=nullif(current_setting('app.principal_id',true),'')::uuid);

-- ---------------------------------------------------------------------------------------
-- consent_allows: THE consent gate (CD5). Latest event granted AND owner active. Unknown owner, other store or
-- an invalid pair yields no row and therefore false, never an error. STABLE, reads only by its arguments.
-- EXECUTE commerce_auth only; R3 grants its planners in 0080 (C-7).
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION customers.consent_allows(p_tenant uuid,p_store uuid,p_owner uuid,p_purpose text,p_channel text)
RETURNS boolean LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT coalesce((SELECT e.granted FROM customers.consent_events e
   WHERE e.tenant_id=p_tenant AND e.store_id=p_store AND e.owner_id=p_owner
    AND e.purpose=p_purpose AND e.channel=p_channel
   ORDER BY e.occurred_at DESC,e.id DESC LIMIT 1),false)
  AND coalesce((SELECT o.active FROM buyer.owners o
   WHERE o.tenant_id=p_tenant AND o.store_id=p_store AND o.id=p_owner),false)
$$;
ALTER FUNCTION customers.consent_allows(uuid,uuid,uuid,text,text) OWNER TO commerce_privacy_writer;
REVOKE ALL ON FUNCTION customers.consent_allows(uuid,uuid,uuid,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION customers.consent_allows(uuid,uuid,uuid,text,text) TO commerce_auth;

-- ---------------------------------------------------------------------------------------
-- apply_erasure (internal, EXECUTE nobody): CD7 steps, idempotent. Caller holds the owners row lock and the
-- GUC scope. request_key of the withdrawal rows = the owner id (deterministic, so a replay inserts nothing new).
-- Never writes actor_key, claims.meta_intake, live.claim_sources or social.*; orders/facts are untouched.
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION customers.apply_erasure(p_tenant uuid,p_store uuid,p_owner uuid)
RETURNS jsonb LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_consents integer; v_sessions integer; v_snapshots integer; v_bundles integer;
BEGIN
 -- CD4: withdraw every granted pair (source erasure, policy erasure).
 INSERT INTO customers.consent_events(tenant_id,store_id,owner_id,purpose,channel,granted,source,policy_version,request_key)
 SELECT p_tenant,p_store,p_owner,x.purpose,x.channel,false,'erasure','erasure',p_owner
  FROM (VALUES('marketing_messages','meta_dm'),('ads_personalization','meta_ads')) AS x(purpose,channel)
  WHERE coalesce((SELECT e.granted FROM customers.consent_events e
    WHERE e.tenant_id=p_tenant AND e.store_id=p_store AND e.owner_id=p_owner AND e.purpose=x.purpose AND e.channel=x.channel
    ORDER BY e.occurred_at DESC,e.id DESC LIMIT 1),false)
  ON CONFLICT DO NOTHING;
 GET DIAGNOSTICS v_consents=ROW_COUNT;
 -- Revoke every live capability session; the UNIQUE(session_id,action) event is written once per session.
 WITH revoked AS (
  UPDATE buyer.capability_sessions c SET revoked_at=clock_timestamp()
   WHERE c.tenant_id=p_tenant AND c.store_id=p_store AND c.owner_id=p_owner AND c.revoked_at IS NULL
   RETURNING c.id),
 events AS (
  INSERT INTO buyer.capability_events(tenant_id,store_id,owner_id,session_id,action)
  SELECT p_tenant,p_store,p_owner,r.id,'capability.revoked' FROM revoked r ON CONFLICT DO NOTHING RETURNING 1)
 SELECT count(*) INTO v_sessions FROM revoked;
 UPDATE buyer.owners o SET active=false WHERE o.tenant_id=p_tenant AND o.store_id=p_store AND o.id=p_owner AND o.active;
 -- Snapshots no order references (referenced ones live on in the order for legal retention, U08). Existing
 -- CHECKs: home needs non-blank city/line1, cvs needs the address columns blank.
 UPDATE storefront.destination_snapshots d SET recipient_name='[erased]',phone='000000',region='',
   city=CASE WHEN d.kind='home' THEN '[erased]' ELSE '' END,postal_code='',
   line1=CASE WHEN d.kind='home' THEN '[erased]' ELSE '' END,line2=''
  WHERE d.tenant_id=p_tenant AND d.store_id=p_store AND d.owner_id=p_owner
   AND NOT (d.recipient_name='[erased]' AND d.phone='000000')
   AND NOT EXISTS(SELECT 1 FROM checkout.orders o WHERE o.tenant_id=d.tenant_id AND o.store_id=d.store_id
     AND o.owner_id=d.owner_id AND o.destination_id=d.id);
 GET DIAGNOSTICS v_snapshots=ROW_COUNT;
 -- Manual labels are merchant-typed names; the full bundle id keeps them unique under claims_bundle_label.
 UPDATE claims.bundles b SET label='erased-'||replace(b.id::text,'-','')
  WHERE b.tenant_id=p_tenant AND b.store_id=p_store AND b.owner_id=p_owner AND b.label IS NOT NULL
   AND b.label<>'erased-'||replace(b.id::text,'-','');
 GET DIAGNOSTICS v_bundles=ROW_COUNT;
 RETURN jsonb_build_object('consents_withdrawn',v_consents,'sessions_revoked',v_sessions,
  'snapshots_redacted',v_snapshots,'bundles_relabelled',v_bundles);
END $$;
ALTER FUNCTION customers.apply_erasure(uuid,uuid,uuid) OWNER TO commerce_privacy_writer;
REVOKE ALL ON FUNCTION customers.apply_erasure(uuid,uuid,uuid) FROM PUBLIC;

-- ---------------------------------------------------------------------------------------
-- buyer_set_consent: only the buyer grants (CD4). The owner row is locked FOR UPDATE BEFORE resolve_scope: the
-- latter takes FOR SHARE on the same row, so locking after it would deadlock two concurrent requests of one buyer
-- (both hold SHARE and wait for the other's UPDATE). That holds only when this definer is the FIRST buyer-scope step of
-- its transaction: callers must NOT wrap it in buyer.WithScope (which resolves the capability, i.e. FOR SHARE, first).
-- internal/buyerhttp consentPut and privacyExport therefore use buyerTransaction (D11), as erasure does.
-- Every new key inserts a row (also when the state is unchanged) so a stale retry can never re-grant.
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION customers.buyer_set_consent(p_hash bytea,p_store uuid,p_purpose text,p_channel text,
 p_granted boolean,p_source text,p_policy text,p_key uuid)
RETURNS jsonb LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_sess record; v_scope record; v_prev customers.consent_events%ROWTYPE; v_row customers.consent_events%ROWTYPE;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR p_granted IS NULL OR p_key IS NULL
  OR p_purpose IS NULL OR p_channel IS NULL OR p_source IS NULL OR p_policy IS NULL
  OR NOT ((p_purpose='marketing_messages' AND p_channel='meta_dm') OR (p_purpose='ads_personalization' AND p_channel='meta_ads'))
  OR p_source NOT IN ('buyer_checkout','buyer_settings') OR p_policy !~ '^[a-z0-9][a-z0-9._-]{0,39}$' THEN
  RAISE EXCEPTION 'invalid consent request' USING ERRCODE='PT400'; END IF;
 SELECT c.tenant_id,c.owner_id INTO v_sess FROM buyer.capability_sessions c WHERE c.token_hash=p_hash AND c.store_id=p_store;
 IF NOT FOUND THEN RAISE EXCEPTION 'invalid buyer capability' USING ERRCODE='PT401'; END IF;
 PERFORM set_config('app.tenant_id',v_sess.tenant_id::text,true),set_config('app.store_id',p_store::text,true);
 PERFORM 1 FROM buyer.owners o WHERE o.tenant_id=v_sess.tenant_id AND o.store_id=p_store AND o.id=v_sess.owner_id FOR UPDATE;
 SELECT * INTO v_scope FROM buyer.resolve_scope(p_hash,p_store);
 IF NOT FOUND OR v_scope.owner_id IS DISTINCT FROM v_sess.owner_id THEN
  RAISE EXCEPTION 'invalid buyer capability' USING ERRCODE='PT401'; END IF;
 PERFORM set_config('app.buyer_id',v_scope.owner_id::text,true),set_config('app.buyer_session_id',v_scope.session_id::text,true),
  set_config('app.principal_id','',true);
 SELECT e.* INTO v_prev FROM customers.consent_events e WHERE e.tenant_id=v_scope.tenant_id AND e.store_id=p_store
  AND e.owner_id=v_scope.owner_id AND e.request_key=p_key AND e.source<>'erasure';
 IF FOUND THEN
  IF (v_prev.purpose,v_prev.channel,v_prev.granted,v_prev.source,v_prev.policy_version)
   IS DISTINCT FROM (p_purpose,p_channel,p_granted,p_source,p_policy) THEN
   RAISE EXCEPTION 'idempotency_conflict' USING ERRCODE='PT409'; END IF;
  v_row:=v_prev;
 ELSE
  INSERT INTO customers.consent_events(tenant_id,store_id,owner_id,purpose,channel,granted,source,policy_version,request_key)
  VALUES(v_scope.tenant_id,p_store,v_scope.owner_id,p_purpose,p_channel,p_granted,p_source,p_policy,p_key)
  RETURNING * INTO v_row;
 END IF;
 RETURN jsonb_build_object('purpose',v_row.purpose,'channel',v_row.channel,'granted',v_row.granted,
  'occurred_at',to_char(v_row.occurred_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'));
END $$;
ALTER FUNCTION customers.buyer_set_consent(bytea,uuid,text,text,boolean,text,text,uuid) OWNER TO commerce_privacy_writer;
REVOKE ALL ON FUNCTION customers.buyer_set_consent(bytea,uuid,text,text,boolean,text,text,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION customers.buyer_set_consent(bytea,uuid,text,text,boolean,text,text,uuid) TO commerce_buyer_runtime;

-- ---------------------------------------------------------------------------------------
-- merchant_withdraw_consent: customers:privacy; always inserts granted=false merchant_recorded (no merchant grant
-- exists: CHECK + no function). Merchant definers VERIFY the GUCs WithScope set (0063 read pattern), never set them.
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION customers.merchant_withdraw_consent(p_hash bytea,p_store uuid,p_customer uuid,p_purpose text,
 p_channel text,p_key uuid)
RETURNS jsonb LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE s record; v_final record; v_prev customers.consent_events%ROWTYPE; v_row customers.consent_events%ROWTYPE;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR p_customer IS NULL OR p_key IS NULL
  OR p_purpose IS NULL OR p_channel IS NULL
  OR NOT ((p_purpose='marketing_messages' AND p_channel='meta_dm') OR (p_purpose='ads_personalization' AND p_channel='meta_ads')) THEN
  RAISE EXCEPTION 'invalid consent request' USING ERRCODE='PT400'; END IF;
 SELECT * INTO s FROM identity.resolve_access(p_hash,p_store,'customers:privacy');
 IF s.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF s.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF s.access_status IS DISTINCT FROM 'ok' THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 IF current_setting('app.tenant_id',true) IS DISTINCT FROM s.tenant_id::text
  OR current_setting('app.store_id',true) IS DISTINCT FROM p_store::text
  OR current_setting('app.principal_id',true) IS DISTINCT FROM s.principal_id::text THEN
  RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 PERFORM 1 FROM buyer.owners o WHERE o.tenant_id=s.tenant_id AND o.store_id=p_store AND o.id=p_customer FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'customer not found' USING ERRCODE='PT404'; END IF;
 SELECT e.* INTO v_prev FROM customers.consent_events e WHERE e.tenant_id=s.tenant_id AND e.store_id=p_store
  AND e.owner_id=p_customer AND e.request_key=p_key AND e.source<>'erasure';
 IF FOUND THEN
  IF (v_prev.purpose,v_prev.channel,v_prev.granted,v_prev.source) IS DISTINCT FROM (p_purpose,p_channel,false,'merchant_recorded') THEN
   RAISE EXCEPTION 'idempotency_conflict' USING ERRCODE='PT409'; END IF;
  v_row:=v_prev;
 ELSE
  INSERT INTO customers.consent_events(tenant_id,store_id,owner_id,purpose,channel,granted,source,policy_version,principal_id,request_key)
  VALUES(s.tenant_id,p_store,p_customer,p_purpose,p_channel,false,'merchant_recorded','merchant',s.principal_id,p_key)
  RETURNING * INTO v_row;
  INSERT INTO ops.audit_events(tenant_id,store_id,principal_id,action)
  VALUES(s.tenant_id,p_store,s.principal_id,'customers.consent_withdrawn');
 END IF;
 -- Final authority after every lock wait and write (no post-revocation success).
 SELECT * INTO v_final FROM identity.resolve_access(p_hash,p_store,'customers:privacy');
 IF v_final.access_status IS DISTINCT FROM 'ok' OR v_final.tenant_id IS DISTINCT FROM s.tenant_id
  OR v_final.principal_id IS DISTINCT FROM s.principal_id OR v_final.authz_revision IS DISTINCT FROM s.authz_revision THEN
  RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 RETURN jsonb_build_object('purpose',v_row.purpose,'channel',v_row.channel,'granted',v_row.granted,
  'occurred_at',to_char(v_row.occurred_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'));
END $$;
ALTER FUNCTION customers.merchant_withdraw_consent(bytea,uuid,uuid,text,text,uuid) OWNER TO commerce_privacy_writer;
REVOKE ALL ON FUNCTION customers.merchant_withdraw_consent(bytea,uuid,uuid,text,text,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION customers.merchant_withdraw_consent(bytea,uuid,uuid,text,text,uuid) TO commerce_runtime;

-- ---------------------------------------------------------------------------------------
-- record_export: called in the SAME transaction as the export reads (Go builds and size-checks the document first,
-- so an oversized export rolls back with no EXPORT row). Auth per p_via; buyer p_customer must be NULL or the scope owner.
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION customers.record_export(p_hash bytea,p_store uuid,p_customer uuid,p_via text,p_key uuid,p_summary jsonb)
RETURNS jsonb LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE s record; v_final record; v_sess record; v_scope record; v_tenant uuid; v_owner uuid; v_principal uuid;
 v_prev customers.privacy_actions%ROWTYPE; v_row customers.privacy_actions%ROWTYPE;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR p_key IS NULL OR p_via IS NULL
  OR p_via NOT IN ('buyer','merchant') OR p_summary IS NULL OR jsonb_typeof(p_summary)<>'object'
  OR octet_length(p_summary::text)>1024 OR (p_via='merchant' AND p_customer IS NULL) THEN
  RAISE EXCEPTION 'invalid export request' USING ERRCODE='PT400'; END IF;
 IF p_via='merchant' THEN
  SELECT * INTO s FROM identity.resolve_access(p_hash,p_store,'customers:privacy');
  IF s.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
  IF s.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
  IF s.access_status IS DISTINCT FROM 'ok' THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
  IF current_setting('app.tenant_id',true) IS DISTINCT FROM s.tenant_id::text
   OR current_setting('app.store_id',true) IS DISTINCT FROM p_store::text
   OR current_setting('app.principal_id',true) IS DISTINCT FROM s.principal_id::text THEN
   RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
  v_tenant:=s.tenant_id; v_owner:=p_customer; v_principal:=s.principal_id;
  PERFORM 1 FROM buyer.owners o WHERE o.tenant_id=v_tenant AND o.store_id=p_store AND o.id=v_owner FOR UPDATE;
  IF NOT FOUND THEN RAISE EXCEPTION 'customer not found' USING ERRCODE='PT404'; END IF;
 ELSE
  SELECT c.tenant_id,c.owner_id INTO v_sess FROM buyer.capability_sessions c WHERE c.token_hash=p_hash AND c.store_id=p_store;
  IF NOT FOUND THEN RAISE EXCEPTION 'invalid buyer capability' USING ERRCODE='PT401'; END IF;
  PERFORM set_config('app.tenant_id',v_sess.tenant_id::text,true),set_config('app.store_id',p_store::text,true);
  PERFORM 1 FROM buyer.owners o WHERE o.tenant_id=v_sess.tenant_id AND o.store_id=p_store AND o.id=v_sess.owner_id FOR UPDATE;
  SELECT * INTO v_scope FROM buyer.resolve_scope(p_hash,p_store);
  IF NOT FOUND OR v_scope.owner_id IS DISTINCT FROM v_sess.owner_id OR (p_customer IS NOT NULL AND p_customer<>v_scope.owner_id) THEN
   RAISE EXCEPTION 'invalid buyer capability' USING ERRCODE='PT401'; END IF;
  PERFORM set_config('app.buyer_id',v_scope.owner_id::text,true),set_config('app.buyer_session_id',v_scope.session_id::text,true),
   set_config('app.principal_id','',true);
  v_tenant:=v_scope.tenant_id; v_owner:=v_scope.owner_id; v_principal:=NULL;
 END IF;
 SELECT a.* INTO v_prev FROM customers.privacy_actions a WHERE a.tenant_id=v_tenant AND a.store_id=p_store
  AND a.owner_id=v_owner AND a.request_key=p_key;
 IF FOUND THEN
  IF v_prev.kind<>'EXPORT' THEN RAISE EXCEPTION 'idempotency_conflict' USING ERRCODE='PT409'; END IF;
  v_row:=v_prev;
 ELSE
  INSERT INTO customers.privacy_actions(tenant_id,store_id,owner_id,kind,via,principal_id,request_key,summary)
  VALUES(v_tenant,p_store,v_owner,'EXPORT',p_via,v_principal,p_key,p_summary) RETURNING * INTO v_row;
  IF p_via='merchant' THEN
   INSERT INTO ops.audit_events(tenant_id,store_id,principal_id,action) VALUES(v_tenant,p_store,v_principal,'customers.exported');
  END IF;
 END IF;
 IF p_via='merchant' THEN
  SELECT * INTO v_final FROM identity.resolve_access(p_hash,p_store,'customers:privacy');
  IF v_final.access_status IS DISTINCT FROM 'ok' OR v_final.tenant_id IS DISTINCT FROM s.tenant_id
   OR v_final.principal_id IS DISTINCT FROM s.principal_id OR v_final.authz_revision IS DISTINCT FROM s.authz_revision THEN
   RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 END IF;
 RETURN jsonb_build_object('id',v_row.id,'kind',v_row.kind,'via',v_row.via,'summary',v_row.summary,
  'completed_at',to_char(v_row.completed_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'));
END $$;
ALTER FUNCTION customers.record_export(bytea,uuid,uuid,text,uuid,jsonb) OWNER TO commerce_privacy_writer;
REVOKE ALL ON FUNCTION customers.record_export(bytea,uuid,uuid,text,uuid,jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION customers.record_export(bytea,uuid,uuid,text,uuid,jsonb) TO commerce_runtime, commerce_buyer_runtime;

-- ---------------------------------------------------------------------------------------
-- erase_owner (CD7). Buyer via: a revoked session whose owner has an ERASURE row is a retry after success -> PT410
-- (found BEFORE any scope exists, so the buyer path never uses buyer.WithScope, D11). Otherwise lock the owner row,
-- apply the key rule, refuse while money or a hold is in flight (PT409 erasure_blocked), apply, tombstone, audit.
-- A merchant repeat with another key returns the stored summary (erasure is one-way per owner, privacy_one_erasure).
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION customers.erase_owner(p_hash bytea,p_store uuid,p_customer uuid,p_via text,p_key uuid)
RETURNS jsonb LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE s record; v_final record; v_sess record; v_scope record; v_tenant uuid; v_owner uuid; v_principal uuid;
 v_prev customers.privacy_actions%ROWTYPE; v_summary jsonb; v_revoked timestamptz;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR p_key IS NULL OR p_via IS NULL
  OR p_via NOT IN ('buyer','merchant') OR (p_via='merchant' AND p_customer IS NULL) THEN
  RAISE EXCEPTION 'invalid erasure request' USING ERRCODE='PT400'; END IF;
 IF p_via='merchant' THEN
  SELECT * INTO s FROM identity.resolve_access(p_hash,p_store,'customers:privacy');
  IF s.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
  IF s.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
  IF s.access_status IS DISTINCT FROM 'ok' THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
  IF current_setting('app.tenant_id',true) IS DISTINCT FROM s.tenant_id::text
   OR current_setting('app.store_id',true) IS DISTINCT FROM p_store::text
   OR current_setting('app.principal_id',true) IS DISTINCT FROM s.principal_id::text THEN
   RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
  v_tenant:=s.tenant_id; v_owner:=p_customer; v_principal:=s.principal_id;
  PERFORM 1 FROM buyer.owners o WHERE o.tenant_id=v_tenant AND o.store_id=p_store AND o.id=v_owner FOR UPDATE;
  IF NOT FOUND THEN RAISE EXCEPTION 'customer not found' USING ERRCODE='PT404'; END IF;
 ELSE
  SELECT c.tenant_id,c.owner_id INTO v_sess FROM buyer.capability_sessions c
   WHERE c.token_hash=p_hash AND c.store_id=p_store;
  IF NOT FOUND THEN RAISE EXCEPTION 'invalid buyer capability' USING ERRCODE='PT401'; END IF;
  PERFORM set_config('app.tenant_id',v_sess.tenant_id::text,true),set_config('app.store_id',p_store::text,true);
  PERFORM 1 FROM buyer.owners o WHERE o.tenant_id=v_sess.tenant_id AND o.store_id=p_store AND o.id=v_sess.owner_id FOR UPDATE;
  -- Re-read the revocation AFTER the owner lock: a concurrent erasure of this buyer has committed by now.
  SELECT c.revoked_at INTO v_revoked FROM buyer.capability_sessions c WHERE c.token_hash=p_hash AND c.store_id=p_store;
  IF v_revoked IS NOT NULL THEN
   IF EXISTS(SELECT 1 FROM customers.privacy_actions a WHERE a.tenant_id=v_sess.tenant_id AND a.store_id=p_store
     AND a.owner_id=v_sess.owner_id AND a.kind='ERASURE') THEN
    RAISE EXCEPTION 'erased' USING ERRCODE='PT410'; END IF;
   RAISE EXCEPTION 'invalid buyer capability' USING ERRCODE='PT401';
  END IF;
  SELECT * INTO v_scope FROM buyer.resolve_scope(p_hash,p_store);
  IF NOT FOUND OR v_scope.owner_id IS DISTINCT FROM v_sess.owner_id OR (p_customer IS NOT NULL AND p_customer<>v_scope.owner_id) THEN
   RAISE EXCEPTION 'invalid buyer capability' USING ERRCODE='PT401'; END IF;
  PERFORM set_config('app.buyer_id',v_scope.owner_id::text,true),set_config('app.buyer_session_id',v_scope.session_id::text,true),
   set_config('app.principal_id','',true);
  v_tenant:=v_scope.tenant_id; v_owner:=v_scope.owner_id; v_principal:=NULL;
 END IF;
 -- Key rule, then one-way per owner: any earlier ERASURE answers with its stored summary.
 SELECT a.* INTO v_prev FROM customers.privacy_actions a WHERE a.tenant_id=v_tenant AND a.store_id=p_store
  AND a.owner_id=v_owner AND a.request_key=p_key;
 IF FOUND AND v_prev.kind<>'ERASURE' THEN RAISE EXCEPTION 'idempotency_conflict' USING ERRCODE='PT409'; END IF;
 IF NOT FOUND THEN
  SELECT a.* INTO v_prev FROM customers.privacy_actions a WHERE a.tenant_id=v_tenant AND a.store_id=p_store
   AND a.owner_id=v_owner AND a.kind='ERASURE';
 END IF;
 IF FOUND THEN
  v_summary:=v_prev.summary;
 ELSE
  -- CD7 refusal: an unexpired hold, any Stripe session still alive (40 min vs the 15 min order hold, 0061/0013
  -- CHECKs), or a refund without a terminal fact would break an in-flight payment (G05, I24 late capture).
  IF EXISTS(SELECT 1 FROM checkout.orders o WHERE o.tenant_id=v_tenant AND o.store_id=p_store AND o.owner_id=v_owner
    AND o.commercial_state IN ('DRAFT','AWAITING_PAYMENT') AND o.expires_at>clock_timestamp())
   OR EXISTS(SELECT 1 FROM payments.stripe_sessions x WHERE x.tenant_id=v_tenant AND x.store_id=p_store
    AND x.owner_id=v_owner AND x.expires_at>clock_timestamp())
   OR EXISTS(SELECT 1 FROM payments.stripe_refunds r WHERE r.tenant_id=v_tenant AND r.store_id=p_store AND r.owner_id=v_owner
    AND NOT EXISTS(SELECT 1 FROM payments.refund_facts f WHERE f.tenant_id=r.tenant_id AND f.store_id=r.store_id
     AND f.refund_id=r.id)) THEN
   RAISE EXCEPTION 'erasure_blocked' USING ERRCODE='PT409'; END IF;
  v_summary:=customers.apply_erasure(v_tenant,p_store,v_owner);
  INSERT INTO customers.privacy_actions(tenant_id,store_id,owner_id,kind,via,principal_id,request_key,summary)
  VALUES(v_tenant,p_store,v_owner,'ERASURE',p_via,v_principal,p_key,v_summary);
  IF p_via='merchant' THEN
   INSERT INTO ops.audit_events(tenant_id,store_id,principal_id,action) VALUES(v_tenant,p_store,v_principal,'customers.erased');
  END IF;
 END IF;
 IF p_via='merchant' THEN
  SELECT * INTO v_final FROM identity.resolve_access(p_hash,p_store,'customers:privacy');
  IF v_final.access_status IS DISTINCT FROM 'ok' OR v_final.tenant_id IS DISTINCT FROM s.tenant_id
   OR v_final.principal_id IS DISTINCT FROM s.principal_id OR v_final.authz_revision IS DISTINCT FROM s.authz_revision THEN
   RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 END IF;
 RETURN v_summary;
END $$;
ALTER FUNCTION customers.erase_owner(bytea,uuid,uuid,text,uuid) OWNER TO commerce_privacy_writer;
REVOKE ALL ON FUNCTION customers.erase_owner(bytea,uuid,uuid,text,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION customers.erase_owner(bytea,uuid,uuid,text,uuid) TO commerce_runtime, commerce_buyer_runtime;

-- ---------------------------------------------------------------------------------------
-- replay_erasures (CD8/§9): restore-time re-application; EXECUTE nobody (migration owner / ops only). Without ids it
-- replays the ERASURE rows present in this database; with ids it is the restore procedure (owner ids come from the
-- external tombstone list) and inserts the missing tombstone (via restore, request_key = owner id).
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION customers.replay_erasures(p_owners uuid[] DEFAULT NULL) RETURNS integer
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE r record; v_count integer:=0; v_summary jsonb;
BEGIN
 FOR r IN
  SELECT o.tenant_id,o.store_id,o.id AS owner_id FROM buyer.owners o WHERE p_owners IS NOT NULL AND o.id=ANY(p_owners)
  UNION
  SELECT a.tenant_id,a.store_id,a.owner_id FROM customers.privacy_actions a WHERE p_owners IS NULL AND a.kind='ERASURE'
  ORDER BY 3
 LOOP
  PERFORM set_config('app.tenant_id',r.tenant_id::text,true),set_config('app.store_id',r.store_id::text,true);
  PERFORM 1 FROM buyer.owners o WHERE o.tenant_id=r.tenant_id AND o.store_id=r.store_id AND o.id=r.owner_id FOR UPDATE;
  v_summary:=customers.apply_erasure(r.tenant_id,r.store_id,r.owner_id);
  INSERT INTO customers.privacy_actions(tenant_id,store_id,owner_id,kind,via,principal_id,request_key,summary)
  VALUES(r.tenant_id,r.store_id,r.owner_id,'ERASURE','restore',NULL,r.owner_id,v_summary) ON CONFLICT DO NOTHING;
  v_count:=v_count+1;
 END LOOP;
 RETURN v_count;
END $$;
ALTER FUNCTION customers.replay_erasures(uuid[]) OWNER TO commerce_privacy_writer;
REVOKE ALL ON FUNCTION customers.replay_erasures(uuid[]) FROM PUBLIC;

-- ---------------------------------------------------------------------------------------
-- Buyer reads (no login role reads customers.* directly): privacy state, and the export sections.
-- p_detail=false: consents + erased flag (GET /v1/buyer/privacy). p_detail=true: also consent history, bound-bundle
-- summaries (platform + time, never actor_key) and privacy actions for the export document.
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION customers.buyer_read_privacy(p_hash bytea,p_store uuid,p_detail boolean)
RETURNS jsonb LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_scope record; v_sess record; v_result jsonb;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR p_detail IS NULL THEN
  RAISE EXCEPTION 'invalid privacy read' USING ERRCODE='PT400'; END IF;
 IF p_detail THEN
  -- Export: the first definer of the export transaction, followed by record_export (owner FOR UPDATE). Same prologue as
  -- buyer_set_consent, so resolve_scope's FOR SHARE below re-locks a row this transaction already holds exclusively
  -- and two concurrent exports of one buyer serialize instead of deadlocking on a SHARE->UPDATE upgrade.
  SELECT c.tenant_id,c.owner_id INTO v_sess FROM buyer.capability_sessions c WHERE c.token_hash=p_hash AND c.store_id=p_store;
  IF NOT FOUND THEN RAISE EXCEPTION 'invalid buyer capability' USING ERRCODE='PT401'; END IF;
  PERFORM set_config('app.tenant_id',v_sess.tenant_id::text,true),set_config('app.store_id',p_store::text,true);
  PERFORM 1 FROM buyer.owners o WHERE o.tenant_id=v_sess.tenant_id AND o.store_id=p_store AND o.id=v_sess.owner_id FOR UPDATE;
 END IF;
 SELECT * INTO v_scope FROM buyer.resolve_scope(p_hash,p_store);
 IF NOT FOUND THEN RAISE EXCEPTION 'invalid buyer capability' USING ERRCODE='PT401'; END IF;
 PERFORM set_config('app.tenant_id',v_scope.tenant_id::text,true),set_config('app.store_id',p_store::text,true),
  set_config('app.buyer_id',v_scope.owner_id::text,true),set_config('app.buyer_session_id',v_scope.session_id::text,true),
  set_config('app.principal_id','',true);
 SELECT jsonb_build_object(
  'store_name',(SELECT st.name FROM control.stores st WHERE st.tenant_id=v_scope.tenant_id AND st.id=p_store),
  'consents',jsonb_build_object(
   'marketing_messages',coalesce((SELECT e.granted FROM customers.consent_events e WHERE e.tenant_id=v_scope.tenant_id
     AND e.store_id=p_store AND e.owner_id=v_scope.owner_id AND e.purpose='marketing_messages' AND e.channel='meta_dm'
     ORDER BY e.occurred_at DESC,e.id DESC LIMIT 1),false),
   'ads_personalization',coalesce((SELECT e.granted FROM customers.consent_events e WHERE e.tenant_id=v_scope.tenant_id
     AND e.store_id=p_store AND e.owner_id=v_scope.owner_id AND e.purpose='ads_personalization' AND e.channel='meta_ads'
     ORDER BY e.occurred_at DESC,e.id DESC LIMIT 1),false)),
  'erased',EXISTS(SELECT 1 FROM customers.privacy_actions a WHERE a.tenant_id=v_scope.tenant_id AND a.store_id=p_store
    AND a.owner_id=v_scope.owner_id AND a.kind='ERASURE'))
  ||CASE WHEN NOT p_detail THEN '{}'::jsonb ELSE jsonb_build_object(
  'consent_history',coalesce((SELECT jsonb_agg(jsonb_build_object('purpose',x.purpose,'channel',x.channel,'granted',x.granted,
     'source',x.source,'policy_version',x.policy_version,
     'occurred_at',to_char(x.occurred_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"')) ORDER BY x.occurred_at DESC,x.id DESC)
     FROM (SELECT e.* FROM customers.consent_events e WHERE e.tenant_id=v_scope.tenant_id AND e.store_id=p_store
       AND e.owner_id=v_scope.owner_id ORDER BY e.occurred_at DESC,e.id DESC LIMIT 500) x),'[]'::jsonb),
  'claims',coalesce((SELECT jsonb_agg(jsonb_build_object('session_id',b.session_id,'platform',b.platform,
     'bound_at',to_char(b.bound_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),'line_count',b.line_count)
     ORDER BY b.bound_at DESC,b.id DESC)
     FROM (SELECT c.id,c.session_id,c.platform,c.bound_at,c.line_count FROM claims.bundles c
       WHERE c.tenant_id=v_scope.tenant_id AND c.store_id=p_store
       AND c.owner_id=v_scope.owner_id ORDER BY c.bound_at DESC,c.id DESC LIMIT 100) b),'[]'::jsonb),
  'privacy_actions',coalesce((SELECT jsonb_agg(jsonb_build_object('kind',y.kind,'via',y.via,
     'completed_at',to_char(y.completed_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),'summary',y.summary)
     ORDER BY y.completed_at DESC,y.id DESC)
     FROM (SELECT a.* FROM customers.privacy_actions a WHERE a.tenant_id=v_scope.tenant_id AND a.store_id=p_store
       AND a.owner_id=v_scope.owner_id ORDER BY a.completed_at DESC,a.id DESC LIMIT 100) y),'[]'::jsonb)) END
  INTO v_result;
 RETURN v_result;
END $$;
ALTER FUNCTION customers.buyer_read_privacy(bytea,uuid,boolean) OWNER TO commerce_privacy_writer;
REVOKE ALL ON FUNCTION customers.buyer_read_privacy(bytea,uuid,boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION customers.buyer_read_privacy(bytea,uuid,boolean) TO commerce_buyer_runtime;

-- Buyer export orders: commerce_buyer_runtime has no checkout table access and the existing buyer order detail lives on
-- the checkout pool, so this definer (owner commerce_checkout_writer, which already reads checkout.orders and the
-- shipment heads) returns the caller's own orders: newest first, at most 201 (Go refuses > 200), snapshot without
-- the merchant-internal allocation, and the SHIPPED head only (manual-fulfilment-v1 §5.2, no note/void_reason/principal).
CREATE FUNCTION customers.buyer_export_orders(p_hash bytea,p_store uuid)
RETURNS jsonb LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_scope record; v_result jsonb;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL THEN
  RAISE EXCEPTION 'invalid export read' USING ERRCODE='PT400'; END IF;
 SELECT * INTO v_scope FROM buyer.resolve_scope(p_hash,p_store);
 IF NOT FOUND THEN RAISE EXCEPTION 'invalid buyer capability' USING ERRCODE='PT401'; END IF;
 PERFORM set_config('app.tenant_id',v_scope.tenant_id::text,true),set_config('app.store_id',p_store::text,true),
  set_config('app.buyer_id',v_scope.owner_id::text,true),set_config('app.buyer_session_id',v_scope.session_id::text,true),
  set_config('app.principal_id','',true);
 SELECT coalesce(jsonb_agg(jsonb_build_object(
   'order_id',o.id,'created_at',to_char(o.created_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'commercial_state',o.commercial_state,'fulfillment_state',o.fulfillment_state,
   'currency',o.currency,'total_minor',o.total_minor,'snapshot',o.snapshot-'allocation',
   'shipment',(SELECT jsonb_build_object('status',v.status,'carrier_code',v.carrier_code,'carrier_name',v.carrier_name,
      'tracking_number',v.tracking_number,'tracking_url',v.tracking_url,
      'recorded_at',to_char(v.recorded_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'))
     FROM fulfillment.manual_shipment_heads h JOIN fulfillment.manual_shipment_versions v
      ON v.tenant_id=h.tenant_id AND v.store_id=h.store_id AND v.order_id=h.order_id AND v.version=h.current_version
     WHERE h.tenant_id=o.tenant_id AND h.store_id=o.store_id AND h.owner_id=o.owner_id AND h.order_id=o.id AND v.status='SHIPPED'))
   ORDER BY o.created_at DESC,o.id DESC),'[]'::jsonb) INTO v_result
  FROM (SELECT x.* FROM checkout.orders x WHERE x.tenant_id=v_scope.tenant_id AND x.store_id=p_store
    AND x.owner_id=v_scope.owner_id ORDER BY x.created_at DESC,x.id DESC LIMIT 201) o;
 RETURN v_result;
END $$;
ALTER FUNCTION customers.buyer_export_orders(bytea,uuid) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION customers.buyer_export_orders(bytea,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION customers.buyer_export_orders(bytea,uuid) TO commerce_buyer_runtime;

-- ---------------------------------------------------------------------------------------
-- identity.read_merchant_customers (customers:read): list (limit 1..101, keyset on (last_activity_at,id) DESC) or
-- detail (p_customer, limit 1). One statement snapshot, the 0063 read pattern: resolve_access, GUC check (never
-- set_config), fresh final fence via merchant_access_denied. Owners with >= 1 order or >= 1 bound bundle only.
-- CD3/C-1: customer_id = owner id; no actor_key, session id or PSP ref is ever selected. D7 row derivation.
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION identity.read_merchant_customers(p_hash bytea,p_store uuid,p_customer uuid,p_limit integer,
 p_after_ts timestamptz,p_after_id uuid,p_q text)
RETURNS jsonb LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE s record; v_result jsonb; v_auth_error text; v_digits text; v_phone boolean:=false;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL
  OR p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 101
  OR (p_after_ts IS NULL)<>(p_after_id IS NULL)
  OR (p_after_ts IS NOT NULL AND NOT isfinite(p_after_ts))
  OR (p_customer IS NOT NULL AND (p_limit<>1 OR p_after_id IS NOT NULL OR p_q IS NOT NULL))
  OR (p_q IS NOT NULL AND (char_length(p_q) NOT BETWEEN 1 AND 40 OR p_q<>btrim(p_q) OR p_q ~ '[[:cntrl:]]'))
  OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid customer read' USING ERRCODE='PT400'; END IF;
 SELECT * INTO s FROM identity.resolve_access(p_hash,p_store,'customers:read');
 IF s.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF s.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF s.access_status IS DISTINCT FROM 'ok' THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 IF current_setting('app.tenant_id',true) IS DISTINCT FROM s.tenant_id::text
  OR current_setting('app.store_id',true) IS DISTINCT FROM p_store::text
  OR current_setting('app.principal_id',true) IS DISTINCT FROM s.principal_id::text THEN
  RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 -- D6: digits (after removing spaces, '-' and '+') = phone-digits suffix; anything else = recipient_name prefix.
 IF p_q IS NOT NULL THEN
  v_digits:=regexp_replace(p_q,'[ +-]','','g');
  v_phone:=v_digits ~ '^[0-9]+$';
 END IF;

 -- ponytail: read-time aggregates (CD3); materialize when p95 > 300 ms at a measured store size (§9).
 -- The row set is driven from owners that HAVE activity (orders / bound bundles), never from every buyer.owners row:
 -- buyer.issue_capability creates an owner per anonymous visitor, so scanning owners made each page O(visitors)
 -- (lane-close review P2, CB03 10k idle owners). The per-customer aggregates below then run over customers only.
 WITH active_owner AS MATERIALIZED (
  SELECT o.owner_id AS id FROM checkout.orders o WHERE o.tenant_id=s.tenant_id AND o.store_id=p_store
   AND (p_customer IS NULL OR o.owner_id=p_customer)
  UNION
  SELECT b.owner_id FROM claims.bundles b WHERE b.tenant_id=s.tenant_id AND b.store_id=p_store AND b.owner_id IS NOT NULL
   AND (p_customer IS NULL OR b.owner_id=p_customer)
 ), base AS MATERIALIZED (
  SELECT ow.tenant_id,ow.store_id,ow.id,ow.created_at AS first_seen,ow.active,
   greatest(
    (SELECT max(o.created_at) FROM checkout.orders o WHERE o.tenant_id=ow.tenant_id AND o.store_id=ow.store_id AND o.owner_id=ow.id),
    (SELECT max(b.bound_at) FROM claims.bundles b WHERE b.tenant_id=ow.tenant_id AND b.store_id=ow.store_id AND b.owner_id=ow.id)
   ) AS last_activity
  FROM active_owner ao
  -- LATERAL ... LIMIT 1 forces one primary-key probe per customer: without it the planner (no statistics right after a
  -- bulk insert of visitors) chose a hash join that read the whole owners table again.
  CROSS JOIN LATERAL (SELECT w.tenant_id,w.store_id,w.id,w.created_at,w.active FROM buyer.owners w
    WHERE w.id=ao.id AND w.tenant_id=s.tenant_id AND w.store_id=p_store LIMIT 1) ow
  WHERE (p_customer IS NULL OR ow.id=p_customer)
   AND (p_q IS NULL OR EXISTS(SELECT 1 FROM checkout.orders qo WHERE qo.tenant_id=ow.tenant_id AND qo.store_id=ow.store_id
     AND qo.owner_id=ow.id AND CASE WHEN v_phone
      THEN right(regexp_replace(coalesce(qo.snapshot#>>'{destination,phone}',''),'[^0-9]','','g'),char_length(v_digits))=v_digits
      ELSE starts_with(lower(coalesce(qo.snapshot#>>'{destination,recipient_name}','')),lower(p_q)) END))
 ), paged AS MATERIALIZED (
  SELECT b.* FROM base b
  WHERE b.last_activity IS NOT NULL AND (p_after_id IS NULL OR (b.last_activity,b.id)<(p_after_ts,p_after_id))
  ORDER BY b.last_activity DESC,b.id DESC LIMIT p_limit
 ), projected AS (
  SELECT pg.last_activity,pg.id,
   jsonb_build_object(
    'customer_id',pg.id,
    'first_seen_at',to_char(pg.first_seen AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
    'last_activity_at',to_char(pg.last_activity AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
    'display_name',lo.name,
    'phone_last3',lo.last3,
    'orders_count',coalesce(ag.orders_count,0),'paid_orders_count',coalesce(ag.paid_orders_count,0),
    'captured_minor',coalesce(ag.captured_minor,0),'refunded_minor',coalesce(ag.refunded_minor,0),
    'currency',lo.currency,
    'claims_count',coalesce(cl.claims_count,0),'platforms',coalesce(cl.platforms,'[]'::jsonb),
    'consents',jsonb_build_object(
     'marketing_messages',coalesce((SELECT e.granted FROM customers.consent_events e WHERE e.tenant_id=pg.tenant_id
       AND e.store_id=pg.store_id AND e.owner_id=pg.id AND e.purpose='marketing_messages' AND e.channel='meta_dm'
       ORDER BY e.occurred_at DESC,e.id DESC LIMIT 1),false),
     'ads_personalization',coalesce((SELECT e.granted FROM customers.consent_events e WHERE e.tenant_id=pg.tenant_id
       AND e.store_id=pg.store_id AND e.owner_id=pg.id AND e.purpose='ads_personalization' AND e.channel='meta_ads'
       ORDER BY e.occurred_at DESC,e.id DESC LIMIT 1),false)),
    'active',pg.active)
   ||CASE WHEN p_customer IS NULL THEN '{}'::jsonb ELSE jsonb_build_object(
    'order_ids',coalesce((SELECT jsonb_agg(x.id ORDER BY x.created_at DESC,x.id DESC)
       FROM (SELECT o.id,o.created_at FROM checkout.orders o WHERE o.tenant_id=pg.tenant_id AND o.store_id=pg.store_id
         AND o.owner_id=pg.id ORDER BY o.created_at DESC,o.id DESC LIMIT 201) x),'[]'::jsonb),
    'claims',coalesce((SELECT jsonb_agg(jsonb_build_object('session_id',b.session_id,'platform',b.platform,
       'bound_at',to_char(b.bound_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),'line_count',b.line_count)
       ORDER BY b.bound_at DESC,b.id DESC)
       FROM (SELECT c.id,c.session_id,c.platform,c.bound_at,c.line_count FROM claims.bundles c
         WHERE c.tenant_id=pg.tenant_id AND c.store_id=pg.store_id
         AND c.owner_id=pg.id ORDER BY c.bound_at DESC,c.id DESC LIMIT 100) b),'[]'::jsonb),
    'consent_history',coalesce((SELECT jsonb_agg(jsonb_build_object('purpose',x.purpose,'channel',x.channel,
       'granted',x.granted,'source',x.source,'policy_version',x.policy_version,
       'occurred_at',to_char(x.occurred_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'))
       ORDER BY x.occurred_at DESC,x.id DESC)
       FROM (SELECT e.* FROM customers.consent_events e WHERE e.tenant_id=pg.tenant_id AND e.store_id=pg.store_id
         AND e.owner_id=pg.id ORDER BY e.occurred_at DESC,e.id DESC LIMIT 500) x),'[]'::jsonb),
    'privacy_actions',coalesce((SELECT jsonb_agg(jsonb_build_object('kind',y.kind,'via',y.via,
       'completed_at',to_char(y.completed_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),'summary',y.summary)
       ORDER BY y.completed_at DESC,y.id DESC)
       FROM (SELECT a.* FROM customers.privacy_actions a WHERE a.tenant_id=pg.tenant_id AND a.store_id=pg.store_id
         AND a.owner_id=pg.id ORDER BY a.completed_at DESC,a.id DESC LIMIT 100) y),'[]'::jsonb)) END AS value
  FROM paged pg
  -- D7: display name, phone last 3 and currency come from the LATEST order's destination (null for bundle-only owners).
  LEFT JOIN LATERAL (
   SELECT o.currency,o.snapshot#>>'{destination,recipient_name}' AS name,
    right(regexp_replace(coalesce(o.snapshot#>>'{destination,phone}',''),'[^0-9]','','g'),3) AS last3
   FROM checkout.orders o WHERE o.tenant_id=pg.tenant_id AND o.store_id=pg.store_id AND o.owner_id=pg.id
   ORDER BY o.created_at DESC,o.id DESC LIMIT 1
  ) lo ON true
  -- I05: money sums cover only the latest order's currency (single-currency stores in v1). "Captured" uses the same
  -- fact-matches-attempt-matches-order predicate as identity.read_merchant_orders; refunded = held (no FAILED/CANCELED/
  -- REJECTED fact) with a SUCCEEDED fact, the stripe-refund-v1 §7.1 rule.
  LEFT JOIN LATERAL (
   SELECT count(*) AS orders_count,count(*) FILTER (WHERE x.captured) AS paid_orders_count,
    coalesce(sum(x.total_minor) FILTER (WHERE x.captured AND x.currency=lo.currency),0)::bigint AS captured_minor,
    coalesce(sum(x.refunded) FILTER (WHERE x.captured AND x.currency=lo.currency),0)::bigint AS refunded_minor
   FROM (
    SELECT o.total_minor,o.currency,
     EXISTS(SELECT 1 FROM checkout.payment_attempts a JOIN payments.facts f ON f.tenant_id=a.tenant_id AND f.store_id=a.store_id
       AND f.attempt_id=a.id AND f.kind='CAPTURED' AND f.connection_id=a.connection_id AND f.execution_profile=a.execution_profile
       AND f.environment=a.environment AND f.currency=a.currency AND f.amount_minor=a.amount_minor
       AND f.currency=o.currency AND f.amount_minor=o.total_minor
      WHERE a.tenant_id=o.tenant_id AND a.store_id=o.store_id AND a.owner_id=o.owner_id AND a.order_id=o.id) AS captured,
     coalesce((SELECT sum(r.amount_minor) FROM checkout.payment_attempts a JOIN payments.stripe_refunds r
        ON r.tenant_id=a.tenant_id AND r.store_id=a.store_id AND r.attempt_id=a.id
       WHERE a.tenant_id=o.tenant_id AND a.store_id=o.store_id AND a.owner_id=o.owner_id AND a.order_id=o.id
        AND NOT EXISTS(SELECT 1 FROM payments.refund_facts rf WHERE rf.tenant_id=r.tenant_id AND rf.store_id=r.store_id
          AND rf.refund_id=r.id AND rf.kind IN ('FAILED','CANCELED','REJECTED'))
        AND EXISTS(SELECT 1 FROM payments.refund_facts rs WHERE rs.tenant_id=r.tenant_id AND rs.store_id=r.store_id
          AND rs.refund_id=r.id AND rs.kind='SUCCEEDED')),0) AS refunded
    FROM checkout.orders o WHERE o.tenant_id=pg.tenant_id AND o.store_id=pg.store_id AND o.owner_id=pg.id
   ) x
  ) ag ON true
  LEFT JOIN LATERAL (
   SELECT count(*) AS claims_count,coalesce(jsonb_agg(DISTINCT c.platform ORDER BY c.platform),'[]'::jsonb) AS platforms
   FROM claims.bundles c WHERE c.tenant_id=pg.tenant_id AND c.store_id=pg.store_id AND c.owner_id=pg.id
  ) cl ON true
 )
 SELECT coalesce(jsonb_agg(value ORDER BY last_activity DESC,id DESC),'[]'::jsonb) INTO v_result FROM projected;

 -- Fresh final fence AFTER the data reads, BEFORE the empty/not-found branches (no post-revocation existence oracle).
 v_auth_error:=identity.merchant_access_denied(p_hash,p_store,ARRAY['customers:read'],s.tenant_id,s.principal_id,s.authz_revision);
 IF v_auth_error IS NOT NULL THEN RAISE EXCEPTION 'customer read access denied' USING ERRCODE=v_auth_error; END IF;
 IF p_customer IS NOT NULL AND jsonb_array_length(v_result)=0 THEN
  RAISE EXCEPTION 'customer not found' USING ERRCODE='PT404'; END IF;
 IF octet_length(v_result::text)>(CASE WHEN p_customer IS NULL THEN 262144 ELSE 524288 END) THEN
  RAISE EXCEPTION 'customer read unavailable' USING ERRCODE='PT503'; END IF;
 RETURN v_result;
END $$;
ALTER FUNCTION identity.read_merchant_customers(bytea,uuid,uuid,integer,timestamptz,uuid,text) OWNER TO commerce_auth;
REVOKE ALL ON FUNCTION identity.read_merchant_customers(bytea,uuid,uuid,integer,timestamptz,uuid,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION identity.read_merchant_customers(bytea,uuid,uuid,integer,timestamptz,uuid,text) TO commerce_runtime;

-- ---------------------------------------------------------------------------------------
-- BD7 finance summary (orders:read): daily captured / refunded / net by currency and environment, days in Asia/Taipei
-- (Q11), <= 92 days. A refund counts on its SUCCEEDED day unless a later FAILED/CANCELED fact exists.
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION identity.read_finance_summary(p_hash bytea,p_store uuid,p_from date,p_to date)
RETURNS jsonb LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE s record; v_rows jsonb; v_auth_error text;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR p_from IS NULL OR p_to IS NULL
  OR NOT isfinite(p_from) OR NOT isfinite(p_to) OR p_to-p_from NOT BETWEEN 0 AND 91
  OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid finance read' USING ERRCODE='PT400'; END IF;
 SELECT * INTO s FROM identity.resolve_access(p_hash,p_store,'orders:read');
 IF s.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF s.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF s.access_status IS DISTINCT FROM 'ok' THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 IF current_setting('app.tenant_id',true) IS DISTINCT FROM s.tenant_id::text
  OR current_setting('app.store_id',true) IS DISTINCT FROM p_store::text
  OR current_setting('app.principal_id',true) IS DISTINCT FROM s.principal_id::text THEN
  RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 WITH lim AS (
  SELECT (p_from::timestamp AT TIME ZONE 'Asia/Taipei') AS t0,((p_to+1)::timestamp AT TIME ZONE 'Asia/Taipei') AS t1
 ), cap AS (
  -- I05: amounts are the CAPTURED fact's own minor units; no currency conversion or cross-currency sum.
  SELECT (f.received_at AT TIME ZONE 'Asia/Taipei')::date AS day,f.currency,f.environment,
   count(*)::bigint AS n,sum(f.amount_minor)::bigint AS minor
  FROM payments.facts f,lim
  WHERE f.tenant_id=s.tenant_id AND f.store_id=p_store AND f.kind='CAPTURED'
   AND f.received_at>=lim.t0 AND f.received_at<lim.t1
  GROUP BY 1,2,3
 ), ref AS (
  SELECT (rf.received_at AT TIME ZONE 'Asia/Taipei')::date AS day,r.currency,a.environment,sum(r.amount_minor)::bigint AS minor
  FROM payments.refund_facts rf CROSS JOIN lim
  JOIN payments.stripe_refunds r ON r.tenant_id=rf.tenant_id AND r.store_id=rf.store_id AND r.id=rf.refund_id
  JOIN checkout.payment_attempts a ON a.tenant_id=r.tenant_id AND a.store_id=r.store_id AND a.id=r.attempt_id
  WHERE rf.tenant_id=s.tenant_id AND rf.store_id=p_store AND rf.kind='SUCCEEDED'
   AND rf.received_at>=lim.t0 AND rf.received_at<lim.t1
   AND NOT EXISTS(SELECT 1 FROM payments.refund_facts later WHERE later.tenant_id=rf.tenant_id AND later.store_id=rf.store_id
     AND later.refund_id=rf.refund_id AND later.kind IN ('FAILED','CANCELED') AND later.received_at>rf.received_at)
  GROUP BY 1,2,3
 ), merged AS (
  SELECT coalesce(c.day,r.day) AS day,coalesce(c.currency,r.currency) AS currency,
   coalesce(c.environment,r.environment) AS environment,coalesce(c.n,0) AS n,coalesce(c.minor,0) AS cm,coalesce(r.minor,0) AS rm
  FROM cap c FULL JOIN ref r ON r.day=c.day AND r.currency=c.currency AND r.environment=c.environment
 )
 SELECT coalesce(jsonb_agg(jsonb_build_object('day',to_char(m.day,'YYYY-MM-DD'),'currency',m.currency,
   'environment',m.environment,'captured_count',m.n,'captured_minor',m.cm,'refunded_minor',m.rm,'net_minor',m.cm-m.rm)
   ORDER BY m.day,m.currency,m.environment),'[]'::jsonb) INTO v_rows FROM merged m;
 v_auth_error:=identity.merchant_access_denied(p_hash,p_store,ARRAY['orders:read'],s.tenant_id,s.principal_id,s.authz_revision);
 IF v_auth_error IS NOT NULL THEN RAISE EXCEPTION 'finance read access denied' USING ERRCODE=v_auth_error; END IF;
 IF octet_length(v_rows::text)>1048576 THEN RAISE EXCEPTION 'finance read unavailable' USING ERRCODE='PT503'; END IF;
 RETURN v_rows;
END $$;
ALTER FUNCTION identity.read_finance_summary(bytea,uuid,date,date) OWNER TO commerce_auth;
REVOKE ALL ON FUNCTION identity.read_finance_summary(bytea,uuid,date,date) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION identity.read_finance_summary(bytea,uuid,date,date) TO commerce_runtime;

-- Same rows, orders:export AND orders:read, one audit row finance.exported (0063 export pattern; the GUCs are
-- verified, not set: platform.WithScope owns them). The rows come from read_finance_summary so the two never diverge.
CREATE FUNCTION identity.export_finance_summary(p_hash bytea,p_store uuid,p_from date,p_to date)
RETURNS jsonb LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE s record; v_rows jsonb; v_auth_error text;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR p_from IS NULL OR p_to IS NULL
  OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid finance export' USING ERRCODE='PT400'; END IF;
 SELECT * INTO s FROM identity.resolve_access(p_hash,p_store,'orders:export');
 IF s.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF s.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF s.access_status IS DISTINCT FROM 'ok' THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 IF current_setting('app.tenant_id',true) IS DISTINCT FROM s.tenant_id::text
  OR current_setting('app.store_id',true) IS DISTINCT FROM p_store::text
  OR current_setting('app.principal_id',true) IS DISTINCT FROM s.principal_id::text THEN
  RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 -- read_finance_summary re-authorizes orders:read and validates the range; both definers share the owner commerce_auth.
 v_rows:=identity.read_finance_summary(p_hash,p_store,p_from,p_to);
 v_auth_error:=identity.merchant_access_denied(p_hash,p_store,ARRAY['orders:export','orders:read'],s.tenant_id,s.principal_id,s.authz_revision);
 IF v_auth_error IS NOT NULL THEN RAISE EXCEPTION 'finance export access denied' USING ERRCODE=v_auth_error; END IF;
 INSERT INTO ops.audit_events(tenant_id,store_id,principal_id,action) VALUES(s.tenant_id,p_store,s.principal_id,'finance.exported');
 RETURN v_rows;
END $$;
ALTER FUNCTION identity.export_finance_summary(bytea,uuid,date,date) OWNER TO commerce_auth;
REVOKE ALL ON FUNCTION identity.export_finance_summary(bytea,uuid,date,date) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION identity.export_finance_summary(bytea,uuid,date,date) TO commerce_runtime;

-- ---------------------------------------------------------------------------------------
-- Documentation (PROCESS §5): owning package, allowed roles, non-goals.
-- ---------------------------------------------------------------------------------------
COMMENT ON ROLE commerce_privacy_writer IS
 'internal/customers definer owner (migrations/0078). NOLOGIN; owns only customers.* privacy/consent definers. Column-level grants on buyer, storefront, checkout, claims, payments (C-6); never actor_key, claims.meta_intake, live.claim_sources or social.*. No runtime login may reach it (platform validatePoolAuthority object-owner rule).';
COMMENT ON SCHEMA customers IS
 'internal/customers: append-only consent and owner-level privacy actions (CD4/CD6-CD8). PUBLIC revoked; USAGE for name resolution only, no table grant to any login role. Non-goals: identity merge, marketing send, CAPI.';
COMMENT ON TABLE customers.consent_events IS
 'internal/customers: append-only consent history per (owner,purpose,channel); current state = latest event, absence = not granted (CD4). Written only by customers.buyer_set_consent, merchant_withdraw_consent and apply_erasure (owner commerce_privacy_writer); read by commerce_privacy_writer and commerce_auth (merchant projection). No UPDATE/DELETE for any role. Personal-data class: consent records.';
COMMENT ON TABLE customers.privacy_actions IS
 'internal/customers: append-only EXPORT/ERASURE log; the ERASURE row is the tombstone customers.replay_erasures re-applies (CD8). summary is counts only, no PII. Written by record_export, erase_owner, replay_erasures (owner commerce_privacy_writer); read by commerce_privacy_writer and commerce_auth. No UPDATE/DELETE for any role.';
COMMENT ON COLUMN customers.consent_events.tenant_id IS 'Owning tenant; RLS scope key. Set from the authenticated scope, never from the client.';
COMMENT ON COLUMN customers.consent_events.store_id IS 'Recipient of the consent (the store, CD4); RLS scope key. Never client-supplied.';
COMMENT ON COLUMN customers.consent_events.owner_id IS 'buyer.owners row the consent belongs to (customer = owner, CD1). Consent on an owner never authorizes an actor_key send (CD5).';
COMMENT ON COLUMN customers.consent_events.id IS 'Event id; ties break latest-event ordering with occurred_at.';
COMMENT ON COLUMN customers.consent_events.purpose IS 'Closed vocabulary: marketing_messages | ads_personalization (Q12).';
COMMENT ON COLUMN customers.consent_events.channel IS 'meta_dm for marketing_messages, meta_ads for ads_personalization (pair CHECK).';
COMMENT ON COLUMN customers.consent_events.granted IS 'true only from the buyer (CHECK); false from buyer, merchant or erasure.';
COMMENT ON COLUMN customers.consent_events.source IS 'Server-set from the request context: buyer_checkout | buyer_settings | merchant_recorded | erasure. Never taken from the request body.';
COMMENT ON COLUMN customers.consent_events.policy_version IS 'Deployed privacy notice version (customers.PrivacyPolicyVersion) at write time; merchant withdrawals use ''merchant'', erasure uses ''erasure''.';
COMMENT ON COLUMN customers.consent_events.principal_id IS 'Merchant principal of a merchant_recorded withdrawal (audit); NULL otherwise.';
COMMENT ON COLUMN customers.consent_events.request_key IS 'Caller Idempotency-Key mapped to a uuid by internal/customers; one non-erasure row per (owner,key), erasure rows use the owner id.';
COMMENT ON COLUMN customers.consent_events.occurred_at IS 'Server clock_timestamp() of the insert; never client-supplied.';
COMMENT ON COLUMN customers.privacy_actions.tenant_id IS 'Owning tenant; RLS scope key.';
COMMENT ON COLUMN customers.privacy_actions.store_id IS 'Owning store; RLS scope key.';
COMMENT ON COLUMN customers.privacy_actions.id IS 'Privacy action id.';
COMMENT ON COLUMN customers.privacy_actions.owner_id IS 'buyer.owners row the action was performed on.';
COMMENT ON COLUMN customers.privacy_actions.kind IS 'EXPORT | ERASURE; at most one ERASURE per owner (privacy_one_erasure).';
COMMENT ON COLUMN customers.privacy_actions.via IS 'buyer | merchant | restore (tombstone re-inserted by replay_erasures).';
COMMENT ON COLUMN customers.privacy_actions.principal_id IS 'Merchant principal for via=merchant (audit); NULL for buyer and restore.';
COMMENT ON COLUMN customers.privacy_actions.request_key IS 'Caller Idempotency-Key mapped to a uuid; restore tombstones use the owner id.';
COMMENT ON COLUMN customers.privacy_actions.summary IS 'Counts only (<= 1024 bytes, no PII): erasure counts, export section counts.';
COMMENT ON COLUMN customers.privacy_actions.completed_at IS 'Server clock_timestamp() of the insert.';
COMMENT ON INDEX customers.consent_one_key IS 'One buyer/merchant consent request = one row per (owner,key); erasure rows are exempt.';
COMMENT ON INDEX customers.consent_current IS 'Latest-event lookup per (owner,purpose,channel) for consent_allows and the projections.';
COMMENT ON INDEX customers.privacy_one_erasure IS 'Erasure is one-way per owner (CD8).';
COMMENT ON INDEX checkout.orders_by_owner IS 'customers list/detail aggregates and erasure refusal checks read orders per owner, newest first (0078).';
COMMENT ON INDEX claims.bundles_by_owner IS 'customers list claims_count/platforms and erasure relabel read bound bundles per owner (0078).';
COMMENT ON FUNCTION customers.guard_append_only() IS 'customers package trigger guard: consent_events and privacy_actions are append-only.';
COMMENT ON FUNCTION customers.consent_allows(uuid,uuid,uuid,text,text) IS
 'internal/customers owns THE consent gate (CD5): latest (occurred_at,id) event for (tenant,store,owner,purpose,channel) is granted AND buyer.owners.active. STABLE definer, owner commerce_privacy_writer, EXECUTE commerce_auth only (R3 grants its planners in 0080, C-7). Unknown owner, other store or invalid pair = false, never an error. Necessary, not sufficient, for any actor_key send/audience (CD5).';
COMMENT ON FUNCTION customers.apply_erasure(uuid,uuid,uuid) IS
 'internal/customers CD7 steps, idempotent; internal (EXECUTE nobody). Caller holds the owner row lock and GUC scope. Withdraws consents, revokes capability sessions, deactivates the owner, redacts destination snapshots no order references, relabels bound manual bundles erased-<32 hex>. Never writes actor_key, claims.meta_intake, live.claim_sources or social.*. Returns counts.';
COMMENT ON FUNCTION customers.buyer_set_consent(bytea,uuid,text,text,boolean,text,text,uuid) IS
 'internal/customers.BuyerSetConsent only; EXECUTE commerce_buyer_runtime. Must be the first buyer-scope step of its transaction (no buyer.WithScope: FOR SHARE then FOR UPDATE deadlocks concurrent requests); owner FOR UPDATE then resolve_scope, always inserts a row per new key; same key + different body = PT409 idempotency_conflict; source/policy passed by Go from server context (CD4). A-3 (R3 ads.put_capi_context) is added by meta-ads, not here.';
COMMENT ON FUNCTION customers.merchant_withdraw_consent(bytea,uuid,uuid,text,text,uuid) IS
 'internal/customers.WithdrawConsent only; EXECUTE commerce_runtime. customers:privacy, GUCs verified (not set), always inserts granted=false merchant_recorded, audit customers.consent_withdrawn. No merchant grant exists (CHECK + no function).';
COMMENT ON FUNCTION customers.record_export(bytea,uuid,uuid,text,uuid,jsonb) IS
 'internal/customers.Export/BuyerExport only; EXECUTE commerce_runtime, commerce_buyer_runtime. Records the EXPORT row in the same transaction as the export reads (an oversized export rolls back with it). Key rule: same key + EXPORT returns the stored row, another kind = PT409.';
COMMENT ON FUNCTION customers.erase_owner(bytea,uuid,uuid,text,uuid) IS
 'internal/customers.Erase/BuyerErase only; EXECUTE commerce_runtime, commerce_buyer_runtime. CD7 erasure: PT410 erased (buyer retry after success), PT409 erasure_blocked (unexpired DRAFT/AWAITING_PAYMENT order, live payments.stripe_sessions row, refund without terminal fact) or idempotency_conflict; one-way per owner; audit customers.erased. Orders, facts, shipments and order-referenced snapshots are retained (U08).';
COMMENT ON FUNCTION customers.replay_erasures(uuid[]) IS
 'internal/customers restore procedure (CD8/§9); EXECUTE nobody (migration owner/ops only). With ids: re-applies erasure for the externally kept owner ids and inserts the missing ERASURE tombstone (via restore, request_key = owner id). Without ids it replays only rows present in this database.';
COMMENT ON FUNCTION customers.buyer_read_privacy(bytea,uuid,boolean) IS
 'internal/customers.ReadBuyerPrivacy/BuyerExport only; EXECUTE commerce_buyer_runtime. Consents + erased flag; p_detail adds consent history, bound-bundle summaries (never actor_key) and privacy actions for the export document. p_detail=true takes the owner FOR UPDATE before resolve_scope (export transaction prologue, same reason as buyer_set_consent); it must be the first buyer-scope step of the transaction. Not in the frozen §3.1 table: no login role may read customers.* directly, so the buyer needs this reader.';
COMMENT ON FUNCTION customers.buyer_export_orders(bytea,uuid) IS
 'internal/customers.BuyerExport only; owner commerce_checkout_writer, EXECUTE commerce_buyer_runtime. The caller''s own orders newest first (<= 201, Go refuses > 200): snapshot without allocation plus the SHIPPED head. Not in the frozen §3.1 table: the buyer pool cannot read checkout tables.';
COMMENT ON FUNCTION identity.read_merchant_customers(bytea,uuid,uuid,integer,timestamptz,uuid,text) IS
 'internal/customers.List/Get only; EXECUTE commerce_runtime. customers:read, GUCs verified, one statement snapshot, fresh final fence. Owners with >= 1 order or bound bundle; read-time projection (CD3, D7); never actor_key, session id or PSP reference.';
COMMENT ON FUNCTION identity.read_finance_summary(bytea,uuid,date,date) IS
 'internal/reporting.Finance only; EXECUTE commerce_runtime. orders:read; 0..91 day range; days in Asia/Taipei; captured/refunded/net by currency and environment; refund counted on its SUCCEEDED day unless a later FAILED/CANCELED fact exists.';
COMMENT ON FUNCTION identity.export_finance_summary(bytea,uuid,date,date) IS
 'internal/reporting.FinanceCSV only; EXECUTE commerce_runtime. orders:export AND orders:read; same rows as read_finance_summary; writes one ops.audit_events row finance.exported.';
COMMENT ON POLICY privacy_audit_insert ON ops.audit_events IS 'commerce_privacy_writer may insert only customers.consent_withdrawn, customers.exported and customers.erased, scoped to the GUCs verified from the merchant authentication result.';
COMMENT ON POLICY auth_finance_export_audit ON ops.audit_events IS 'commerce_auth may insert only the finance.exported audit row, scoped to the GUCs identity.export_finance_summary verified.';
COMMENT ON POLICY privacy_consent_read ON customers.consent_events IS 'USING(true): consent_allows must work from any GUC state; every definer filters tenant/store/owner explicitly.';
COMMENT ON POLICY privacy_owner_read ON buyer.owners IS 'USING(true): consent_allows checks active from any GUC state; writes stay scoped (privacy_owner_update).';
COMMENT ON POLICY privacy_session_read ON buyer.capability_sessions IS 'USING(true): the buyer erasure retry must find a revoked session by token hash before any scope exists; updates stay scoped.';
