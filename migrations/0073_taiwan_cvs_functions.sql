-- 0073 Taiwan CVS logistics functions, grants and policies (contracts/taiwan-cvs-logistics-v1.md FROZEN
-- 2026-09-30 §4.3, §16, §16.8; docs/delivery/units/cvs-core.md defaults C2-C7, C11; rulings X8, X9, B19-B22).
--
-- Owns: every SECURITY DEFINER entry point of the CVS feature (selection, buyer-entered store, request /
-- plan / load / finish / ingest / abandon / settle, collection, store settings, pay-at-pickup release,
-- purpose-scoped ECPay key loaders, the replaced MD6 eligibility predicate and the two merchant order
-- projections) and the exact grant / policy matrix of §4.3 with the round-4 and X9 additions.
--
-- Non-goals: no credential plaintext (the ciphertext is opened in Go with ECPAY_LOGISTICS_KEYRING), no ECPay
-- call (Go and the dispatcher route own the wire), no recipient name/phone stored or returned outside the
-- lease-fenced load_cvs_create, no `checkout.begin_hold` (post_river/0017), no new permission (R-5).
--
-- Additions beyond the contract's function list (each recorded in the unit return, cvs-core.md): read_ecpay_logistics,
-- read_cvs_settings (the runtime has no table grant, so the admin GETs need a reader), read_cvs_action_source (the
-- provider values print-form and the pre-abandon Query/V5 need, which the GET projection deliberately omits),
-- ecpay_connection_id (deterministic connection id so Go can bind the AEAD AAD before the first insert),
-- cvs_selection_projection / order_money_shippable / settle_cvs_attempt (private helpers), and extra trailing
-- parameters of abandon_cvs_shipment (acknowledgement and the found-trade codes of the Query/V5 Go ran).
--
-- Depends on: 0072 (tables and guards), 0063 (eligibility, projections), 0062 (resolve_access grant, ops grants),
-- 0008/0016 (operations), 0013 (checkout writer), post_river/0014 (external_operation_job_commit).
-- Callers: internal/fulfillment, internal/checkout, internal/merchantorders (projection), cmd/claims-worker route
-- (load/finish, via commerce_worker). Every function pins search_path=pg_catalog and REVOKEs PUBLIC.

-- ---------------------------------------------------------------------------------------------------
-- Pure helpers (IMMUTABLE, no table access).
-- ---------------------------------------------------------------------------------------------------
CREATE FUNCTION fulfillment.ecpay_recipient_ok(p_name text,p_phone text) RETURNS boolean
LANGUAGE sql IMMUTABLE SET search_path=pg_catalog AS $$
 -- F5 (retrieved 2026-09-29): ReceiverName 4-10 wide (CJK / full-width count 2), no digits/symbols/emoji;
 -- ReceiverCellPhone ^09[0-9]{8}$. Letters are matched by explicit code-point ranges (not [[:alpha:]]) so the
 -- verdict never depends on the database locale. Go twin: ecpay.RecipientOK (shared golden table).
 SELECT p_name IS NOT NULL AND p_phone IS NOT NULL
  AND p_name ~ '^[A-Za-z㐀-䶿一-鿿豈-﫿Ａ-Ｚａ-ｚ]+( [A-Za-z㐀-䶿一-鿿豈-﫿Ａ-Ｚａ-ｚ]+)*$'
  AND char_length(p_name)+char_length(regexp_replace(p_name,'[^㐀-䶿一-鿿豈-﫿Ａ-Ｚａ-ｚ]','','g')) BETWEEN 4 AND 10
  AND (CASE WHEN regexp_replace(p_phone,'[ ()-]','','g') ~ '^\+8869[0-9]{8}$'
       THEN '0'||substr(regexp_replace(p_phone,'[ ()-]','','g'),5)
       ELSE regexp_replace(p_phone,'[ ()-]','','g') END) ~ '^09[0-9]{8}$'
$$;
ALTER FUNCTION fulfillment.ecpay_recipient_ok(text,text) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION fulfillment.ecpay_recipient_ok(text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION fulfillment.ecpay_recipient_ok(text,text) TO commerce_runtime,commerce_checkout_writer,commerce_worker,commerce_integration_writer;

-- C2: LC + RFC 4648 base32 (upper, no padding) of sha256(operation uuid text)[:18]. Go twin: ecpay.MerchantTradeNo.
CREATE FUNCTION fulfillment.ecpay_trade_no(p_operation uuid) RETURNS text
LANGUAGE sql IMMUTABLE SET search_path=pg_catalog AS $$
 -- Every bitwise operator shares one precedence class in PostgreSQL, so the 5 bits of each base32 digit are weighted with * and +.
 SELECT 'LC'||string_agg(substr('ABCDEFGHIJKLMNOPQRSTUVWXYZ234567',1+
   16*((get_byte(h.d,(i*5)/8)>>(7-((i*5)%8)))&1)
  + 8*((get_byte(h.d,(i*5+1)/8)>>(7-((i*5+1)%8)))&1)
  + 4*((get_byte(h.d,(i*5+2)/8)>>(7-((i*5+2)%8)))&1)
  + 2*((get_byte(h.d,(i*5+3)/8)>>(7-((i*5+3)%8)))&1)
  +   ((get_byte(h.d,(i*5+4)/8)>>(7-((i*5+4)%8)))&1),1),'' ORDER BY i)
 FROM (SELECT sha256(convert_to(p_operation::text,'UTF8')) AS d) h, generate_series(0,17) AS i
$$;
ALTER FUNCTION fulfillment.ecpay_trade_no(uuid) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION fulfillment.ecpay_trade_no(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION fulfillment.ecpay_trade_no(uuid) TO commerce_runtime,commerce_checkout_writer;

-- C5 / F9: order validity in calendar days per subtype (7-ELEVEN 5, FamilyMart 6, Hi-Life and OK 7).
CREATE FUNCTION fulfillment.ecpay_validity_days(p_subtype text) RETURNS integer
LANGUAGE sql IMMUTABLE SET search_path=pg_catalog AS $$
 SELECT CASE WHEN p_subtype LIKE 'UNIMART%' THEN 5 WHEN p_subtype LIKE 'FAMI%' THEN 6
  WHEN p_subtype LIKE 'HILIFE%' OR p_subtype='OKMARTC2C' THEN 7 END
$$;
ALTER FUNCTION fulfillment.ecpay_validity_days(text) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION fulfillment.ecpay_validity_days(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION fulfillment.ecpay_validity_days(text) TO commerce_runtime,commerce_checkout_writer;

-- C5 / F19: LogisticsStatus codes that prove a parcel has not moved (evidence-only edits: TCV07/TCV12).
CREATE FUNCTION fulfillment.ecpay_created_only_codes() RETURNS text[]
LANGUAGE sql IMMUTABLE SET search_path=pg_catalog AS $$ SELECT ARRAY['300']::text[] $$;
ALTER FUNCTION fulfillment.ecpay_created_only_codes() OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION fulfillment.ecpay_created_only_codes() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION fulfillment.ecpay_created_only_codes() TO commerce_runtime,commerce_checkout_writer;

-- Deterministic connection id: at most one ecpay_logistics account exists per (tenant, store, environment)
-- (ecpay_one_account_per_store_environment), so the id can be derived. Go needs it BEFORE the first insert to bind
-- the AEAD AAD (tenant, store, connection, environment, merchant, version) to the ciphertext it submits.
-- Twin: fulfillment.ConnectionID (Go). UUID version/variant bits are set on a sha256 prefix.
CREATE FUNCTION fulfillment.ecpay_connection_id(p_tenant uuid,p_store uuid,p_environment text) RETURNS uuid
LANGUAGE sql IMMUTABLE SET search_path=pg_catalog AS $$
 SELECT encode(set_byte(set_byte(h.b,6,(get_byte(h.b,6)&15)|64),8,(get_byte(h.b,8)&63)|128),'hex')::uuid
 FROM (SELECT substr(sha256(convert_to('ecpay-logistics-connection|'||p_tenant::text||'|'||p_store::text||'|'||p_environment,'UTF8')),1,16) AS b) h
$$;
ALTER FUNCTION fulfillment.ecpay_connection_id(uuid,uuid,text) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION fulfillment.ecpay_connection_id(uuid,uuid,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION fulfillment.ecpay_connection_id(uuid,uuid,text) TO commerce_runtime,commerce_integration_writer,commerce_checkout_writer;

-- ---------------------------------------------------------------------------------------------------
-- Grant / policy matrix (§4.3 + round 3/4 + X9). Tables were created with FORCE RLS in 0072; nothing is granted to
-- commerce_runtime, commerce_checkout_runtime, commerce_buyer_runtime or commerce_worker on them.
-- ---------------------------------------------------------------------------------------------------
GRANT USAGE ON SCHEMA fulfillment TO commerce_integration_writer;
GRANT USAGE ON SCHEMA integration TO commerce_checkout_runtime;   -- load_ecpay_key_for_selection (buyer verify retry)

-- commerce_checkout_writer -----------------------------------------------------------------------------
GRANT SELECT ON integration.ecpay_logistics_profiles TO commerce_checkout_writer;
GRANT UPDATE(updated_at) ON integration.ecpay_logistics_profiles TO commerce_checkout_writer;   -- FOR SHARE lock only
CREATE POLICY cvs_profile_writer_read ON integration.ecpay_logistics_profiles FOR SELECT TO commerce_checkout_writer USING(true);
CREATE POLICY cvs_profile_writer_lock ON integration.ecpay_logistics_profiles FOR UPDATE TO commerce_checkout_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid) WITH CHECK(false);

GRANT SELECT,INSERT ON fulfillment.cvs_selections,fulfillment.cvs_shipments,fulfillment.cvs_shipment_events TO commerce_checkout_writer;
GRANT UPDATE(state,returned_store_id,returned_outside,reject_code,pickup_id,updated_at,version) ON fulfillment.cvs_selections TO commerce_checkout_writer;
GRANT UPDATE(state,provider_logistics_id,cvs_payment_no,cvs_validation_no,shipment_no,last_status_code,last_status_at,result_code,updated_at,version)
 ON fulfillment.cvs_shipments TO commerce_checkout_writer;
CREATE POLICY cvs_selection_writer ON fulfillment.cvs_selections TO commerce_checkout_writer USING(true) WITH CHECK(true);
CREATE POLICY cvs_shipment_writer ON fulfillment.cvs_shipments TO commerce_checkout_writer USING(true) WITH CHECK(true);
CREATE POLICY cvs_event_writer ON fulfillment.cvs_shipment_events TO commerce_checkout_writer USING(true) WITH CHECK(true);

-- §16 round 4: per-store settings, scoped by the definer's GUCs; the UPDATE policy also covers begin_hold's row lock.
GRANT SELECT,INSERT ON fulfillment.cvs_store_settings TO commerce_checkout_writer;
GRANT UPDATE(enabled_chains,pay_at_pickup_enabled,pay_at_pickup_max_twd,pay_at_pickup_max_open,version,updated_at)
 ON fulfillment.cvs_store_settings TO commerce_checkout_writer;
CREATE POLICY cvs_settings_rw ON fulfillment.cvs_store_settings FOR SELECT TO commerce_checkout_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
CREATE POLICY cvs_settings_rw_insert ON fulfillment.cvs_store_settings FOR INSERT TO commerce_checkout_writer
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
CREATE POLICY cvs_settings_rw_update ON fulfillment.cvs_store_settings FOR UPDATE TO commerce_checkout_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);

-- Round 1 finding 8 / §16.1: provider-verified and buyer-entered pickup sources (0013:293-303 gave the writer only a lock policy).
GRANT INSERT ON fulfillment.pickup_versions,fulfillment.pickup_heads TO commerce_checkout_writer;
GRANT UPDATE(current_version,pickup_id) ON fulfillment.pickup_heads TO commerce_checkout_writer;
CREATE POLICY cvs_ecpay_pickup_version_insert ON fulfillment.pickup_versions FOR INSERT TO commerce_checkout_writer
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND namespace ~ '^ecpay\.' AND verification_kind='PROVIDER_DIRECTORY_VERIFIED');
CREATE POLICY cvs_buyer_pickup_version_insert ON fulfillment.pickup_versions FOR INSERT TO commerce_checkout_writer
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND namespace='buyer.'||nullif(current_setting('app.buyer_id',true),'') AND verification_kind='BUYER_ENTERED');
CREATE POLICY cvs_ecpay_pickup_head_insert ON fulfillment.pickup_heads FOR INSERT TO commerce_checkout_writer
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND namespace ~ '^ecpay\.');
CREATE POLICY cvs_buyer_pickup_head_insert ON fulfillment.pickup_heads FOR INSERT TO commerce_checkout_writer
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND namespace='buyer.'||nullif(current_setting('app.buyer_id',true),''));
CREATE POLICY cvs_ecpay_pickup_head_update ON fulfillment.pickup_heads FOR UPDATE TO commerce_checkout_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid AND namespace ~ '^ecpay\.')
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid AND namespace ~ '^ecpay\.');
CREATE POLICY cvs_buyer_pickup_head_update ON fulfillment.pickup_heads FOR UPDATE TO commerce_checkout_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND namespace='buyer.'||nullif(current_setting('app.buyer_id',true),''))
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND namespace='buyer.'||nullif(current_setting('app.buyer_id',true),''));

-- Round 3: any member's attempt is visible to settle/abandon (no principal clause); no INSERT on operations/events, no river grant.
CREATE POLICY checkout_cvs_operation_read ON integration.operations FOR SELECT TO commerce_checkout_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND provider='ecpay_logistics' AND action='ecpay.cvs_create');

-- open_cvs_selection: the return origin must be an ACTIVE, published storefront domain of the store (round 2).
GRANT SELECT ON control.storefront_domains,control.storefront_publications TO commerce_checkout_writer;
CREATE POLICY cvs_domain_writer_read ON control.storefront_domains FOR SELECT TO commerce_checkout_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
CREATE POLICY cvs_publication_writer_read ON control.storefront_publications FOR SELECT TO commerce_checkout_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);

-- Buyer commands (open selection, enter store) keep their idempotency receipts in buyer.command_results (0007), session-scoped.
GRANT SELECT,INSERT ON buyer.command_results TO commerce_checkout_writer;
CREATE POLICY cvs_receipt_read ON buyer.command_results FOR SELECT TO commerce_checkout_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND owner_id=nullif(current_setting('app.buyer_id',true),'')::uuid AND session_id=nullif(current_setting('app.buyer_session_id',true),'')::uuid);
CREATE POLICY cvs_receipt_insert ON buyer.command_results FOR INSERT TO commerce_checkout_writer
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND owner_id=nullif(current_setting('app.buyer_id',true),'')::uuid AND session_id=nullif(current_setting('app.buyer_session_id',true),'')::uuid
  AND operation IN ('fulfillment.cvs_selection.open','fulfillment.cvs_store.enter'));

-- Round 4 R4-2: 0013:175 limited the writer's UPDATE columns on checkout.orders.
GRANT UPDATE(collection_state) ON checkout.orders TO commerce_checkout_writer;
GRANT SELECT(payment_mode,collection_state) ON checkout.orders TO commerce_auth;
GRANT SELECT(tenant_id,store_id,order_id,attempt,state) ON fulfillment.cvs_shipments TO commerce_auth;
CREATE POLICY cvs_shipment_auth_read ON fulfillment.cvs_shipments FOR SELECT TO commerce_auth USING(true);

-- §16.8: the only MERCHANT ledger row the writer may insert is a DEALLOCATE of the definer's own order/principal.
CREATE POLICY checkout_writer_pay_at_pickup_release ON inventory.ledger FOR INSERT TO commerce_checkout_writer
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND actor_kind='MERCHANT' AND kind='DEALLOCATE' AND principal_id IS NOT NULL
  AND buyer_owner_id=nullif(current_setting('app.buyer_id',true),'')::uuid
  AND buyer_session_id=nullif(current_setting('app.buyer_session_id',true),'')::uuid);

-- commerce_integration_writer -----------------------------------------------------------------------------
GRANT SELECT,INSERT ON integration.ecpay_logistics_profiles TO commerce_integration_writer;
GRANT UPDATE(mode,enabled,qualified_credential_version,qualified_at,version,updated_at) ON integration.ecpay_logistics_profiles TO commerce_integration_writer;
CREATE POLICY cvs_profile_integration_read ON integration.ecpay_logistics_profiles FOR SELECT TO commerce_integration_writer USING(true);
CREATE POLICY cvs_profile_integration_insert ON integration.ecpay_logistics_profiles FOR INSERT TO commerce_integration_writer
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
CREATE POLICY cvs_profile_integration_update ON integration.ecpay_logistics_profiles FOR UPDATE TO commerce_integration_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
GRANT INSERT ON integration.merchant_accounts TO commerce_integration_writer;
GRANT UPDATE(credential_version,updated_at) ON integration.merchant_accounts TO commerce_integration_writer;
CREATE POLICY ecpay_registrar_account_insert ON integration.merchant_accounts FOR INSERT TO commerce_integration_writer
 WITH CHECK(provider='ecpay_logistics' AND tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid AND principal_id=nullif(current_setting('app.principal_id',true),'')::uuid);
CREATE POLICY ecpay_registrar_account_update ON integration.merchant_accounts FOR UPDATE TO commerce_integration_writer
 USING(provider='ecpay_logistics' AND tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid)
 WITH CHECK(provider='ecpay_logistics' AND tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
GRANT INSERT ON integration.account_credentials TO commerce_integration_writer;
CREATE POLICY ecpay_registrar_credential_insert ON integration.account_credentials FOR INSERT TO commerce_integration_writer
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND principal_id=nullif(current_setting('app.principal_id',true),'')::uuid
  AND EXISTS(SELECT 1 FROM integration.merchant_accounts a WHERE a.tenant_id=account_credentials.tenant_id
   AND a.store_id=account_credentials.store_id AND a.id=account_credentials.connection_id AND a.provider='ecpay_logistics'));
CREATE POLICY ecpay_registrar_binding_insert ON integration.bindings FOR INSERT TO commerce_integration_writer
 WITH CHECK(provider='ecpay_logistics' AND tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid AND principal_id=nullif(current_setting('app.principal_id',true),'')::uuid);
GRANT EXECUTE ON FUNCTION identity.resolve_access(bytea,uuid,text) TO commerce_integration_writer;
GRANT SELECT,INSERT ON ops.command_results TO commerce_integration_writer;
CREATE POLICY ecpay_registrar_command_read ON ops.command_results FOR SELECT TO commerce_integration_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
CREATE POLICY ecpay_registrar_command_insert ON ops.command_results FOR INSERT TO commerce_integration_writer
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND principal_id=nullif(current_setting('app.principal_id',true),'')::uuid
  AND operation IN ('integration.ecpay_logistics.register','integration.ecpay_logistics.enabled'));
CREATE POLICY ecpay_registrar_audit ON ops.audit_events FOR INSERT TO commerce_integration_writer
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND principal_id=nullif(current_setting('app.principal_id',true),'')::uuid
  AND action IN ('logistics.ecpay.connected','logistics.ecpay.rotated','logistics.ecpay.enabled','logistics.ecpay.disabled'));
CREATE POLICY cvs_create_insert ON integration.operations FOR INSERT TO commerce_integration_writer
 WITH CHECK(provider='ecpay_logistics' AND action='ecpay.cvs_create' AND purpose='transactional' AND actor_kind='MERCHANT'
  AND state='READY' AND generation=0
  AND tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
GRANT SELECT ON fulfillment.cvs_shipments TO commerce_integration_writer;
CREATE POLICY cvs_shipment_integration_read ON fulfillment.cvs_shipments FOR SELECT TO commerce_integration_writer USING(true);
GRANT SELECT(id,tenant_id,store_id,state,expires_at,connection_id) ON fulfillment.cvs_selections TO commerce_integration_writer;
CREATE POLICY cvs_selection_integration_read ON fulfillment.cvs_selections FOR SELECT TO commerce_integration_writer USING(true);
-- The recipient is read from the frozen order snapshot only while a REQUESTED/UNKNOWN attempt exists (TD8).
GRANT SELECT(tenant_id,store_id,owner_id,id,currency,snapshot) ON checkout.orders TO commerce_integration_writer;
CREATE POLICY cvs_order_snapshot_integration ON checkout.orders FOR SELECT TO commerce_integration_writer
 USING(EXISTS(SELECT 1 FROM fulfillment.cvs_shipments c WHERE c.tenant_id=orders.tenant_id AND c.store_id=orders.store_id
  AND c.order_id=orders.id AND c.state IN ('REQUESTED','UNKNOWN')));

-- ---------------------------------------------------------------------------------------------------
-- integration.register_ecpay_logistics / set_ecpay_logistics_enabled (owner integration_writer, EXECUTE commerce_runtime).
-- Merchant token + integration:manage + fresh final authorization (0063 record_manual_shipment pattern).
-- ---------------------------------------------------------------------------------------------------
CREATE FUNCTION integration.register_ecpay_logistics(p_hash bytea,p_store uuid,p_key text,p_request_hash bytea,
 p_expected_version bigint,p_environment text,p_merchant_id text,p_mode text,p_key_id text,p_nonce bytea,
 p_ciphertext bytea,p_probe_ok boolean,p_payment_environment text)
RETURNS jsonb LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE s record; v_final record; v_conn uuid; v_prof record; v_acct record; v_binding uuid; v_new bigint;
 v_saved bytea; v_response jsonb; v_replay boolean:=false; v_now timestamptz; v_action text;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR p_key IS NULL OR p_key !~ '^[A-Za-z0-9_.:-]{8,128}$'
  OR p_request_hash IS NULL OR octet_length(p_request_hash)<>32 OR p_expected_version IS NULL OR p_expected_version<0
  OR p_expected_version>=9223372036854775807 OR p_environment IS NULL OR p_environment NOT IN ('SANDBOX','LIVE')
  OR p_merchant_id IS NULL OR p_merchant_id !~ '^[A-Za-z0-9]{1,10}$' OR p_mode IS NULL OR p_mode NOT IN ('C2C','B2C')
  OR p_key_id IS NULL OR p_key_id !~ '^[A-Za-z0-9_-]{1,40}$' OR p_nonce IS NULL OR octet_length(p_nonce)<>12
  OR p_ciphertext IS NULL OR octet_length(p_ciphertext) NOT BETWEEN 17 AND 8192 OR p_probe_ok IS NULL
  OR p_payment_environment IS NULL OR p_payment_environment NOT IN ('SANDBOX','LIVE','')
  OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid ecpay registration' USING ERRCODE='PT400'; END IF;
 SELECT * INTO s FROM identity.resolve_access(p_hash,p_store,'integration:manage');
 IF s.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF s.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF s.access_status IS DISTINCT FROM 'ok' THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 PERFORM set_config('app.tenant_id',s.tenant_id::text,true),set_config('app.store_id',p_store::text,true),
  set_config('app.principal_id',s.principal_id::text,true);
 PERFORM pg_advisory_xact_lock(hashtextextended('ecpay.register|'||s.tenant_id||'|'||p_store||'|'||p_environment,0));
 SELECT c.request_hash,c.response INTO v_saved,v_response FROM ops.command_results c
  WHERE c.tenant_id=s.tenant_id AND c.store_id=p_store AND c.operation='integration.ecpay_logistics.register'
   AND c.idempotency_key=p_key;
 IF FOUND THEN
  IF v_saved<>p_request_hash THEN RAISE EXCEPTION 'idempotency_conflict' USING ERRCODE='PT409'; END IF;
  v_replay:=true;
 ELSE
  -- Environment pin (round 2): the deployment payment environment is the only ECPay environment a store may hold,
  -- so a LIVE deployment can never hold the public stage merchant. Nothing is written on refusal.
  IF p_payment_environment='' OR p_environment<>p_payment_environment THEN
   RAISE EXCEPTION 'ecpay_environment_not_allowed' USING ERRCODE='PT422'; END IF;
  IF NOT p_probe_ok THEN RAISE EXCEPTION 'ecpay_probe_failed' USING ERRCODE='PT422'; END IF;
  v_conn:=fulfillment.ecpay_connection_id(s.tenant_id,p_store,p_environment);
  SELECT a.* INTO v_acct FROM integration.merchant_accounts a
   WHERE a.tenant_id=s.tenant_id AND a.store_id=p_store AND a.provider='ecpay_logistics' AND a.environment=p_environment FOR UPDATE;
  IF FOUND THEN
   SELECT pr.* INTO v_prof FROM integration.ecpay_logistics_profiles pr
    WHERE pr.tenant_id=s.tenant_id AND pr.store_id=p_store AND pr.connection_id=v_acct.id FOR UPDATE;
   IF p_expected_version<>v_prof.version THEN RAISE EXCEPTION 'version_changed' USING ERRCODE='PT409'; END IF;
   IF v_acct.account_id<>p_merchant_id THEN RAISE EXCEPTION 'merchant_id_changed' USING ERRCODE='PT422'; END IF;
   v_conn:=v_acct.id; v_action:='logistics.ecpay.rotated';
  ELSE
   IF p_expected_version<>0 THEN RAISE EXCEPTION 'version_changed' USING ERRCODE='PT409'; END IF;
   v_action:='logistics.ecpay.connected';
  END IF;
  -- Credential version = the profile version being written, so Go can bind it into the AAD before this call.
  v_new:=p_expected_version+1;
  v_now:=clock_timestamp();
  IF v_action='logistics.ecpay.connected' THEN
   INSERT INTO integration.bindings(tenant_id,store_id,principal_id,provider,external_asset_id)
    VALUES(s.tenant_id,p_store,s.principal_id,'ecpay_logistics',p_environment||':'||p_merchant_id) RETURNING id INTO v_binding;
   INSERT INTO integration.merchant_accounts(id,tenant_id,store_id,principal_id,provider,environment,account_id,binding_id,credential_version)
    VALUES(v_conn,s.tenant_id,p_store,s.principal_id,'ecpay_logistics',p_environment,p_merchant_id,v_binding,v_new);
   INSERT INTO integration.account_credentials(tenant_id,store_id,connection_id,version,key_id,nonce,ciphertext,principal_id)
    VALUES(s.tenant_id,p_store,v_conn,v_new,p_key_id,p_nonce,p_ciphertext,s.principal_id);
   INSERT INTO integration.ecpay_logistics_profiles(tenant_id,store_id,connection_id,mode,endpoint_id,enabled,
    qualified_credential_version,qualified_at,version,updated_at)
    VALUES(s.tenant_id,p_store,v_conn,p_mode,gen_random_uuid(),false,v_new,v_now,v_new,v_now)
    RETURNING * INTO v_prof;
  ELSE
   INSERT INTO integration.account_credentials(tenant_id,store_id,connection_id,version,key_id,nonce,ciphertext,principal_id)
    VALUES(s.tenant_id,p_store,v_conn,v_new,p_key_id,p_nonce,p_ciphertext,s.principal_id);
   UPDATE integration.merchant_accounts SET credential_version=v_new,updated_at=v_now WHERE id=v_conn;
   UPDATE integration.ecpay_logistics_profiles SET mode=p_mode,qualified_credential_version=v_new,qualified_at=v_now,
    version=v_new,updated_at=v_now WHERE tenant_id=s.tenant_id AND store_id=p_store AND connection_id=v_conn
    RETURNING * INTO v_prof;
  END IF;
  INSERT INTO ops.audit_events(tenant_id,store_id,principal_id,action) VALUES(s.tenant_id,p_store,s.principal_id,v_action);
  v_response:=jsonb_build_object('environment',p_environment,'mode',v_prof.mode,'merchant_id',p_merchant_id,
   'version',v_prof.version,'enabled',v_prof.enabled,
   'qualified_at',to_char(v_prof.qualified_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'ok_verified',v_prof.ok_verified,'endpoint_id',v_prof.endpoint_id);
  INSERT INTO ops.command_results(tenant_id,store_id,operation,idempotency_key,request_hash,response,principal_id)
   VALUES(s.tenant_id,p_store,'integration.ecpay_logistics.register',p_key,p_request_hash,v_response,s.principal_id);
 END IF;
 SELECT * INTO v_final FROM identity.resolve_access(p_hash,p_store,'integration:manage');
 IF v_final.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF v_final.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF v_final.access_status<>'ok' OR v_final.tenant_id IS DISTINCT FROM s.tenant_id
  OR v_final.principal_id IS DISTINCT FROM s.principal_id OR v_final.authz_revision IS DISTINCT FROM s.authz_revision THEN
  RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 RETURN v_response;
END $$;
ALTER FUNCTION integration.register_ecpay_logistics(bytea,uuid,text,bytea,bigint,text,text,text,text,bytea,bytea,boolean,text) OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION integration.register_ecpay_logistics(bytea,uuid,text,bytea,bigint,text,text,text,text,bytea,bytea,boolean,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION integration.register_ecpay_logistics(bytea,uuid,text,bytea,bigint,text,text,text,text,bytea,bytea,boolean,text) TO commerce_runtime;

CREATE FUNCTION integration.set_ecpay_logistics_enabled(p_hash bytea,p_store uuid,p_key text,p_request_hash bytea,
 p_expected_version bigint,p_enabled boolean)
RETURNS jsonb LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE s record; v_final record; v_prof record; v_acct record; v_saved bytea; v_response jsonb; v_now timestamptz;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR p_key IS NULL OR p_key !~ '^[A-Za-z0-9_.:-]{8,128}$'
  OR p_request_hash IS NULL OR octet_length(p_request_hash)<>32 OR p_expected_version IS NULL OR p_expected_version<1
  OR p_expected_version>=9223372036854775807 OR p_enabled IS NULL OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid ecpay enable' USING ERRCODE='PT400'; END IF;
 SELECT * INTO s FROM identity.resolve_access(p_hash,p_store,'integration:manage');
 IF s.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF s.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF s.access_status IS DISTINCT FROM 'ok' THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 PERFORM set_config('app.tenant_id',s.tenant_id::text,true),set_config('app.store_id',p_store::text,true),
  set_config('app.principal_id',s.principal_id::text,true);
 PERFORM pg_advisory_xact_lock(hashtextextended('ecpay.enable|'||s.tenant_id||'|'||p_store,0));
 SELECT c.request_hash,c.response INTO v_saved,v_response FROM ops.command_results c
  WHERE c.tenant_id=s.tenant_id AND c.store_id=p_store AND c.operation='integration.ecpay_logistics.enabled'
   AND c.idempotency_key=p_key;
 IF FOUND THEN
  IF v_saved<>p_request_hash THEN RAISE EXCEPTION 'idempotency_conflict' USING ERRCODE='PT409'; END IF;
 ELSE
  -- The store's profile at this version (a store holds at most one per environment; the newest wins a tie).
  SELECT pr.* INTO v_prof FROM integration.ecpay_logistics_profiles pr
   WHERE pr.tenant_id=s.tenant_id AND pr.store_id=p_store AND pr.version=p_expected_version
   ORDER BY pr.updated_at DESC LIMIT 1 FOR UPDATE;
  IF NOT FOUND THEN
   IF EXISTS(SELECT 1 FROM integration.ecpay_logistics_profiles pr WHERE pr.tenant_id=s.tenant_id AND pr.store_id=p_store) THEN
    RAISE EXCEPTION 'version_changed' USING ERRCODE='PT409'; END IF;
   RAISE EXCEPTION 'not found' USING ERRCODE='PT404';
  END IF;
  SELECT a.* INTO v_acct FROM integration.merchant_accounts a WHERE a.tenant_id=s.tenant_id AND a.store_id=p_store AND a.id=v_prof.connection_id;
  IF p_enabled AND v_prof.qualified_credential_version IS DISTINCT FROM v_acct.credential_version THEN
   RAISE EXCEPTION 'not_qualified' USING ERRCODE='PT422'; END IF;
  IF v_prof.enabled<>p_enabled THEN
   v_now:=clock_timestamp();
   BEGIN
    UPDATE integration.ecpay_logistics_profiles SET enabled=p_enabled,version=version+1,updated_at=v_now
     WHERE tenant_id=v_prof.tenant_id AND store_id=v_prof.store_id AND connection_id=v_prof.connection_id
     RETURNING * INTO v_prof;
   EXCEPTION WHEN unique_violation THEN
    RAISE EXCEPTION 'another_profile_enabled' USING ERRCODE='PT409';
   END;
  END IF;
  INSERT INTO ops.audit_events(tenant_id,store_id,principal_id,action)
   VALUES(s.tenant_id,p_store,s.principal_id,CASE WHEN p_enabled THEN 'logistics.ecpay.enabled' ELSE 'logistics.ecpay.disabled' END);
  v_response:=jsonb_build_object('environment',v_acct.environment,'mode',v_prof.mode,'merchant_id',v_acct.account_id,
   'version',v_prof.version,'enabled',v_prof.enabled,
   'qualified_at',to_char(v_prof.qualified_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'ok_verified',v_prof.ok_verified,'endpoint_id',v_prof.endpoint_id);
  INSERT INTO ops.command_results(tenant_id,store_id,operation,idempotency_key,request_hash,response,principal_id)
   VALUES(s.tenant_id,p_store,'integration.ecpay_logistics.enabled',p_key,p_request_hash,v_response,s.principal_id);
 END IF;
 SELECT * INTO v_final FROM identity.resolve_access(p_hash,p_store,'integration:manage');
 IF v_final.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF v_final.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF v_final.access_status<>'ok' OR v_final.tenant_id IS DISTINCT FROM s.tenant_id
  OR v_final.principal_id IS DISTINCT FROM s.principal_id OR v_final.authz_revision IS DISTINCT FROM s.authz_revision THEN
  RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 RETURN v_response;
END $$;
ALTER FUNCTION integration.set_ecpay_logistics_enabled(bytea,uuid,text,bytea,bigint,boolean) OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION integration.set_ecpay_logistics_enabled(bytea,uuid,text,bytea,bigint,boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION integration.set_ecpay_logistics_enabled(bytea,uuid,text,bytea,bigint,boolean) TO commerce_runtime;

-- Merchant reads of the profile and the store settings (the runtime has no table grant on either; integration:read).
-- Stored in fulfillment (not integration) so the T06 approved-integration-function list stays exactly +8.
CREATE FUNCTION fulfillment.read_ecpay_logistics(p_hash bytea,p_store uuid) RETURNS jsonb
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE s record; v_final record; r record;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid ecpay read' USING ERRCODE='PT400'; END IF;
 SELECT * INTO s FROM identity.resolve_access(p_hash,p_store,'integration:read');
 IF s.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF s.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF s.access_status IS DISTINCT FROM 'ok' THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 -- The enabled profile if any, else the newest (a SANDBOX row left behind by an earlier deployment stays visible).
 SELECT a.environment,pr.mode,a.account_id,pr.version,pr.enabled,pr.qualified_at,pr.ok_verified,pr.endpoint_id,
  a.credential_version,pr.qualified_credential_version INTO r
  FROM integration.ecpay_logistics_profiles pr
  JOIN integration.merchant_accounts a ON a.tenant_id=pr.tenant_id AND a.store_id=pr.store_id AND a.id=pr.connection_id
  WHERE pr.tenant_id=s.tenant_id AND pr.store_id=p_store
  ORDER BY pr.enabled DESC,pr.updated_at DESC LIMIT 1;
 SELECT * INTO v_final FROM identity.resolve_access(p_hash,p_store,'integration:read');
 IF v_final.access_status<>'ok' OR v_final.principal_id IS DISTINCT FROM s.principal_id
  OR v_final.authz_revision IS DISTINCT FROM s.authz_revision THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 IF r.environment IS NULL THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 RETURN jsonb_build_object('environment',r.environment,'mode',r.mode,'merchant_id',r.account_id,'version',r.version,
  'enabled',r.enabled,'qualified_at',to_char(r.qualified_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
  'ok_verified',r.ok_verified,'endpoint_id',r.endpoint_id,'credential_version',r.credential_version);
END $$;
ALTER FUNCTION fulfillment.read_ecpay_logistics(bytea,uuid) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION fulfillment.read_ecpay_logistics(bytea,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION fulfillment.read_ecpay_logistics(bytea,uuid) TO commerce_runtime;

CREATE FUNCTION fulfillment.read_cvs_settings(p_hash bytea,p_store uuid) RETURNS jsonb
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE s record; v_final record; r record;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid settings read' USING ERRCODE='PT400'; END IF;
 SELECT * INTO s FROM identity.resolve_access(p_hash,p_store,'integration:read');
 IF s.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF s.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF s.access_status IS DISTINCT FROM 'ok' THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 PERFORM set_config('app.tenant_id',s.tenant_id::text,true),set_config('app.store_id',p_store::text,true),
  set_config('app.principal_id',s.principal_id::text,true);
 SELECT c.* INTO r FROM fulfillment.cvs_store_settings c WHERE c.tenant_id=s.tenant_id AND c.store_id=p_store;
 SELECT * INTO v_final FROM identity.resolve_access(p_hash,p_store,'integration:read');
 IF v_final.access_status<>'ok' OR v_final.principal_id IS DISTINCT FROM s.principal_id
  OR v_final.authz_revision IS DISTINCT FROM s.authz_revision THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 -- C8: no row = the §16.5 defaults at version 0 (pay-at-pickup off).
 IF r.tenant_id IS NULL THEN
  RETURN jsonb_build_object('version',0,'enabled_chains',jsonb_build_array('cvs_711','cvs_familymart','cvs_hilife','cvs_okmart'),
   'pay_at_pickup_enabled',false,'pay_at_pickup_max_twd',NULL,'pay_at_pickup_max_open',20);
 END IF;
 RETURN jsonb_build_object('version',r.version,'enabled_chains',to_jsonb(r.enabled_chains),
  'pay_at_pickup_enabled',r.pay_at_pickup_enabled,'pay_at_pickup_max_twd',r.pay_at_pickup_max_twd,
  'pay_at_pickup_max_open',r.pay_at_pickup_max_open);
END $$;
ALTER FUNCTION fulfillment.read_cvs_settings(bytea,uuid) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION fulfillment.read_cvs_settings(bytea,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION fulfillment.read_cvs_settings(bytea,uuid) TO commerce_runtime;

-- ---------------------------------------------------------------------------------------------------
-- API-side key loaders (round 1, finding 8; R-1). Purpose-scoped so an unauthenticated route can only reach the
-- connection its own URL/selection names. Return the CURRENT credential; decryption happens in Go.
-- ---------------------------------------------------------------------------------------------------
CREATE FUNCTION integration.load_ecpay_key_for_status(p_endpoint uuid)
RETURNS TABLE(tenant_id uuid,store_id uuid,connection_id uuid,merchant_id text,environment text,credential_version bigint,
 key_id text,nonce bytea,ciphertext bytea)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT a.tenant_id,a.store_id,a.id,a.account_id,a.environment,c.version,c.key_id,c.nonce,c.ciphertext
 FROM integration.ecpay_logistics_profiles pr
 JOIN integration.merchant_accounts a ON a.tenant_id=pr.tenant_id AND a.store_id=pr.store_id AND a.id=pr.connection_id
 JOIN integration.account_credentials c ON c.tenant_id=a.tenant_id AND c.store_id=a.store_id AND c.connection_id=a.id
  AND c.version=a.credential_version
 WHERE pr.endpoint_id=p_endpoint AND a.provider='ecpay_logistics'
$$;
ALTER FUNCTION integration.load_ecpay_key_for_status(uuid) OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION integration.load_ecpay_key_for_status(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION integration.load_ecpay_key_for_status(uuid) TO commerce_runtime;

CREATE FUNCTION integration.load_ecpay_key_for_selection(p_selection uuid)
RETURNS TABLE(tenant_id uuid,store_id uuid,connection_id uuid,merchant_id text,environment text,credential_version bigint,
 key_id text,nonce bytea,ciphertext bytea)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT a.tenant_id,a.store_id,a.id,a.account_id,a.environment,c.version,c.key_id,c.nonce,c.ciphertext
 FROM fulfillment.cvs_selections sel
 JOIN integration.merchant_accounts a ON a.tenant_id=sel.tenant_id AND a.store_id=sel.store_id AND a.id=sel.connection_id
 JOIN integration.account_credentials c ON c.tenant_id=a.tenant_id AND c.store_id=a.store_id AND c.connection_id=a.id
  AND c.version=a.credential_version
 WHERE sel.id=p_selection AND sel.state IN ('OPEN','RETURNED') AND sel.expires_at>clock_timestamp()
  AND a.provider='ecpay_logistics'
$$;
ALTER FUNCTION integration.load_ecpay_key_for_selection(uuid) OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION integration.load_ecpay_key_for_selection(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION integration.load_ecpay_key_for_selection(uuid) TO commerce_runtime,commerce_checkout_runtime;

CREATE FUNCTION integration.load_ecpay_key_for_merchant(p_hash bytea,p_store uuid)
RETURNS TABLE(tenant_id uuid,store_id uuid,connection_id uuid,merchant_id text,environment text,credential_version bigint,
 key_id text,nonce bytea,ciphertext bytea)
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE s record; v_final record; r record;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid key load' USING ERRCODE='PT400'; END IF;
 SELECT * INTO s FROM identity.resolve_access(p_hash,p_store,'fulfillment:write');
 IF s.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF s.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF s.access_status IS DISTINCT FROM 'ok' THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 -- The store's enabled connection, else its newest one (print/abandon of a shipment made before a disable).
 SELECT a.tenant_id,a.store_id,a.id,a.account_id,a.environment,c.version,c.key_id,c.nonce,c.ciphertext INTO r
  FROM integration.ecpay_logistics_profiles pr
  JOIN integration.merchant_accounts a ON a.tenant_id=pr.tenant_id AND a.store_id=pr.store_id AND a.id=pr.connection_id
  JOIN integration.account_credentials c ON c.tenant_id=a.tenant_id AND c.store_id=a.store_id AND c.connection_id=a.id
   AND c.version=a.credential_version
  WHERE pr.tenant_id=s.tenant_id AND pr.store_id=p_store AND a.provider='ecpay_logistics'
  ORDER BY pr.enabled DESC,pr.updated_at DESC LIMIT 1;
 SELECT * INTO v_final FROM identity.resolve_access(p_hash,p_store,'fulfillment:write');
 IF v_final.access_status<>'ok' OR v_final.principal_id IS DISTINCT FROM s.principal_id
  OR v_final.authz_revision IS DISTINCT FROM s.authz_revision THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 IF r.id IS NULL THEN RAISE EXCEPTION 'connection_unavailable' USING ERRCODE='PT422'; END IF;
 RETURN QUERY SELECT r.tenant_id,r.store_id,r.id,r.account_id,r.environment,r.version,r.key_id,r.nonce,r.ciphertext;
END $$;
ALTER FUNCTION integration.load_ecpay_key_for_merchant(bytea,uuid) OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION integration.load_ecpay_key_for_merchant(bytea,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION integration.load_ecpay_key_for_merchant(bytea,uuid) TO commerce_runtime;

-- ---------------------------------------------------------------------------------------------------
-- Buyer selection (ECPay map) — owner checkout_writer, EXECUTE commerce_checkout_runtime unless noted (X9 pool note).
-- ---------------------------------------------------------------------------------------------------
CREATE FUNCTION fulfillment.open_cvs_selection(p_buyer_hash bytea,p_store uuid,p_key text,p_request_hash bytea,
 p_cart_version bigint,p_market uuid,p_service_code text,p_nonce_sha256 bytea,p_return_origin text,p_return_path text,
 p_payment_environment text)
RETURNS jsonb LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_scope record; v_now timestamptz; v_cart record; v_head bigint; v_svc record; v_prof record; v_sub text;
 v_saved bytea; v_id uuid:=gen_random_uuid(); v_expires timestamptz; v_chains text[]; v_response jsonb;
BEGIN
 IF p_buyer_hash IS NULL OR octet_length(p_buyer_hash)<>32 OR p_store IS NULL OR p_key IS NULL
  OR p_key !~ '^[A-Za-z0-9_.:-]{8,128}$' OR p_request_hash IS NULL OR octet_length(p_request_hash)<>32
  OR p_cart_version IS NULL OR p_cart_version<1 OR p_market IS NULL OR p_service_code IS NULL
  OR p_service_code !~ '^[a-z][a-z0-9_-]{0,39}$' OR p_nonce_sha256 IS NULL OR octet_length(p_nonce_sha256)<>32
  OR p_return_origin IS NULL OR p_return_path IS NULL OR p_payment_environment IS NULL
  OR p_payment_environment NOT IN ('SANDBOX','LIVE','')
  OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid cvs selection' USING ERRCODE='PT400'; END IF;
 -- Buyer scope before anything else (buyer.WithScope pattern, before replay).
 SELECT * INTO v_scope FROM buyer.resolve_scope(p_buyer_hash,p_store);
 IF NOT FOUND THEN RAISE EXCEPTION 'invalid buyer capability' USING ERRCODE='PT401'; END IF;
 PERFORM set_config('app.tenant_id',v_scope.tenant_id::text,true),set_config('app.store_id',p_store::text,true),
  set_config('app.buyer_id',v_scope.owner_id::text,true),set_config('app.buyer_session_id',v_scope.session_id::text,true),
  set_config('app.principal_id','',true);
 PERFORM pg_advisory_xact_lock(hashtextextended('cvs.selection.open|'||v_scope.tenant_id||'|'||p_store||'|'||v_scope.owner_id||'|'||p_key,0));
 SELECT r.request_hash INTO v_saved FROM buyer.command_results r WHERE r.tenant_id=v_scope.tenant_id AND r.store_id=p_store
  AND r.owner_id=v_scope.owner_id AND r.session_id=v_scope.session_id AND r.operation='fulfillment.cvs_selection.open'
  AND r.idempotency_key=p_key;
 IF FOUND THEN
  -- The nonce is not re-derivable, so a replay cannot return the form: the client opens a new selection (§5.2).
  IF v_saved<>p_request_hash THEN RAISE EXCEPTION 'idempotency_conflict' USING ERRCODE='PT409'; END IF;
  RAISE EXCEPTION 'selection_replay_new_key' USING ERRCODE='PT409';
 END IF;
 IF p_return_path !~ '^/(zh-TW|zh-CN|en)/products/[A-Za-z0-9_-]{1,64}$' THEN
  RAISE EXCEPTION 'bad_return_path' USING ERRCODE='PT422'; END IF;
 IF NOT EXISTS(SELECT 1 FROM control.storefront_domains d JOIN control.storefront_publications pu
   ON pu.tenant_id=d.tenant_id AND pu.store_id=d.store_id AND pu.published
   WHERE d.tenant_id=v_scope.tenant_id AND d.store_id=p_store AND d.state='ACTIVE' AND d.origin=p_return_origin) THEN
  RAISE EXCEPTION 'bad_return_origin' USING ERRCODE='PT422'; END IF;
 -- Lock order: cart FOR SHARE -> service head -> profile.
 SELECT c.id,c.version INTO v_cart FROM storefront.carts c WHERE c.tenant_id=v_scope.tenant_id AND c.store_id=p_store
  AND c.owner_id=v_scope.owner_id FOR SHARE;
 IF NOT FOUND OR v_cart.version<>p_cart_version OR NOT EXISTS(SELECT 1 FROM storefront.cart_lines l
   WHERE l.tenant_id=v_scope.tenant_id AND l.store_id=p_store AND l.owner_id=v_scope.owner_id AND l.cart_id=v_cart.id) THEN
  RAISE EXCEPTION 'cart changed' USING ERRCODE='PT409'; END IF;
 SELECT h.current_version INTO v_head FROM fulfillment.service_heads h WHERE h.tenant_id=v_scope.tenant_id
  AND h.store_id=p_store AND h.market_id=p_market AND h.country='TW' AND h.code=p_service_code FOR SHARE;
 IF NOT FOUND THEN RAISE EXCEPTION 'service_unavailable' USING ERRCODE='PT422'; END IF;
 SELECT s.delivery_kind,s.enabled,s.visible INTO v_svc FROM fulfillment.service_versions s
  WHERE s.tenant_id=v_scope.tenant_id AND s.store_id=p_store AND s.market_id=p_market AND s.country='TW'
   AND s.code=p_service_code AND s.version=v_head;
 IF NOT FOUND OR NOT v_svc.enabled OR NOT v_svc.visible
  OR v_svc.delivery_kind NOT IN ('cvs_711','cvs_familymart','cvs_hilife','cvs_okmart') THEN
  RAISE EXCEPTION 'service_unavailable' USING ERRCODE='PT422'; END IF;
 -- The store's single enabled, qualified profile (unique index ecpay_one_enabled_profile).
 SELECT pr.connection_id,pr.mode,pr.ok_verified,pr.hilife_verified,pr.qualified_credential_version,
  a.environment,a.account_id,a.credential_version INTO v_prof
  FROM integration.ecpay_logistics_profiles pr
  JOIN integration.merchant_accounts a ON a.tenant_id=pr.tenant_id AND a.store_id=pr.store_id AND a.id=pr.connection_id
  WHERE pr.tenant_id=v_scope.tenant_id AND pr.store_id=p_store AND pr.enabled FOR SHARE OF pr;
 IF NOT FOUND OR v_prof.qualified_credential_version IS DISTINCT FROM v_prof.credential_version
  OR p_payment_environment='' OR v_prof.environment<>p_payment_environment
  OR (v_svc.delivery_kind='cvs_okmart' AND NOT v_prof.ok_verified)
  OR (v_svc.delivery_kind='cvs_hilife' AND NOT v_prof.hilife_verified) THEN
  RAISE EXCEPTION 'service_unavailable' USING ERRCODE='PT422'; END IF;
 SELECT c.enabled_chains INTO v_chains FROM fulfillment.cvs_store_settings c
  WHERE c.tenant_id=v_scope.tenant_id AND c.store_id=p_store;
 IF NOT FOUND THEN v_chains:=ARRAY['cvs_711','cvs_familymart','cvs_hilife','cvs_okmart']; END IF;
 IF NOT v_svc.delivery_kind=ANY(v_chains) THEN RAISE EXCEPTION 'service_unavailable' USING ERRCODE='PT422'; END IF;
 v_sub:=CASE WHEN v_prof.mode='C2C' THEN
   CASE v_svc.delivery_kind WHEN 'cvs_711' THEN 'UNIMARTC2C' WHEN 'cvs_familymart' THEN 'FAMIC2C'
    WHEN 'cvs_hilife' THEN 'HILIFEC2C' WHEN 'cvs_okmart' THEN 'OKMARTC2C' END
  ELSE CASE v_svc.delivery_kind WHEN 'cvs_711' THEN 'UNIMART' WHEN 'cvs_familymart' THEN 'FAMI'
    WHEN 'cvs_hilife' THEN 'HILIFE' END END;
 IF v_sub IS NULL THEN RAISE EXCEPTION 'service_unavailable' USING ERRCODE='PT422'; END IF;
 IF (SELECT count(*) FROM fulfillment.cvs_selections x WHERE x.tenant_id=v_scope.tenant_id AND x.store_id=p_store
   AND x.owner_id=v_scope.owner_id AND x.cart_id=v_cart.id AND x.state='OPEN' AND x.expires_at>clock_timestamp())>=10 THEN
  RAISE EXCEPTION 'too_many_open_selections' USING ERRCODE='PT429'; END IF;
 v_now:=clock_timestamp(); v_expires:=v_now+interval '15 minutes';
 INSERT INTO fulfillment.cvs_selections(tenant_id,store_id,owner_id,id,session_id,cart_id,cart_version,kind,connection_id,
  credential_version,logistics_subtype,nonce_sha256,state,return_origin,return_path,created_at,expires_at,updated_at,version)
 VALUES(v_scope.tenant_id,p_store,v_scope.owner_id,v_id,v_scope.session_id,v_cart.id,p_cart_version,v_svc.delivery_kind,
  v_prof.connection_id,v_prof.credential_version,v_sub,p_nonce_sha256,'OPEN',p_return_origin,p_return_path,v_now,v_expires,v_now,1);
 v_response:=jsonb_build_object('selection_id',v_id,'subtype',v_sub,'merchant_id',v_prof.account_id,
  'environment',v_prof.environment,'expires_at',to_char(v_expires AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'));
 INSERT INTO buyer.command_results(tenant_id,store_id,owner_id,session_id,operation,idempotency_key,request_hash,response)
  VALUES(v_scope.tenant_id,p_store,v_scope.owner_id,v_scope.session_id,'fulfillment.cvs_selection.open',p_key,p_request_hash,
   jsonb_build_object('selection_id',v_id));
 SELECT * INTO v_scope FROM buyer.resolve_scope(p_buyer_hash,p_store);
 IF NOT FOUND THEN RAISE EXCEPTION 'buyer capability expired' USING ERRCODE='PT401'; END IF;
 RETURN v_response;
END $$;
ALTER FUNCTION fulfillment.open_cvs_selection(bytea,uuid,text,bytea,bigint,uuid,text,bytea,text,text,text) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION fulfillment.open_cvs_selection(bytea,uuid,text,bytea,bigint,uuid,text,bytea,text,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION fulfillment.open_cvs_selection(bytea,uuid,text,bytea,bigint,uuid,text,bytea,text,text,text) TO commerce_checkout_runtime;

-- Unauthenticated provider return. Check order is part of the contract (round 1): nonce FIRST, so whoever only knows
-- the selection id (it is in the storefront URL and ECPay's ServerReplyURL) can neither change nor burn a selection.
CREATE FUNCTION fulfillment.record_cvs_map_return(p_selection uuid,p_nonce_sha256 bytea,p_merchant_id text,
 p_subtype text,p_store_id text,p_outside text)
RETURNS jsonb LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE sel fulfillment.cvs_selections%ROWTYPE; v_account text; v_diff integer:=0; v_reject text; v_now timestamptz; i integer;
BEGIN
 IF p_selection IS NULL OR p_nonce_sha256 IS NULL OR octet_length(p_nonce_sha256)<>32 THEN
  RAISE EXCEPTION 'invalid map return' USING ERRCODE='PT400'; END IF;
 SELECT x.* INTO sel FROM fulfillment.cvs_selections x WHERE x.id=p_selection FOR UPDATE;
 IF NOT FOUND THEN RETURN jsonb_build_object('known',false); END IF;
 -- Unauthenticated route: the scope is derived from the locked selection so the writer's scoped policies (accounts) apply.
 PERFORM set_config('app.tenant_id',sel.tenant_id::text,true),set_config('app.store_id',sel.store_id::text,true);
 -- Constant-time digest compare (XOR-accumulate over all 32 bytes).
 FOR i IN 0..31 LOOP v_diff:=v_diff | (get_byte(sel.nonce_sha256,i) # get_byte(p_nonce_sha256,i)); END LOOP;
 IF sel.state<>'OPEN' OR v_diff<>0 THEN
  RETURN jsonb_build_object('known',true,'applied',false,'state',sel.state,'return_origin',sel.return_origin,
   'return_path',sel.return_path,'subtype',sel.logistics_subtype,'returned_store_id',sel.returned_store_id);
 END IF;
 v_now:=clock_timestamp();
 SELECT a.account_id INTO v_account FROM integration.merchant_accounts a
  WHERE a.tenant_id=sel.tenant_id AND a.store_id=sel.store_id AND a.id=sel.connection_id;
 IF v_now>=sel.expires_at THEN v_reject:='expired';
 ELSIF p_merchant_id IS DISTINCT FROM v_account THEN v_reject:='merchant_mismatch';
 ELSIF p_subtype IS DISTINCT FROM sel.logistics_subtype THEN v_reject:='subtype_mismatch';
 ELSIF p_store_id IS NULL OR p_store_id !~ '^[A-Za-z0-9]{1,9}$' THEN v_reject:='bad_store_id'; END IF;
 IF v_reject IS NOT NULL THEN
  UPDATE fulfillment.cvs_selections SET state='REJECTED',reject_code=v_reject,updated_at=v_now,version=version+1
   WHERE id=sel.id RETURNING * INTO sel;
 ELSE
  UPDATE fulfillment.cvs_selections SET state='RETURNED',returned_store_id=p_store_id,
   returned_outside=CASE p_outside WHEN '1' THEN true WHEN '0' THEN false END,updated_at=v_now,version=version+1
   WHERE id=sel.id RETURNING * INTO sel;
 END IF;
 RETURN jsonb_build_object('known',true,'applied',true,'state',sel.state,'reject_code',sel.reject_code,
  'return_origin',sel.return_origin,'return_path',sel.return_path,'subtype',sel.logistics_subtype,
  'returned_store_id',sel.returned_store_id);
END $$;
ALTER FUNCTION fulfillment.record_cvs_map_return(uuid,bytea,text,text,text,text) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION fulfillment.record_cvs_map_return(uuid,bytea,text,text,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION fulfillment.record_cvs_map_return(uuid,bytea,text,text,text,text) TO commerce_runtime;

-- Directory verification. Go looked the store up in GetStoreList OUTSIDE any transaction and passes the directory row.
CREATE FUNCTION fulfillment.verify_cvs_selection(p_selection uuid,p_store_id text,p_directory_hit boolean,
 p_dir_name text,p_dir_address text,p_fetched_at timestamptz)
RETURNS jsonb LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE sel fulfillment.cvs_selections%ROWTYPE; v_acct record; v_now timestamptz; v_reject text; v_name text; v_addr text;
 v_ns text; v_head record; v_ver record; v_pickup uuid; v_version bigint; v_outside boolean; v_code text;
BEGIN
 IF p_selection IS NULL OR p_store_id IS NULL OR p_directory_hit IS NULL THEN
  RAISE EXCEPTION 'invalid selection verify' USING ERRCODE='PT400'; END IF;
 SELECT x.* INTO sel FROM fulfillment.cvs_selections x WHERE x.id=p_selection FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'selection not found' USING ERRCODE='PT404'; END IF;
 PERFORM set_config('app.tenant_id',sel.tenant_id::text,true),set_config('app.store_id',sel.store_id::text,true),
  set_config('app.buyer_id',sel.owner_id::text,true),set_config('app.buyer_session_id',sel.session_id::text,true),
  set_config('app.principal_id','',true);
 -- Already decided: idempotent projection (a second verify never writes).
 IF sel.state IN ('VERIFIED','REJECTED','EXPIRED') THEN RETURN fulfillment.cvs_selection_projection(sel.id); END IF;
 IF sel.state<>'RETURNED' OR sel.returned_store_id IS DISTINCT FROM p_store_id THEN
  RAISE EXCEPTION 'selection state' USING ERRCODE='PT409'; END IF;
 v_now:=clock_timestamp();
 v_name:=btrim(coalesce(p_dir_name,'')); v_addr:=btrim(coalesce(p_dir_address,''));
 IF v_now>=sel.expires_at THEN v_reject:='expired';
 ELSIF NOT p_directory_hit THEN v_reject:='store_not_in_directory';
 -- F17: ReceiverStoreID is at most 6 chars; a longer directory code can never be created.
 ELSIF char_length(p_store_id)>6 THEN v_reject:='store_code_length';
 ELSIF char_length(v_name) NOT BETWEEN 1 AND 120 OR v_name ~ '[[:cntrl:]]'
  OR char_length(v_addr) NOT BETWEEN 1 AND 400 OR v_addr ~ '[[:cntrl:]]' THEN v_reject:='bad_directory_row'; END IF;
 IF v_reject IS NOT NULL THEN
  UPDATE fulfillment.cvs_selections SET state='REJECTED',reject_code=v_reject,updated_at=v_now,version=version+1
   WHERE id=sel.id;
  RETURN fulfillment.cvs_selection_projection(sel.id);
 END IF;
 SELECT a.environment,a.principal_id INTO v_acct FROM integration.merchant_accounts a
  WHERE a.tenant_id=sel.tenant_id AND a.store_id=sel.store_id AND a.id=sel.connection_id;
 v_ns:='ecpay.'||lower(v_acct.environment)||'.'||lower(sel.logistics_subtype);
 v_code:=p_store_id;   -- exactly as the directory returned it: a string, leading zeros kept
 -- Same advisory key and lock order as fulfillment.AttestPickup (internal/fulfillment/pickup.go pickupLockKey).
 PERFORM pg_advisory_xact_lock(hashtextextended('fulfillment.pickup|'||sel.tenant_id||'|'||sel.store_id||'|'||sel.kind||'|'||v_ns||'|'||v_code,0));
 SELECT h.current_version,h.pickup_id,h.enabled INTO v_head FROM fulfillment.pickup_heads h
  WHERE h.tenant_id=sel.tenant_id AND h.store_id=sel.store_id AND h.kind=sel.kind AND h.namespace=v_ns AND h.code=v_code FOR UPDATE;
 v_pickup:=NULL;
 IF FOUND AND v_head.enabled THEN
  SELECT v.verification_kind,v.name,v.address,v.valid_until INTO v_ver FROM fulfillment.pickup_versions v
   WHERE v.tenant_id=sel.tenant_id AND v.store_id=sel.store_id AND v.id=v_head.pickup_id;
  -- Reuse only a directory-verified, unchanged, not-expiring-within-1h current version (never a merchant-attested one).
  IF v_ver.verification_kind='PROVIDER_DIRECTORY_VERIFIED' AND v_ver.name=v_name AND v_ver.address=v_addr
   AND v_ver.valid_until>v_now+interval '1 hour' THEN v_pickup:=v_head.pickup_id; END IF;
 END IF;
 IF v_pickup IS NULL THEN
  v_version:=coalesce(v_head.current_version,0)+1;
  INSERT INTO fulfillment.pickup_versions(tenant_id,store_id,kind,namespace,code,version,country,name,address,
   verification_kind,evidence_ref,principal_id,attested_at,valid_until)
  VALUES(sel.tenant_id,sel.store_id,sel.kind,v_ns,v_code,v_version,'TW',v_name,v_addr,'PROVIDER_DIRECTORY_VERIFIED',
   'ecpay-dir:'||sel.id,v_acct.principal_id,v_now,v_now+interval '24 hours') RETURNING id INTO v_pickup;
  IF v_head.pickup_id IS NULL THEN
   INSERT INTO fulfillment.pickup_heads(tenant_id,store_id,kind,namespace,code,current_version,pickup_id,enabled)
    VALUES(sel.tenant_id,sel.store_id,sel.kind,v_ns,v_code,v_version,v_pickup,true);
  ELSE
   UPDATE fulfillment.pickup_heads SET current_version=v_version,pickup_id=v_pickup
    WHERE tenant_id=sel.tenant_id AND store_id=sel.store_id AND kind=sel.kind AND namespace=v_ns AND code=v_code
     AND current_version=v_head.current_version;
   IF NOT FOUND THEN RAISE EXCEPTION 'pickup head moved' USING ERRCODE='PT409'; END IF;
  END IF;
 END IF;
 UPDATE fulfillment.cvs_selections SET state='VERIFIED',pickup_id=v_pickup,updated_at=v_now,version=version+1 WHERE id=sel.id;
 RETURN fulfillment.cvs_selection_projection(sel.id);
END $$;
ALTER FUNCTION fulfillment.verify_cvs_selection(uuid,text,boolean,text,text,timestamptz) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION fulfillment.verify_cvs_selection(uuid,text,boolean,text,text,timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION fulfillment.verify_cvs_selection(uuid,text,boolean,text,text,timestamptz) TO commerce_runtime,commerce_checkout_runtime;

-- Shared projection of one selection (helper, not granted): state, reject_code and, for VERIFIED, the pickup row.
CREATE FUNCTION fulfillment.cvs_selection_projection(p_selection uuid) RETURNS jsonb
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT jsonb_build_object('selection_id',s.id,
  'state',CASE WHEN s.state IN ('OPEN','RETURNED') AND s.expires_at<=clock_timestamp() THEN 'EXPIRED' ELSE s.state END,
  'reject_code',s.reject_code,'kind',s.kind,'subtype',s.logistics_subtype,'returned_store_id',s.returned_store_id,
  'pickup',CASE WHEN s.state='VERIFIED' THEN jsonb_build_object('pickup_id',p.id,'kind',p.kind,'code',p.code,
    'name',p.name,'address',p.address,'outside',coalesce(s.returned_outside,false)) END)
 FROM fulfillment.cvs_selections s
 LEFT JOIN fulfillment.pickup_versions p ON p.tenant_id=s.tenant_id AND p.store_id=s.store_id AND p.id=s.pickup_id
 WHERE s.id=p_selection
$$;
ALTER FUNCTION fulfillment.cvs_selection_projection(uuid) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION fulfillment.cvs_selection_projection(uuid) FROM PUBLIC;

CREATE FUNCTION fulfillment.read_cvs_selection(p_buyer_hash bytea,p_store uuid,p_selection uuid) RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_scope record; sel record;
BEGIN
 IF p_buyer_hash IS NULL OR octet_length(p_buyer_hash)<>32 OR p_store IS NULL OR p_selection IS NULL THEN
  RAISE EXCEPTION 'invalid selection read' USING ERRCODE='PT400'; END IF;
 SELECT * INTO v_scope FROM buyer.resolve_scope(p_buyer_hash,p_store);
 IF NOT FOUND THEN RAISE EXCEPTION 'invalid buyer capability' USING ERRCODE='PT401'; END IF;
 -- Owner + session + cart must match the buyer scope; another buyer's selection is an indistinguishable not-found.
 SELECT x.id INTO sel FROM fulfillment.cvs_selections x
  JOIN storefront.carts c ON c.tenant_id=x.tenant_id AND c.store_id=x.store_id AND c.owner_id=x.owner_id AND c.id=x.cart_id
  WHERE x.id=p_selection AND x.tenant_id=v_scope.tenant_id AND x.store_id=p_store AND x.owner_id=v_scope.owner_id
   AND x.session_id=v_scope.session_id;
 IF NOT FOUND THEN RAISE EXCEPTION 'selection not found' USING ERRCODE='PT404'; END IF;
 RETURN fulfillment.cvs_selection_projection(p_selection);
END $$;
ALTER FUNCTION fulfillment.read_cvs_selection(bytea,uuid,uuid) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION fulfillment.read_cvs_selection(bytea,uuid,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION fulfillment.read_cvs_selection(bytea,uuid,uuid) TO commerce_checkout_runtime;

-- ---------------------------------------------------------------------------------------------------
-- §16.1 buyer-entered store (C1b). Same owner/pool as open_cvs_selection; command receipt in buyer.command_results.
-- ---------------------------------------------------------------------------------------------------
CREATE FUNCTION fulfillment.record_buyer_cvs_store(p_buyer_hash bytea,p_store uuid,p_key text,p_request_hash bytea,
 p_cart_version bigint,p_market uuid,p_service_code text,p_store_code text,p_store_name text,p_store_address text)
RETURNS jsonb LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_scope record; v_cart record; v_head bigint; v_svc record; v_chains text[]; v_saved bytea; v_response jsonb;
 v_ns text; v_pk record; v_ver record; v_pickup uuid; v_version bigint; v_now timestamptz; v_name text; v_addr text; v_saved_resp jsonb; v_rx text;
BEGIN
 IF p_buyer_hash IS NULL OR octet_length(p_buyer_hash)<>32 OR p_store IS NULL OR p_key IS NULL
  OR p_key !~ '^[A-Za-z0-9_.:-]{8,128}$' OR p_request_hash IS NULL OR octet_length(p_request_hash)<>32
  OR p_cart_version IS NULL OR p_cart_version<1 OR p_market IS NULL OR p_service_code IS NULL
  OR p_service_code !~ '^[a-z][a-z0-9_-]{0,39}$' OR p_store_code IS NULL OR p_store_name IS NULL OR p_store_address IS NULL
  OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid buyer store' USING ERRCODE='PT400'; END IF;
 SELECT * INTO v_scope FROM buyer.resolve_scope(p_buyer_hash,p_store);
 IF NOT FOUND THEN RAISE EXCEPTION 'invalid buyer capability' USING ERRCODE='PT401'; END IF;
 PERFORM set_config('app.tenant_id',v_scope.tenant_id::text,true),set_config('app.store_id',p_store::text,true),
  set_config('app.buyer_id',v_scope.owner_id::text,true),set_config('app.buyer_session_id',v_scope.session_id::text,true),
  set_config('app.principal_id','',true);
 PERFORM pg_advisory_xact_lock(hashtextextended('cvs.store.enter|'||v_scope.tenant_id||'|'||p_store||'|'||v_scope.owner_id||'|'||p_key,0));
 SELECT r.request_hash,r.response INTO v_saved,v_saved_resp FROM buyer.command_results r WHERE r.tenant_id=v_scope.tenant_id
  AND r.store_id=p_store AND r.owner_id=v_scope.owner_id AND r.session_id=v_scope.session_id
  AND r.operation='fulfillment.cvs_store.enter' AND r.idempotency_key=p_key;
 IF FOUND THEN
  IF v_saved<>p_request_hash THEN RAISE EXCEPTION 'idempotency_conflict' USING ERRCODE='PT409'; END IF;
  RETURN v_saved_resp;
 END IF;
 v_name:=btrim(p_store_name); v_addr:=btrim(p_store_address);
 SELECT c.id,c.version INTO v_cart FROM storefront.carts c WHERE c.tenant_id=v_scope.tenant_id AND c.store_id=p_store
  AND c.owner_id=v_scope.owner_id FOR SHARE;
 IF NOT FOUND OR v_cart.version<>p_cart_version OR NOT EXISTS(SELECT 1 FROM storefront.cart_lines l
   WHERE l.tenant_id=v_scope.tenant_id AND l.store_id=p_store AND l.owner_id=v_scope.owner_id AND l.cart_id=v_cart.id) THEN
  RAISE EXCEPTION 'cart changed' USING ERRCODE='PT409'; END IF;
 SELECT h.current_version INTO v_head FROM fulfillment.service_heads h WHERE h.tenant_id=v_scope.tenant_id
  AND h.store_id=p_store AND h.market_id=p_market AND h.country='TW' AND h.code=p_service_code FOR SHARE;
 IF NOT FOUND THEN RAISE EXCEPTION 'service_unavailable' USING ERRCODE='PT422'; END IF;
 SELECT s.delivery_kind,s.enabled,s.visible,s.mode INTO v_svc FROM fulfillment.service_versions s
  WHERE s.tenant_id=v_scope.tenant_id AND s.store_id=p_store AND s.market_id=p_market AND s.country='TW'
   AND s.code=p_service_code AND s.version=v_head;
 SELECT c.enabled_chains INTO v_chains FROM fulfillment.cvs_store_settings c WHERE c.tenant_id=v_scope.tenant_id AND c.store_id=p_store;
 IF NOT FOUND THEN v_chains:=ARRAY['cvs_711','cvs_familymart','cvs_hilife','cvs_okmart']; END IF;
 -- Current enabled+visible MANUAL CVS service of a chain the store offers; refused when the store is in ecpay_map mode.
 IF v_svc.delivery_kind IS NULL OR NOT v_svc.enabled OR NOT v_svc.visible OR v_svc.mode<>'MANUAL'
  OR v_svc.delivery_kind NOT IN ('cvs_711','cvs_familymart','cvs_hilife','cvs_okmart') OR NOT v_svc.delivery_kind=ANY(v_chains)
  OR EXISTS(SELECT 1 FROM integration.ecpay_logistics_profiles pr JOIN integration.merchant_accounts a
    ON a.tenant_id=pr.tenant_id AND a.store_id=pr.store_id AND a.id=pr.connection_id
   WHERE pr.tenant_id=v_scope.tenant_id AND pr.store_id=p_store AND pr.enabled
    AND pr.qualified_credential_version=a.credential_version) THEN
  RAISE EXCEPTION 'service_unavailable' USING ERRCODE='PT422'; END IF;
 -- §16.1 F21: per-chain code format (twin: fulfillment.ValidBuyerStoreCode, shared golden table).
 v_rx:=CASE v_svc.delivery_kind WHEN 'cvs_711' THEN '^[0-9]{6}$' WHEN 'cvs_familymart' THEN '^[0-9]{6}$'
   WHEN 'cvs_okmart' THEN '^[0-9]{4}$' ELSE '^[0-9]{3,8}$' END;
 IF p_store_code !~ v_rx THEN RAISE EXCEPTION 'bad_store_code' USING ERRCODE='PT422'; END IF;
 IF char_length(v_name) NOT BETWEEN 1 AND 40 OR v_name ~ '[[:cntrl:]]' OR v_name<>p_store_name THEN
  RAISE EXCEPTION 'bad_store_name' USING ERRCODE='PT422'; END IF;
 IF char_length(v_addr) NOT BETWEEN 5 AND 120 OR v_addr ~ '[[:cntrl:]]' OR v_addr<>p_store_address THEN
  RAISE EXCEPTION 'bad_store_address' USING ERRCODE='PT422'; END IF;
 IF (SELECT count(*) FROM buyer.command_results r WHERE r.tenant_id=v_scope.tenant_id AND r.store_id=p_store
   AND r.owner_id=v_scope.owner_id AND r.operation='fulfillment.cvs_store.enter' AND r.created_at>clock_timestamp()-interval '1 hour')>=20 THEN
  RAISE EXCEPTION 'too_many_stores_entered' USING ERRCODE='PT429'; END IF;
 v_now:=clock_timestamp(); v_ns:='buyer.'||v_scope.owner_id;
 PERFORM pg_advisory_xact_lock(hashtextextended('fulfillment.pickup|'||v_scope.tenant_id||'|'||p_store||'|'||v_svc.delivery_kind||'|'||v_ns||'|'||p_store_code,0));
 SELECT h.current_version,h.pickup_id,h.enabled INTO v_pk FROM fulfillment.pickup_heads h WHERE h.tenant_id=v_scope.tenant_id
  AND h.store_id=p_store AND h.kind=v_svc.delivery_kind AND h.namespace=v_ns AND h.code=p_store_code FOR UPDATE;
 v_pickup:=NULL;
 IF FOUND AND v_pk.enabled THEN
  SELECT v.verification_kind,v.name,v.address,v.valid_until INTO v_ver FROM fulfillment.pickup_versions v
   WHERE v.tenant_id=v_scope.tenant_id AND v.store_id=p_store AND v.id=v_pk.pickup_id;
  IF v_ver.verification_kind='BUYER_ENTERED' AND v_ver.name=v_name AND v_ver.address=v_addr
   AND v_ver.valid_until>v_now+interval '1 hour' THEN v_pickup:=v_pk.pickup_id; END IF;
 END IF;
 IF v_pickup IS NULL THEN
  v_version:=coalesce(v_pk.current_version,0)+1;
  INSERT INTO fulfillment.pickup_versions(tenant_id,store_id,kind,namespace,code,version,country,name,address,
   verification_kind,evidence_ref,principal_id,attested_at,valid_until)
  VALUES(v_scope.tenant_id,p_store,v_svc.delivery_kind,v_ns,p_store_code,v_version,'TW',v_name,v_addr,'BUYER_ENTERED',
   'buyer-entry:'||encode(p_request_hash,'hex'),NULL,v_now,v_now+interval '24 hours') RETURNING id INTO v_pickup;
  IF v_pk.pickup_id IS NULL THEN
   INSERT INTO fulfillment.pickup_heads(tenant_id,store_id,kind,namespace,code,current_version,pickup_id,enabled)
    VALUES(v_scope.tenant_id,p_store,v_svc.delivery_kind,v_ns,p_store_code,v_version,v_pickup,true);
  ELSE
   UPDATE fulfillment.pickup_heads SET current_version=v_version,pickup_id=v_pickup
    WHERE tenant_id=v_scope.tenant_id AND store_id=p_store AND kind=v_svc.delivery_kind AND namespace=v_ns
     AND code=p_store_code AND current_version=v_pk.current_version;
   IF NOT FOUND THEN RAISE EXCEPTION 'pickup head moved' USING ERRCODE='PT409'; END IF;
  END IF;
 END IF;
 v_response:=jsonb_build_object('pickup_id',v_pickup,'kind',v_svc.delivery_kind,'code',p_store_code,'name',v_name,
  'address',v_addr,'source','buyer_entered');
 INSERT INTO buyer.command_results(tenant_id,store_id,owner_id,session_id,operation,idempotency_key,request_hash,response)
  VALUES(v_scope.tenant_id,p_store,v_scope.owner_id,v_scope.session_id,'fulfillment.cvs_store.enter',p_key,p_request_hash,v_response);
 SELECT * INTO v_scope FROM buyer.resolve_scope(p_buyer_hash,p_store);
 IF NOT FOUND THEN RAISE EXCEPTION 'buyer capability expired' USING ERRCODE='PT401'; END IF;
 RETURN v_response;
END $$;
ALTER FUNCTION fulfillment.record_buyer_cvs_store(bytea,uuid,text,bytea,bigint,uuid,text,text,text,text) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION fulfillment.record_buyer_cvs_store(bytea,uuid,text,bytea,bigint,uuid,text,text,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION fulfillment.record_buyer_cvs_store(bytea,uuid,text,bytea,bigint,uuid,text,text,text,text) TO commerce_checkout_runtime;

-- Options: per-store mode and settings (§5.1, §16.5). STABLE; the caller's GUCs are asserted, never trusted alone.
CREATE FUNCTION fulfillment.read_cvs_offer(p_hash bytea,p_store uuid)
RETURNS TABLE(pickup_selection text,enabled_chains text[],pay_at_pickup_enabled boolean,pay_at_pickup_max_twd integer,
 ok_verified boolean,hilife_verified boolean)
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_scope record; c record; pr record;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL THEN
  RAISE EXCEPTION 'invalid offer read' USING ERRCODE='PT400'; END IF;
 SELECT * INTO v_scope FROM buyer.resolve_scope(p_hash,p_store);
 IF NOT FOUND THEN RAISE EXCEPTION 'invalid buyer capability' USING ERRCODE='PT401'; END IF;
 -- The settings policy reads the caller's GUCs; a missing GUC would silently read "no row = defaults", so fail closed.
 IF current_setting('app.tenant_id',true) IS DISTINCT FROM v_scope.tenant_id::text
  OR current_setting('app.store_id',true) IS DISTINCT FROM p_store::text THEN
  RAISE EXCEPTION 'offer scope' USING ERRCODE='PT403'; END IF;
 SELECT s.* INTO c FROM fulfillment.cvs_store_settings s WHERE s.tenant_id=v_scope.tenant_id AND s.store_id=p_store;
 SELECT p.ok_verified AS ok,p.hilife_verified AS hl INTO pr
  FROM integration.ecpay_logistics_profiles p
  JOIN integration.merchant_accounts a ON a.tenant_id=p.tenant_id AND a.store_id=p.store_id AND a.id=p.connection_id
  WHERE p.tenant_id=v_scope.tenant_id AND p.store_id=p_store AND p.enabled
   AND p.qualified_credential_version=a.credential_version;
 RETURN QUERY SELECT CASE WHEN pr.ok IS NOT NULL THEN 'ecpay_map' ELSE 'buyer_entered' END,
  coalesce(c.enabled_chains,ARRAY['cvs_711','cvs_familymart','cvs_hilife','cvs_okmart']::text[]),
  coalesce(c.pay_at_pickup_enabled,false),c.pay_at_pickup_max_twd,coalesce(pr.ok,false),coalesce(pr.hl,false);
END $$;
ALTER FUNCTION fulfillment.read_cvs_offer(bytea,uuid) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION fulfillment.read_cvs_offer(bytea,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION fulfillment.read_cvs_offer(bytea,uuid) TO commerce_checkout_runtime;

-- ---------------------------------------------------------------------------------------------------
-- Shippability. 0063 fulfillment.manual_shipment_eligible (INVOKER) is REPLACED (re-derived from 0063:211-240): its
-- payment/review/refund clauses move VERBATIM into order_money_shippable and gain the pay_at_pickup branch (§16.2); the
-- eligibility adds "no live ECPay attempt" (round 1, finding 1). Owner, ACL and callers (record_manual_shipment,
-- identity.export_unshipped_orders, identity.read_merchant_orders) are unchanged.
-- ---------------------------------------------------------------------------------------------------
CREATE FUNCTION fulfillment.order_money_shippable(p_tenant uuid,p_store uuid,p_order uuid)
RETURNS boolean LANGUAGE sql STABLE SET search_path=pg_catalog AS $$
 SELECT EXISTS(
  SELECT 1 FROM checkout.orders o
  JOIN checkout.payment_attempts a ON a.tenant_id=o.tenant_id AND a.store_id=o.store_id
   AND a.owner_id=o.owner_id AND a.order_id=o.id
  JOIN fulfillment.payment_work_items w ON w.tenant_id=o.tenant_id AND w.store_id=o.store_id
   AND w.owner_id=o.owner_id AND w.order_id=o.id AND w.attempt_id=a.id AND w.state='READY'
  JOIN payments.facts f ON f.tenant_id=a.tenant_id AND f.store_id=a.store_id AND f.attempt_id=a.id
   AND f.kind='CAPTURED' AND f.connection_id=a.connection_id AND f.execution_profile=a.execution_profile
   AND f.environment=a.environment AND f.currency=a.currency AND f.amount_minor=a.amount_minor
   AND f.currency=o.currency AND f.amount_minor=o.total_minor
  WHERE o.tenant_id=p_tenant AND o.store_id=p_store AND o.id=p_order
   AND o.commercial_state='CONFIRMED' AND o.payment_mode='card'
   -- Reviews raised after capture never touch the work item, so they must block shipping themselves;
   -- presentment drift keeps money matched (I05) and does not.
   AND NOT EXISTS(SELECT 1 FROM payments.review_cases rc WHERE rc.tenant_id=a.tenant_id
     AND rc.store_id=a.store_id AND rc.attempt_id=a.id AND rc.reason<>'PROVIDER_PRESENTMENT_DRIFT')
   -- Refund capacity held (no FAILED/CANCELED/REJECTED fact) must stay below captured: a fully
   -- refunded or full-refund-in-flight order cannot ship; a partial refund can (M-8).
   AND NOT EXISTS(SELECT 1 FROM (
     SELECT coalesce(sum(r.amount_minor),0) AS held FROM payments.stripe_refunds r
      WHERE r.tenant_id=a.tenant_id AND r.store_id=a.store_id AND r.attempt_id=a.id
       AND NOT EXISTS(SELECT 1 FROM payments.refund_facts rf WHERE rf.tenant_id=r.tenant_id
        AND rf.store_id=r.store_id AND rf.refund_id=r.id AND rf.kind IN ('FAILED','CANCELED','REJECTED'))
    ) h WHERE h.held>0 AND h.held>=f.amount_minor))
 OR EXISTS(
  -- §16.2: a pay_at_pickup order is shippable while CONFIRMED and PENDING collection; it never has a payment attempt.
  SELECT 1 FROM checkout.orders o
  WHERE o.tenant_id=p_tenant AND o.store_id=p_store AND o.id=p_order
   AND o.commercial_state='CONFIRMED' AND o.payment_mode='pay_at_pickup' AND o.collection_state='PENDING'
   AND NOT EXISTS(SELECT 1 FROM checkout.payment_attempts pa WHERE pa.tenant_id=o.tenant_id
    AND pa.store_id=o.store_id AND pa.order_id=o.id))
$$;
ALTER FUNCTION fulfillment.order_money_shippable(uuid,uuid,uuid) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION fulfillment.order_money_shippable(uuid,uuid,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION fulfillment.order_money_shippable(uuid,uuid,uuid) TO commerce_checkout_writer,commerce_auth;

CREATE OR REPLACE FUNCTION fulfillment.manual_shipment_eligible(p_tenant uuid,p_store uuid,p_order uuid)
RETURNS boolean LANGUAGE sql STABLE SET search_path=pg_catalog AS $$
 SELECT EXISTS(
  SELECT 1 FROM checkout.orders o
  WHERE o.tenant_id=p_tenant AND o.store_id=p_store AND o.id=p_order
   AND o.commercial_state='CONFIRMED' AND o.fulfillment_state='MANUAL_UNASSIGNED'
   AND fulfillment.order_money_shippable(o.tenant_id,o.store_id,o.id)
   -- An ECPay attempt that is REQUESTED/UNKNOWN/CREATED... blocks a manual record (I13).
   AND NOT EXISTS(SELECT 1 FROM fulfillment.cvs_shipments c WHERE c.tenant_id=o.tenant_id AND c.store_id=o.store_id
     AND c.order_id=o.id AND c.state NOT IN ('FAILED','ABANDONED')))
$$;

-- Dispatch-time money check for the worker (round 1, finding 2): sets the scope GUCs from its arguments (the dispatcher
-- has none), answers a boolean, and clears them again.
CREATE FUNCTION fulfillment.cvs_order_payable(p_tenant uuid,p_store uuid,p_order uuid) RETURNS boolean
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_ok boolean;
BEGIN
 IF p_tenant IS NULL OR p_store IS NULL OR p_order IS NULL THEN RAISE EXCEPTION 'invalid payable check' USING ERRCODE='22023'; END IF;
 PERFORM set_config('app.tenant_id',p_tenant::text,true),set_config('app.store_id',p_store::text,true);
 -- Close-out (review P2, 2026-09-30): request_stripe_refund takes this order row FOR UPDATE first (RD3). Waiting on it here means a
 -- refund that is still uncommitted is committed (and therefore seen by the money check below) before the label purchase is
 -- cleared; a refund that starts after this transaction is refused by payments.guard_refund_cvs_dispatch while the operation is DISPATCHING.
 PERFORM 1 FROM checkout.orders o WHERE o.tenant_id=p_tenant AND o.store_id=p_store AND o.id=p_order FOR SHARE;
 v_ok:=fulfillment.order_money_shippable(p_tenant,p_store,p_order);
 PERFORM set_config('app.tenant_id','',true),set_config('app.store_id','',true);
 RETURN v_ok;
END $$;
ALTER FUNCTION fulfillment.cvs_order_payable(uuid,uuid,uuid) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION fulfillment.cvs_order_payable(uuid,uuid,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION fulfillment.cvs_order_payable(uuid,uuid,uuid) TO commerce_integration_writer;

-- Close-out (review P2, 2026-09-30): a card refund cannot start while the label purchase may be on the wire. Between the worker's
-- load_cvs_create and ECPay's answer no transaction is open, so the money check alone cannot see a refund that commits there. The
-- refund request already holds the order lock (RD3) when this trigger runs, and cvs_order_payable waits for that lock, so the two
-- orders are: refund committed first -> the check refuses the Create; operation already DISPATCHING -> the refund is refused here.
-- READY (not yet claimed) stays refundable: that refund is what stops the dispatch (TCV05). Replaces nothing in post_river/0013.
CREATE FUNCTION payments.guard_refund_cvs_dispatch() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 IF EXISTS(SELECT 1 FROM fulfillment.cvs_shipments c
   JOIN integration.operations o ON o.tenant_id=c.tenant_id AND o.store_id=c.store_id AND o.id=c.operation_id
  WHERE c.tenant_id=NEW.tenant_id AND c.store_id=NEW.store_id AND c.order_id=NEW.order_id
   AND c.state='REQUESTED' AND o.state='DISPATCHING') THEN
  RAISE EXCEPTION 'cvs_attempt_in_flight' USING ERRCODE='PT409'; END IF;
 RETURN NEW;
END $$;
ALTER FUNCTION payments.guard_refund_cvs_dispatch() OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION payments.guard_refund_cvs_dispatch() FROM PUBLIC;
CREATE TRIGGER guard_refund_cvs_dispatch BEFORE INSERT ON payments.stripe_refunds
 FOR EACH ROW EXECUTE FUNCTION payments.guard_refund_cvs_dispatch();
COMMENT ON FUNCTION payments.guard_refund_cvs_dispatch() IS 'fulfillment (CVS) owner; BEFORE INSERT on payments.stripe_refunds (written only by payments.request_stripe_refund, which holds the order lock). Refuses PT409 cvs_attempt_in_flight while the order''s CVS label operation is DISPATCHING; reads cvs_shipments and integration.operations only; no writes, no provider I/O.';
GRANT SELECT(tenant_id,store_id,owner_id,id,payment_mode,collection_state,commercial_state) ON checkout.orders TO commerce_integration_writer;

-- ---------------------------------------------------------------------------------------------------
-- settle_cvs_attempt: closes the gaps no dispatcher completion reaches (verified 0008 claim_operation: a binding change
-- writes STALE_BINDING / UNKNOWN binding_changed inside the claim, never via Complete). INVOKER helper, not granted: only
-- the checkout_writer definers below call it, with the tenant/store GUCs already set. Idempotent.
-- ---------------------------------------------------------------------------------------------------
CREATE FUNCTION fulfillment.settle_cvs_attempt(p_tenant uuid,p_store uuid,p_order uuid) RETURNS void
LANGUAGE plpgsql VOLATILE SET search_path=pg_catalog AS $$
DECLARE c fulfillment.cvs_shipments%ROWTYPE; o record; v_to text; v_code text; v_now timestamptz:=clock_timestamp();
BEGIN
 SELECT x.* INTO c FROM fulfillment.cvs_shipments x WHERE x.tenant_id=p_tenant AND x.store_id=p_store AND x.order_id=p_order
  ORDER BY x.attempt DESC LIMIT 1 FOR UPDATE;
 IF NOT FOUND OR c.state NOT IN ('REQUESTED','UNKNOWN') THEN RETURN; END IF;
 SELECT x.state,x.result_code,x.lease_until INTO o FROM integration.operations x
  WHERE x.tenant_id=p_tenant AND x.store_id=p_store AND x.id=c.operation_id;
 IF NOT FOUND THEN RETURN; END IF;
 IF c.state='REQUESTED' AND o.state IN ('STALE_BINDING','BLOCKED_POLICY','CANCELLED') THEN
  -- Nothing was sent (READY never dispatched): a new attempt is safe.
  v_to:='FAILED'; v_code:='ecpay.not_sent.'||regexp_replace(lower(coalesce(nullif(o.result_code,''),o.state)),'[^a-z0-9_.]','_','g');
 ELSIF o.state='UNKNOWN' AND o.lease_until IS NULL AND o.result_code IN ('binding_changed','reconcile_budget_exhausted') THEN
  v_to:='UNKNOWN'; v_code:='ecpay.'||o.result_code;
 ELSIF o.state IN ('DISPATCHING','UNKNOWN') AND o.lease_until IS NOT NULL AND o.lease_until<v_now-interval '1 hour' THEN
  -- A completion that keeps failing, or a job River discarded: the create may exist, so UNKNOWN (never FAILED).
  v_to:='UNKNOWN'; v_code:='ecpay.settle_stale_lease';
 ELSE RETURN; END IF;
 -- An UNKNOWN attempt only records WHY dispatch gave up (abandon is allowed on exactly these codes); no change = no write.
 IF c.state='UNKNOWN' AND c.result_code IS NOT DISTINCT FROM left(v_code,80) THEN RETURN; END IF;
 UPDATE fulfillment.cvs_shipments SET state=v_to,result_code=left(v_code,80),updated_at=v_now,version=version+1
  WHERE tenant_id=c.tenant_id AND store_id=c.store_id AND order_id=c.order_id AND attempt=c.attempt AND version=c.version;
 INSERT INTO fulfillment.cvs_shipment_events(tenant_id,store_id,order_id,attempt,source,event_code,from_state,to_state)
  VALUES(c.tenant_id,c.store_id,c.order_id,c.attempt,'local','shipment.settled',c.state,v_to);
END $$;
ALTER FUNCTION fulfillment.settle_cvs_attempt(uuid,uuid,uuid) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION fulfillment.settle_cvs_attempt(uuid,uuid,uuid) FROM PUBLIC;

-- ---------------------------------------------------------------------------------------------------
-- plan_cvs_create (round 3): the operation is planned here, not through core.Service.Plan (which authorizes
-- integration:execute and derives the operation id itself). Owner integration_writer, EXECUTE only checkout_writer.
-- ---------------------------------------------------------------------------------------------------
CREATE FUNCTION integration.plan_cvs_create(p_tenant uuid,p_store uuid,p_order uuid,p_attempt smallint,p_principal uuid,
 p_connection uuid,p_operation uuid,p_job bigint)
RETURNS void LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE a record; b record; j record; v_request jsonb;
BEGIN
 IF p_tenant IS NULL OR p_store IS NULL OR p_order IS NULL OR p_attempt IS NULL OR p_attempt NOT BETWEEN 1 AND 5
  OR p_principal IS NULL OR p_connection IS NULL OR p_operation IS NULL OR p_job IS NULL OR p_job<=0
  OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid cvs plan' USING ERRCODE='22023'; END IF;
 SELECT x.binding_id INTO a FROM integration.merchant_accounts x WHERE x.tenant_id=p_tenant AND x.store_id=p_store
  AND x.id=p_connection AND x.provider='ecpay_logistics';
 IF NOT FOUND THEN RAISE EXCEPTION 'invalid cvs plan' USING ERRCODE='22023'; END IF;
 -- Binding FOR SHARE via worker_binding_lock (0008:108/118); a disabled binding cannot plan.
 SELECT z.id,z.provider,z.external_asset_id,z.semantic_version,z.enabled INTO b FROM integration.bindings z
  WHERE z.tenant_id=p_tenant AND z.store_id=p_store AND z.id=a.binding_id FOR SHARE;
 IF NOT FOUND OR NOT b.enabled OR b.provider<>'ecpay_logistics' THEN
  RAISE EXCEPTION 'invalid cvs plan' USING ERRCODE='22023'; END IF;
 -- The River job must be exactly the row inserted in this transaction (xmin needs table-level SELECT), exactly as 0064:861-864.
 SELECT r.id INTO j FROM river.river_job r WHERE r.id=p_job AND r.kind='external_operation_v1' AND r.queue='default'
  AND r.unique_key IS NULL AND r.args=jsonb_build_object('operation_id',p_operation::text,'version',1)
  AND r.xmin=pg_current_xact_id()::xid;
 IF NOT FOUND THEN RAISE EXCEPTION 'invalid cvs plan' USING ERRCODE='22023'; END IF;
 -- TD8: the request holds only {order_id, attempt}; the recipient is loaded under lease from the frozen order snapshot.
 v_request:=jsonb_build_object('order_id',p_order,'attempt',p_attempt);
 INSERT INTO integration.operations(tenant_id,store_id,id,principal_id,binding_id,binding_version,provider,external_asset_id,
  purpose,action,semantic_key,request_hash,request,job_id)
 VALUES(p_tenant,p_store,p_operation,p_principal,b.id,b.semantic_version,b.provider,b.external_asset_id,
  'transactional','ecpay.cvs_create','cvs:'||p_order||':'||p_attempt,sha256(convert_to(v_request::text,'UTF8')),v_request,p_job);
 INSERT INTO integration.operation_events(tenant_id,store_id,operation_id,generation,state,mode,reason_code)
  VALUES(p_tenant,p_store,p_operation,0,'READY','','operation_planned');
END $$;
ALTER FUNCTION integration.plan_cvs_create(uuid,uuid,uuid,smallint,uuid,uuid,uuid,bigint) OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION integration.plan_cvs_create(uuid,uuid,uuid,smallint,uuid,uuid,uuid,bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION integration.plan_cvs_create(uuid,uuid,uuid,smallint,uuid,uuid,uuid,bigint) TO commerce_checkout_writer;

-- ---------------------------------------------------------------------------------------------------
-- request_cvs_shipment (§4.3): one merchant click = one operation + one shipment row in one READ COMMITTED tx.
-- A replayed key RAISEs PT2RP; Go rolls back its River job and answers from read_cvs_shipment_command.
-- ---------------------------------------------------------------------------------------------------
CREATE FUNCTION fulfillment.request_cvs_shipment(p_hash bytea,p_store uuid,p_order uuid,p_key text,p_request_hash bytea,
 p_expected_version bigint,p_operation uuid,p_job bigint)
RETURNS jsonb LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE s record; v_final record; o record; l record; v_attempt smallint; pv record; pr record; m text[]; v_env text;
 v_sub text; v_penv text; v_goods bigint; v_collection integer; v_response jsonb; v_now timestamptz;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR p_order IS NULL OR p_key IS NULL
  OR p_key !~ '^[A-Za-z0-9_.:-]{8,128}$' OR p_request_hash IS NULL OR octet_length(p_request_hash)<>32
  OR p_expected_version IS NULL OR p_expected_version<0 OR p_expected_version>=9223372036854775807
  OR p_operation IS NULL OR p_job IS NULL OR p_job<1 OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid cvs shipment request' USING ERRCODE='PT400'; END IF;
 -- Authorize before any lock (0063:299-305); the GUCs below then come from this result, never from the caller.
 SELECT * INTO s FROM identity.resolve_access(p_hash,p_store,'fulfillment:write');
 IF s.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF s.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF s.access_status IS DISTINCT FROM 'ok' THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 PERFORM set_config('app.tenant_id',s.tenant_id::text,true),set_config('app.store_id',p_store::text,true),
  set_config('app.principal_id',s.principal_id::text,true);
 -- Replay (same or different request hash): Go must drop its job and answer from the stored result (PT2RP).
 IF EXISTS(SELECT 1 FROM ops.command_results c WHERE c.tenant_id=s.tenant_id AND c.store_id=p_store
   AND c.operation='fulfillment.cvs_shipment.request' AND c.idempotency_key=p_key) THEN
  RAISE EXCEPTION 'replay' USING ERRCODE='PT2RP'; END IF;
 -- Lock order: order -> settle/attempts (the work item, refund rows and the manual head are read by the eligibility
 -- predicate; a manual record and a refund request both hold this same order lock first).
 SELECT x.owner_id,x.creator_session_id,x.snapshot,x.payment_mode,x.total_minor,x.currency INTO o FROM checkout.orders x
  WHERE x.tenant_id=s.tenant_id AND x.store_id=p_store AND x.id=p_order FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'order not found' USING ERRCODE='PT404'; END IF;
 PERFORM set_config('app.buyer_id',o.owner_id::text,true);
 PERFORM fulfillment.settle_cvs_attempt(s.tenant_id,p_store,p_order);
 SELECT x.attempt,x.state,x.version INTO l FROM fulfillment.cvs_shipments x
  WHERE x.tenant_id=s.tenant_id AND x.store_id=p_store AND x.order_id=p_order ORDER BY x.attempt DESC LIMIT 1 FOR UPDATE;
 IF FOUND THEN
  IF l.state NOT IN ('FAILED','ABANDONED') THEN RAISE EXCEPTION 'not_shippable' USING ERRCODE='PT422'; END IF;
  IF p_expected_version<>l.version THEN RAISE EXCEPTION 'version_changed' USING ERRCODE='PT409'; END IF;
  IF l.attempt>=5 THEN RAISE EXCEPTION 'not_shippable' USING ERRCODE='PT422'; END IF;
  v_attempt:=l.attempt+1;
 ELSE
  IF p_expected_version<>0 THEN RAISE EXCEPTION 'version_changed' USING ERRCODE='PT409'; END IF;
  v_attempt:=1;
 END IF;
 IF NOT fulfillment.manual_shipment_eligible(s.tenant_id,p_store,p_order) THEN
  RAISE EXCEPTION 'not_shippable' USING ERRCODE='PT422'; END IF;
 -- Destination: a CVS kind whose pickup is directory-verified (a buyer-entered or merchant-attested store cannot buy a label).
 IF coalesce(o.snapshot#>>'{destination,kind}','') NOT IN ('cvs_711','cvs_familymart','cvs_hilife','cvs_okmart')
  OR coalesce(o.snapshot#>>'{destination,pickup,id}','') !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' THEN
  RAISE EXCEPTION 'no_cvs_destination' USING ERRCODE='PT422'; END IF;
 SELECT v.id,v.kind,v.namespace,v.code,v.verification_kind INTO pv FROM fulfillment.pickup_versions v
  WHERE v.tenant_id=s.tenant_id AND v.store_id=p_store AND v.id=(o.snapshot#>>'{destination,pickup,id}')::uuid;
 IF NOT FOUND OR pv.verification_kind<>'PROVIDER_DIRECTORY_VERIFIED' OR pv.code !~ '^[A-Za-z0-9]{1,6}$' THEN
  RAISE EXCEPTION 'no_cvs_destination' USING ERRCODE='PT422'; END IF;
 -- The store's single enabled, qualified connection at its current credential version.
 SELECT p.connection_id,p.mode,p.qualified_credential_version,a.environment,a.credential_version INTO pr
  FROM integration.ecpay_logistics_profiles p
  JOIN integration.merchant_accounts a ON a.tenant_id=p.tenant_id AND a.store_id=p.store_id AND a.id=p.connection_id
  WHERE p.tenant_id=s.tenant_id AND p.store_id=p_store AND p.enabled;
 IF NOT FOUND OR pr.qualified_credential_version IS DISTINCT FROM pr.credential_version THEN
  RAISE EXCEPTION 'connection_unavailable' USING ERRCODE='PT422'; END IF;
 m:=regexp_match(pv.namespace,'^ecpay\.(sandbox|live)\.([a-z0-9]+)$');
 IF m IS NULL THEN RAISE EXCEPTION 'no_cvs_destination' USING ERRCODE='PT422'; END IF;
 v_env:=upper(m[1]); v_sub:=upper(m[2]);
 IF v_env<>pr.environment OR v_sub NOT IN ('UNIMARTC2C','FAMIC2C','HILIFEC2C','OKMARTC2C','UNIMART','FAMI','HILIFE')
  OR (pr.mode='C2C' AND v_sub NOT LIKE '%C2C') OR (pr.mode='B2C' AND v_sub LIKE '%C2C') THEN
  RAISE EXCEPTION 'cvs_environment_mismatch' USING ERRCODE='PT422'; END IF;
 IF o.payment_mode='card' THEN
  -- Second guard (round 4): a buyer who paid LIVE never gets a stage label. PROVIDER_MOCK counts as SANDBOX.
  SELECT CASE WHEN at.execution_profile='PROVIDER_MOCK' THEN 'SANDBOX' ELSE f.environment END INTO v_penv
   FROM payments.facts f JOIN checkout.payment_attempts at ON at.tenant_id=f.tenant_id AND at.store_id=f.store_id AND at.id=f.attempt_id
   WHERE f.tenant_id=s.tenant_id AND f.store_id=p_store AND at.order_id=p_order AND f.kind='CAPTURED' LIMIT 1;
  IF v_penv IS DISTINCT FROM pr.environment THEN RAISE EXCEPTION 'cvs_environment_mismatch' USING ERRCODE='PT422'; END IF;
  v_goods:=(o.snapshot#>>'{quote,amount,subtotal_minor}')::bigint;   -- I05: server-frozen items subtotal, TWD x100
  v_collection:=NULL;
 ELSE
  v_goods:=o.total_minor;                                              -- I05: pay_at_pickup collects the order total
 END IF;
 IF o.currency<>'TWD' OR v_goods IS NULL OR v_goods%100<>0 OR v_goods NOT BETWEEN 100 AND 2000000 THEN
  RAISE EXCEPTION 'cvs_amount_exceeds' USING ERRCODE='PT422'; END IF;
 IF o.payment_mode='pay_at_pickup' THEN v_collection:=(v_goods/100)::integer; END IF;
 IF NOT fulfillment.ecpay_recipient_ok(o.snapshot#>>'{destination,recipient_name}',o.snapshot#>>'{destination,phone}') THEN
  RAISE EXCEPTION 'cvs_recipient_rejected' USING ERRCODE='PT422'; END IF;
 PERFORM integration.plan_cvs_create(s.tenant_id,p_store,p_order,v_attempt,s.principal_id,pr.connection_id,p_operation,p_job);
 v_now:=clock_timestamp();
 INSERT INTO fulfillment.cvs_shipments(tenant_id,store_id,owner_id,order_id,attempt,connection_id,credential_version,
  environment,logistics_subtype,receiver_store_id,pickup_id,goods_amount,collection_amount,merchant_trade_no,operation_id,
  state,principal_id,created_at,updated_at,version)
 VALUES(s.tenant_id,p_store,o.owner_id,p_order,v_attempt,pr.connection_id,pr.credential_version,pr.environment,v_sub,pv.code,
  pv.id,(v_goods/100)::integer,v_collection,fulfillment.ecpay_trade_no(p_operation),p_operation,'REQUESTED',s.principal_id,v_now,v_now,1);
 INSERT INTO fulfillment.cvs_shipment_events(tenant_id,store_id,order_id,attempt,source,event_code,to_state)
  VALUES(s.tenant_id,p_store,p_order,v_attempt,'local','shipment.requested','REQUESTED');
 INSERT INTO ops.audit_events(tenant_id,store_id,principal_id,action)
  VALUES(s.tenant_id,p_store,s.principal_id,'fulfillment.cvs_shipment_requested');
 v_response:=jsonb_build_object('attempt',v_attempt,'state','REQUESTED','operation_id',p_operation);
 INSERT INTO ops.command_results(tenant_id,store_id,operation,idempotency_key,request_hash,response,principal_id)
  VALUES(s.tenant_id,p_store,'fulfillment.cvs_shipment.request',p_key,p_request_hash,v_response,s.principal_id);
 SELECT * INTO v_final FROM identity.resolve_access(p_hash,p_store,'fulfillment:write');
 IF v_final.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF v_final.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF v_final.access_status<>'ok' OR v_final.tenant_id IS DISTINCT FROM s.tenant_id
  OR v_final.principal_id IS DISTINCT FROM s.principal_id OR v_final.authz_revision IS DISTINCT FROM s.authz_revision THEN
  RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 RETURN v_response;
END $$;
ALTER FUNCTION fulfillment.request_cvs_shipment(bytea,uuid,uuid,text,bytea,bigint,uuid,bigint) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION fulfillment.request_cvs_shipment(bytea,uuid,uuid,text,bytea,bigint,uuid,bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION fulfillment.request_cvs_shipment(bytea,uuid,uuid,text,bytea,bigint,uuid,bigint) TO commerce_runtime;

-- Replay answer for a PT2RP: writes nothing, returns the stored body (or PT409 idempotency_conflict).
CREATE FUNCTION fulfillment.read_cvs_shipment_command(p_hash bytea,p_store uuid,p_key text,p_request_hash bytea)
RETURNS jsonb LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE s record; v_final record; v_saved bytea; v_response jsonb;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR p_key IS NULL OR p_key !~ '^[A-Za-z0-9_.:-]{8,128}$'
  OR p_request_hash IS NULL OR octet_length(p_request_hash)<>32 OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid replay read' USING ERRCODE='PT400'; END IF;
 SELECT * INTO s FROM identity.resolve_access(p_hash,p_store,'fulfillment:write');
 IF s.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF s.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF s.access_status IS DISTINCT FROM 'ok' THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 PERFORM set_config('app.tenant_id',s.tenant_id::text,true),set_config('app.store_id',p_store::text,true),
  set_config('app.principal_id',s.principal_id::text,true);
 SELECT c.request_hash,c.response INTO v_saved,v_response FROM ops.command_results c
  WHERE c.tenant_id=s.tenant_id AND c.store_id=p_store AND c.operation='fulfillment.cvs_shipment.request' AND c.idempotency_key=p_key;
 SELECT * INTO v_final FROM identity.resolve_access(p_hash,p_store,'fulfillment:write');
 IF v_final.access_status<>'ok' OR v_final.principal_id IS DISTINCT FROM s.principal_id
  OR v_final.authz_revision IS DISTINCT FROM s.authz_revision THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 IF v_saved IS NULL THEN RAISE EXCEPTION 'command not found' USING ERRCODE='PT404'; END IF;
 IF v_saved<>p_request_hash THEN RAISE EXCEPTION 'idempotency_conflict' USING ERRCODE='PT409'; END IF;
 RETURN v_response;
END $$;
ALTER FUNCTION fulfillment.read_cvs_shipment_command(bytea,uuid,text,bytea) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION fulfillment.read_cvs_shipment_command(bytea,uuid,text,bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION fulfillment.read_cvs_shipment_command(bytea,uuid,text,bytea) TO commerce_runtime;

-- ---------------------------------------------------------------------------------------------------
-- load_cvs_create: the route's LoadSecret body. Lease-fenced like load_stripe_credential (0061:947); returns the FROZEN
-- shipment fields and the FROZEN credential version (no current-head fallback). Dispatch mode also proves the shipment is still
-- REQUESTED, the profile enabled and qualified at that version, and the order still payable (round 1, finding 2);
-- reconcile mode (R-7b) needs only the lease fence and the frozen credential: the create may exist.
-- Errors: PT409 = policy refusal (dispatcher: BLOCKED_POLICY, zero requests), 40001 = lease fence, others = plain error.
-- ---------------------------------------------------------------------------------------------------
CREATE FUNCTION integration.load_cvs_create(p_operation uuid,p_generation bigint,p_lease_token bytea,p_mode text)
RETURNS TABLE(tenant_id uuid,store_id uuid,order_id uuid,attempt smallint,connection_id uuid,merchant_id text,
 environment text,credential_version bigint,key_id text,nonce bytea,ciphertext bytea,logistics_subtype text,
 receiver_store_id text,goods_amount integer,collection_amount integer,merchant_trade_no text,trade_created_at timestamptz,
 endpoint_id uuid,recipient_name text,recipient_phone text)
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE o integration.operations%ROWTYPE; c fulfillment.cvs_shipments%ROWTYPE; a integration.merchant_accounts%ROWTYPE;
 pr integration.ecpay_logistics_profiles%ROWTYPE; k integration.account_credentials%ROWTYPE; v_name text; v_phone text;
BEGIN
 IF p_operation IS NULL OR p_generation IS NULL OR p_generation<1 OR p_lease_token IS NULL OR octet_length(p_lease_token)<>32
  OR p_mode IS NULL OR p_mode NOT IN ('dispatch','reconcile') THEN
  RAISE EXCEPTION 'invalid cvs load' USING ERRCODE='22023'; END IF;
 SELECT x.* INTO o FROM integration.operations x WHERE x.id=p_operation AND x.provider='ecpay_logistics' AND x.action='ecpay.cvs_create';
 IF NOT FOUND THEN RAISE EXCEPTION 'cvs operation unavailable' USING ERRCODE='P0002'; END IF;
 -- Lease fence: token digest + generation + unexpired (DB clock) + the mode the claim was made in.
 IF o.generation<>p_generation OR o.lease_until IS NULL OR o.lease_until<=clock_timestamp()
  OR o.lease_token_hash IS DISTINCT FROM sha256(p_lease_token) OR o.lease_mode<>p_mode
  OR o.state<>(CASE p_mode WHEN 'dispatch' THEN 'DISPATCHING' ELSE 'UNKNOWN' END) THEN
  RAISE EXCEPTION 'cvs lease conflict' USING ERRCODE='40001'; END IF;
 SELECT x.* INTO c FROM fulfillment.cvs_shipments x WHERE x.tenant_id=o.tenant_id AND x.store_id=o.store_id AND x.operation_id=o.id;
 IF NOT FOUND THEN RAISE EXCEPTION 'cvs shipment unavailable' USING ERRCODE='P0002'; END IF;
 SELECT x.* INTO a FROM integration.merchant_accounts x WHERE x.tenant_id=c.tenant_id AND x.store_id=c.store_id
  AND x.id=c.connection_id AND x.provider='ecpay_logistics';
 SELECT x.* INTO pr FROM integration.ecpay_logistics_profiles x WHERE x.tenant_id=c.tenant_id AND x.store_id=c.store_id
  AND x.connection_id=c.connection_id;
 SELECT x.* INTO k FROM integration.account_credentials x WHERE x.tenant_id=c.tenant_id AND x.store_id=c.store_id
  AND x.connection_id=c.connection_id AND x.version=c.credential_version;
 IF a.id IS NULL OR pr.connection_id IS NULL OR k.connection_id IS NULL OR o.external_asset_id<>a.environment||':'||a.account_id
  OR a.environment<>c.environment THEN
  RAISE EXCEPTION 'cvs account unavailable' USING ERRCODE='PT409'; END IF;
 IF p_mode='dispatch' THEN
  IF c.state<>'REQUESTED' OR NOT pr.enabled OR a.credential_version<>c.credential_version
   OR pr.qualified_credential_version IS DISTINCT FROM c.credential_version
   OR NOT fulfillment.cvs_order_payable(c.tenant_id,c.store_id,c.order_id) THEN
   RAISE EXCEPTION 'cvs create not allowed' USING ERRCODE='PT409'; END IF;
  -- TD8: the recipient exists only in the frozen order snapshot and only inside this lease-fenced load.
  SELECT x.snapshot#>>'{destination,recipient_name}',x.snapshot#>>'{destination,phone}' INTO v_name,v_phone
   FROM checkout.orders x WHERE x.tenant_id=c.tenant_id AND x.store_id=c.store_id AND x.id=c.order_id;
  IF v_name IS NULL OR v_phone IS NULL THEN RAISE EXCEPTION 'cvs recipient unavailable' USING ERRCODE='PT409'; END IF;
 END IF;
 -- Recheck after every wait so an expired claim cannot release secret material.
 SELECT x.* INTO o FROM integration.operations x WHERE x.id=p_operation;
 IF o.generation<>p_generation OR o.lease_until IS NULL OR o.lease_until<=clock_timestamp()
  OR o.lease_token_hash IS DISTINCT FROM sha256(p_lease_token) OR o.lease_mode<>p_mode THEN
  RAISE EXCEPTION 'cvs lease conflict' USING ERRCODE='40001'; END IF;
 RETURN QUERY SELECT c.tenant_id,c.store_id,c.order_id,c.attempt,c.connection_id,a.account_id,c.environment,c.credential_version,
  k.key_id,k.nonce,k.ciphertext,c.logistics_subtype,c.receiver_store_id,c.goods_amount,c.collection_amount,c.merchant_trade_no,
  c.created_at,pr.endpoint_id,v_name,v_phone;
END $$;
ALTER FUNCTION integration.load_cvs_create(uuid,bigint,bytea,text) OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION integration.load_cvs_create(uuid,bigint,bytea,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION integration.load_cvs_create(uuid,bigint,bytea,text) TO commerce_worker;

-- ---------------------------------------------------------------------------------------------------
-- apply_cvs_create_result: every fulfillment write of a create/query outcome, in ONE definer (round 3, R3-2) so
-- finish_cvs_create writes no fulfillment table itself. TOTAL over provider data (R-7a): a code that does not fit its
-- column is stored NULL plus one local event carrying only sha256(field||':'||raw); an error here is a database fault.
-- Lock order: order -> shipment (request_cvs_shipment and abandon lock the same way).
-- ---------------------------------------------------------------------------------------------------
CREATE FUNCTION fulfillment.apply_cvs_create_result(p_tenant uuid,p_store uuid,p_order uuid,p_attempt smallint,p_operation uuid,
 p_outcome text,p_outcome_code text,p_logistics_id text,p_payment_no text,p_validation_no text,p_shipment_no text,p_status_code text)
RETURNS void LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE c fulfillment.cvs_shipments%ROWTYPE; v_now timestamptz:=clock_timestamp(); v_to text; v_code text; v_pay text; v_val text; v_ship text;
 v_status text; v_lid text; v_bad text[]:=ARRAY[]::text[]; f text; v_raw text; v_ocode text;
BEGIN
 IF p_tenant IS NULL OR p_store IS NULL OR p_order IS NULL OR p_attempt IS NULL OR p_operation IS NULL
  OR p_outcome IS NULL OR p_outcome NOT IN ('SUCCEEDED','FAILED_FINAL','UNKNOWN','BLOCKED_POLICY','CANCELLED','ACKNOWLEDGED') THEN
  RAISE EXCEPTION 'invalid cvs result' USING ERRCODE='22023'; END IF;
 PERFORM set_config('app.tenant_id',p_tenant::text,true),set_config('app.store_id',p_store::text,true);
 PERFORM 1 FROM checkout.orders o WHERE o.tenant_id=p_tenant AND o.store_id=p_store AND o.id=p_order FOR UPDATE;
 SELECT x.* INTO c FROM fulfillment.cvs_shipments x WHERE x.tenant_id=p_tenant AND x.store_id=p_store AND x.order_id=p_order
  AND x.attempt=p_attempt AND x.operation_id=p_operation FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'cvs shipment unavailable' USING ERRCODE='P0002'; END IF;
 PERFORM set_config('app.principal_id',c.principal_id::text,true);
 -- The adapter's codes already carry the ecpay. prefix (ecpay.Client Code*); strip it once so result_code is ecpay.rejected, never
 -- ecpay.ecpay.rejected, while dispatcher-made codes (binding_changed ...) still get it below.
 v_ocode:=regexp_replace(regexp_replace(lower(coalesce(nullif(p_outcome_code,''),'unspecified')),'[^a-z0-9_.]','_','g'),'^ecpay\.','');
 v_lid:=CASE WHEN p_logistics_id ~ '^[0-9A-Za-z_-]{1,40}$' THEN p_logistics_id END;
 v_pay:=p_payment_no; v_val:=p_validation_no; v_ship:=p_shipment_no;
 FOREACH f IN ARRAY ARRAY['payment_no','validation_no','shipment_no'] LOOP
  v_raw:=CASE f WHEN 'payment_no' THEN v_pay WHEN 'validation_no' THEN v_val ELSE v_ship END;
  IF v_raw IS NOT NULL AND v_raw !~ '^[0-9A-Za-z_-]{1,40}$' THEN
   v_bad:=v_bad||f;
   IF f='payment_no' THEN v_pay:=NULL; ELSIF f='validation_no' THEN v_val:=NULL; ELSE v_ship:=NULL; END IF;
   INSERT INTO fulfillment.cvs_shipment_events(tenant_id,store_id,order_id,attempt,source,event_code,body_sha256)
    VALUES(p_tenant,p_store,p_order,p_attempt,'local','ecpay.code_nonconforming',sha256(convert_to(f||':'||v_raw,'UTF8')))
    ON CONFLICT DO NOTHING;
  END IF;
 END LOOP;
 v_status:=CASE WHEN p_status_code ~ '^[0-9]{1,8}$' THEN p_status_code END;
 IF c.state IN ('CREATED','AT_DC','AT_STORE','PICKED_UP','UNCLAIMED') THEN RETURN; END IF;   -- idempotent: already CREATED or later
 IF c.state IN ('FAILED','ABANDONED') THEN
  -- Never a state change and never a unique-index violation: only a merchant alert when a label may exist.
  INSERT INTO fulfillment.cvs_shipment_events(tenant_id,store_id,order_id,attempt,source,event_code,from_state,to_state)
   VALUES(p_tenant,p_store,p_order,p_attempt,'local',
    CASE WHEN p_outcome IN ('SUCCEEDED','UNKNOWN') THEN 'alert.duplicate_label_risk' ELSE 'shipment.late_result' END,c.state,c.state);
  IF p_outcome IN ('SUCCEEDED','UNKNOWN') THEN
   INSERT INTO ops.audit_events(tenant_id,store_id,principal_id,action)
    VALUES(p_tenant,p_store,c.principal_id,'fulfillment.cvs_duplicate_label_risk');
  END IF;
  RETURN;
 END IF;
 -- c.state is REQUESTED or UNKNOWN from here.
 IF p_outcome='SUCCEEDED' AND v_lid IS NOT NULL THEN
  UPDATE fulfillment.cvs_shipments SET state='CREATED',provider_logistics_id=v_lid,cvs_payment_no=v_pay,cvs_validation_no=v_val,
   shipment_no=v_ship,last_status_code=coalesce(v_status,last_status_code),
   last_status_at=CASE WHEN v_status IS NOT NULL THEN v_now ELSE last_status_at END,result_code='ecpay.created',
   updated_at=v_now,version=version+1
   WHERE tenant_id=c.tenant_id AND store_id=c.store_id AND order_id=c.order_id AND attempt=c.attempt;
  UPDATE checkout.orders SET fulfillment_state='PROVIDER_LABEL_CREATED',updated_at=v_now
   WHERE tenant_id=p_tenant AND store_id=p_store AND id=p_order;
  INSERT INTO fulfillment.cvs_shipment_events(tenant_id,store_id,order_id,attempt,source,event_code,from_state,to_state)
   VALUES(p_tenant,p_store,p_order,p_attempt,'local','ecpay.created',c.state,'CREATED');
  RETURN;
 END IF;
 IF p_outcome IN ('BLOCKED_POLICY','CANCELLED') AND c.state='REQUESTED' THEN
  v_to:='FAILED'; v_code:='ecpay.not_sent.'||v_ocode;      -- zero requests were sent: a new attempt is safe
 ELSIF p_outcome='FAILED_FINAL' AND c.state='REQUESTED' THEN
  v_to:='FAILED'; v_code:='ecpay.'||v_ocode;               -- ECPay refused the create (0|msg)
 ELSIF p_outcome='FAILED_FINAL' AND c.state='UNKNOWN' AND v_ocode='trade_not_found' THEN
  v_to:='FAILED'; v_code:='ecpay.trade_not_found';         -- a MAC-verified query proved the trade does not exist
 ELSIF p_outcome IN ('UNKNOWN','SUCCEEDED','FAILED_FINAL','ACKNOWLEDGED') OR p_outcome IN ('BLOCKED_POLICY','CANCELLED') THEN
  -- UNKNOWN never redispatches (I06). A SUCCEEDED without a conforming id (adapter rule: cannot happen) is also UNKNOWN.
  v_to:='UNKNOWN'; v_code:='ecpay.'||v_ocode;
 END IF;
 IF v_to='UNKNOWN' AND c.state='UNKNOWN' AND c.result_code IS NOT DISTINCT FROM left(v_code,80) THEN RETURN; END IF;
 UPDATE fulfillment.cvs_shipments SET state=v_to,result_code=left(v_code,80),updated_at=v_now,version=version+1
  WHERE tenant_id=c.tenant_id AND store_id=c.store_id AND order_id=c.order_id AND attempt=c.attempt;
 INSERT INTO fulfillment.cvs_shipment_events(tenant_id,store_id,order_id,attempt,source,event_code,from_state,to_state)
  VALUES(p_tenant,p_store,p_order,p_attempt,'local','ecpay.'||CASE v_to WHEN 'FAILED' THEN 'failed' ELSE 'unknown' END,c.state,v_to);
END $$;
ALTER FUNCTION fulfillment.apply_cvs_create_result(uuid,uuid,uuid,smallint,uuid,text,text,text,text,text,text,text) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION fulfillment.apply_cvs_create_result(uuid,uuid,uuid,smallint,uuid,text,text,text,text,text,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION fulfillment.apply_cvs_create_result(uuid,uuid,uuid,smallint,uuid,text,text,text,text,text,text,text) TO commerce_integration_writer;

-- Called by the route's Finish hook (R-7a) inside completeOperation's tx BEFORE integration.complete_operation (which runs last
-- and clears the lease). Lease-fenced; writes no fulfillment table itself.
CREATE FUNCTION integration.finish_cvs_create(p_operation uuid,p_generation bigint,p_lease_token bytea,p_outcome text,
 p_outcome_code text,p_logistics_id text,p_payment_no text,p_validation_no text,p_shipment_no text,p_status_code text)
RETURNS void LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE o integration.operations%ROWTYPE; c record;
BEGIN
 IF p_operation IS NULL OR p_generation IS NULL OR p_generation<1 OR p_lease_token IS NULL OR octet_length(p_lease_token)<>32
  OR p_outcome IS NULL OR p_outcome NOT IN ('SUCCEEDED','FAILED_FINAL','UNKNOWN','BLOCKED_POLICY','CANCELLED','ACKNOWLEDGED') THEN
  RAISE EXCEPTION 'invalid cvs finish' USING ERRCODE='22023'; END IF;
 SELECT x.* INTO o FROM integration.operations x WHERE x.id=p_operation AND x.provider='ecpay_logistics' AND x.action='ecpay.cvs_create';
 IF NOT FOUND THEN RAISE EXCEPTION 'cvs operation unavailable' USING ERRCODE='P0002'; END IF;
 IF o.generation<>p_generation OR o.lease_until IS NULL OR o.lease_until<=clock_timestamp()
  OR o.lease_token_hash IS DISTINCT FROM sha256(p_lease_token) OR o.state NOT IN ('DISPATCHING','UNKNOWN') THEN
  RAISE EXCEPTION 'cvs lease conflict' USING ERRCODE='40001'; END IF;
 SELECT x.tenant_id,x.store_id,x.order_id,x.attempt INTO c FROM fulfillment.cvs_shipments x
  WHERE x.tenant_id=o.tenant_id AND x.store_id=o.store_id AND x.operation_id=o.id;
 IF NOT FOUND THEN RAISE EXCEPTION 'cvs shipment unavailable' USING ERRCODE='P0002'; END IF;
 PERFORM fulfillment.apply_cvs_create_result(c.tenant_id,c.store_id,c.order_id,c.attempt,p_operation,p_outcome,p_outcome_code,
  p_logistics_id,p_payment_no,p_validation_no,p_shipment_no,p_status_code);
END $$;
ALTER FUNCTION integration.finish_cvs_create(uuid,bigint,bytea,text,text,text,text,text,text,text) OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION integration.finish_cvs_create(uuid,bigint,bytea,text,text,text,text,text,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION integration.finish_cvs_create(uuid,bigint,bytea,text,text,text,text,text,text,text) TO commerce_worker;

-- ---------------------------------------------------------------------------------------------------
-- ingest_ecpay_status (§4.3, §6, §16.4): called by the unauthenticated status route after Go verified CheckMacValue with
-- the endpoint's keys. Returns 'applied' | 'duplicate' | 'retry' | 'ignored' (Go answers 1|OK except for 'retry' -> 503).
-- Normalises before insert so a vendor variant never aborts the tx and loses the report.
-- Lock order: order -> shipment (the same order as request/abandon/apply).
-- ---------------------------------------------------------------------------------------------------
CREATE FUNCTION fulfillment.ingest_ecpay_status(p_endpoint uuid,p_body_sha256 bytea,p_merchant_id text,p_merchant_trade_no text,
 p_logistics_id text,p_rtn_code text,p_rtn_msg text,p_update_date text,p_payment_no text,p_validation_no text)
RETURNS text LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE pr record; c fulfillment.cvs_shipments%ROWTYPE; o record; v_code text; v_date text; v_msg text; v_to text; v_now timestamptz:=clock_timestamp();
 v_inserted integer; v_seven boolean; v_lid text; v_pay text; v_val text; v_collect text; v_conflict boolean:=false; v_event uuid; v_ship_state text; c_dc text; c_store text; c_pick text; c_back text;
BEGIN
 IF p_endpoint IS NULL OR p_body_sha256 IS NULL OR octet_length(p_body_sha256)<>32 OR p_merchant_id IS NULL
  OR p_merchant_trade_no IS NULL THEN RAISE EXCEPTION 'invalid status report' USING ERRCODE='PT400'; END IF;
 SELECT x.tenant_id,x.store_id,x.connection_id INTO pr FROM integration.ecpay_logistics_profiles x WHERE x.endpoint_id=p_endpoint;
 IF NOT FOUND THEN RETURN 'ignored'; END IF;
 PERFORM set_config('app.tenant_id',pr.tenant_id::text,true),set_config('app.store_id',pr.store_id::text,true);
 IF NOT EXISTS(SELECT 1 FROM integration.merchant_accounts a WHERE a.tenant_id=pr.tenant_id AND a.store_id=pr.store_id
   AND a.id=pr.connection_id AND a.account_id=p_merchant_id) THEN RETURN 'ignored'; END IF;
 SELECT x.order_id INTO o FROM fulfillment.cvs_shipments x WHERE x.connection_id=pr.connection_id AND x.merchant_trade_no=p_merchant_trade_no;
 IF NOT FOUND THEN RETURN 'ignored'; END IF;
 PERFORM 1 FROM checkout.orders k WHERE k.tenant_id=pr.tenant_id AND k.store_id=pr.store_id AND k.id=o.order_id FOR UPDATE;
 SELECT x.* INTO c FROM fulfillment.cvs_shipments x WHERE x.connection_id=pr.connection_id AND x.merchant_trade_no=p_merchant_trade_no FOR UPDATE;
 PERFORM set_config('app.principal_id',c.principal_id::text,true);
 IF c.state='REQUESTED' THEN RETURN 'retry'; END IF;      -- the create's own Finish decides first; ECPay retries
 v_lid:=CASE WHEN p_logistics_id ~ '^[0-9A-Za-z_-]{1,40}$' THEN p_logistics_id END;
 IF c.provider_logistics_id IS NOT NULL AND v_lid IS DISTINCT FROM c.provider_logistics_id THEN RETURN 'ignored'; END IF;
 -- Normalisation (round 1, finding 13): non-conforming values become NULL, the message loses control chars and is truncated.
 v_code:=CASE WHEN p_rtn_code ~ '^[0-9]{1,8}$' THEN p_rtn_code END;
 v_date:=CASE WHEN p_update_date ~ '^[0-9]{4}/[0-9]{2}/[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}$' THEN p_update_date END;
 v_msg:=left(regexp_replace(coalesce(p_rtn_msg,''),'[[:cntrl:]]','','g'),200);
 INSERT INTO fulfillment.cvs_shipment_events(tenant_id,store_id,order_id,attempt,source,body_sha256,provider_code,provider_message,
  provider_updated_at)
 VALUES(c.tenant_id,c.store_id,c.order_id,c.attempt,'ecpay_status',p_body_sha256,v_code,nullif(v_msg,''),v_date)
 ON CONFLICT (tenant_id,store_id,order_id,attempt,body_sha256) DO NOTHING RETURNING id INTO v_event;
 IF v_event IS NULL THEN RETURN 'duplicate'; END IF;
 IF c.state IN ('FAILED','ABANDONED') THEN
  -- MD1 one-live index: never touched. The report proves a label may exist for an attempt the merchant gave up: alert only.
  INSERT INTO fulfillment.cvs_shipment_events(tenant_id,store_id,order_id,attempt,source,event_code,from_state,to_state)
   VALUES(c.tenant_id,c.store_id,c.order_id,c.attempt,'local','alert.duplicate_label_risk',c.state,c.state);
  INSERT INTO ops.audit_events(tenant_id,store_id,principal_id,action)
   VALUES(c.tenant_id,c.store_id,c.principal_id,'fulfillment.cvs_duplicate_label_risk');
  RETURN 'applied';
 END IF;
 v_ship_state:=c.state;
 -- UNKNOWN + a MAC-verified report of this trade proves the create landed: adopt the provider ids, then map the status.
 IF c.state='UNKNOWN' AND v_lid IS NOT NULL THEN
  v_pay:=CASE WHEN p_payment_no ~ '^[0-9A-Za-z_-]{1,40}$' THEN p_payment_no END;
  v_val:=CASE WHEN p_validation_no ~ '^[0-9A-Za-z_-]{1,40}$' THEN p_validation_no END;
  UPDATE fulfillment.cvs_shipments SET state='CREATED',provider_logistics_id=v_lid,cvs_payment_no=coalesce(v_pay,cvs_payment_no),
   cvs_validation_no=coalesce(v_val,cvs_validation_no),result_code='ecpay.created',updated_at=v_now,version=version+1
   WHERE tenant_id=c.tenant_id AND store_id=c.store_id AND order_id=c.order_id AND attempt=c.attempt RETURNING * INTO c;
  UPDATE checkout.orders SET fulfillment_state='PROVIDER_LABEL_CREATED',updated_at=v_now
   WHERE tenant_id=c.tenant_id AND store_id=c.store_id AND id=c.order_id;
  INSERT INTO fulfillment.cvs_shipment_events(tenant_id,store_id,order_id,attempt,source,event_code,from_state,to_state)
   VALUES(c.tenant_id,c.store_id,c.order_id,c.attempt,'local','ecpay.created','UNKNOWN','CREATED');
 ELSIF c.state='UNKNOWN' THEN
  RETURN 'applied';                                  -- event stored; the trade id is unproven, so no transition
 END IF;
 -- §6 exact code table; unknown code, OK mart code or a backwards jump = event only.
 v_seven:=c.logistics_subtype LIKE 'UNIMART%';
 v_to:=NULL;
 -- Expected code per transition for this subtype (F7, retrieved 2026-09-29): to DC 7-11 2030 / others 3024; at store 7-11 B2C 2063,
 -- 7-11 C2C 2073, others 3018; picked up 7-11 2067 / others 3022; unclaimed 7-11 2074 / others 3020; 7-11 re-delivered 2098.
 c_dc:=CASE WHEN v_seven THEN '2030' ELSE '3024' END;
 c_store:=CASE WHEN c.logistics_subtype='UNIMART' THEN '2063' WHEN c.logistics_subtype='UNIMARTC2C' THEN '2073' ELSE '3018' END;
 c_pick:=CASE WHEN v_seven THEN '2067' ELSE '3022' END;
 c_back:=CASE WHEN v_seven THEN '2074' ELSE '3020' END;
 IF c.logistics_subtype<>'OKMARTC2C' AND v_code IS NOT NULL THEN
  IF c.state='CREATED' AND v_code=c_dc THEN v_to:='AT_DC';
  ELSIF c.state IN ('CREATED','AT_DC') AND v_code=c_store THEN v_to:='AT_STORE';
  ELSIF c.state='AT_STORE' AND v_code=c_pick THEN v_to:='PICKED_UP';
  ELSIF c.state='AT_STORE' AND v_code=c_back THEN v_to:='UNCLAIMED';
  ELSIF c.state IN ('AT_STORE','UNCLAIMED') AND v_seven AND v_code='2098' THEN v_to:='AT_STORE';
  END IF;
 END IF;
 UPDATE fulfillment.cvs_shipments SET state=coalesce(v_to,state),last_status_code=coalesce(v_code,last_status_code),
  last_status_at=CASE WHEN v_code IS NOT NULL THEN v_now ELSE last_status_at END,updated_at=v_now,version=version+1
  WHERE tenant_id=c.tenant_id AND store_id=c.store_id AND order_id=c.order_id AND attempt=c.attempt;
 IF v_to IS NOT NULL THEN
  INSERT INTO fulfillment.cvs_shipment_events(tenant_id,store_id,order_id,attempt,source,event_code,from_state,to_state)
   VALUES(c.tenant_id,c.store_id,c.order_id,c.attempt,'local','shipment.status',c.state,v_to);
 END IF;
 -- §16.4 collection: 2067/3022 PICKED_UP => COLLECTED, 2074/3020 UNCLAIMED => RETURNED (pay_at_pickup orders only, audited).
 -- Keyed on the reported code, not on the transition: a pickup/return report that cannot move the shipment (e.g. 2067 after
 -- 2074 + merchant restock, TCV17) still raises collection_conflict against the state the merchant already recorded.
 IF c.logistics_subtype<>'OKMARTC2C' AND v_code IN (c_pick,c_back) THEN
  SELECT k.payment_mode,k.collection_state INTO o FROM checkout.orders k
   WHERE k.tenant_id=c.tenant_id AND k.store_id=c.store_id AND k.id=c.order_id;
  IF o.payment_mode='pay_at_pickup' THEN
   v_collect:=CASE v_code WHEN c_pick THEN 'COLLECTED' ELSE 'RETURNED' END;
   IF o.collection_state='PENDING' AND v_to IN ('PICKED_UP','UNCLAIMED') THEN
    UPDATE checkout.orders SET collection_state=v_collect,updated_at=v_now
     WHERE tenant_id=c.tenant_id AND store_id=c.store_id AND id=c.order_id;
    -- §11.5 n/a: no stock, ledger, payment or refund row; the platform never moves pay-at-pickup money.
    INSERT INTO ops.audit_events(tenant_id,store_id,principal_id,action)
     VALUES(c.tenant_id,c.store_id,c.principal_id,'fulfillment.collection_reported');
   ELSIF o.collection_state<>'PENDING' AND o.collection_state <> ALL(CASE v_collect WHEN 'COLLECTED'
     THEN ARRAY['COLLECTED','REFUNDED_OFFLINE'] ELSE ARRAY['RETURNED','RESTOCKED'] END) THEN
    -- A conflicting state the merchant already recorded (its own successors REFUNDED_OFFLINE/RESTOCKED are not a conflict):
    -- event + alert only, never a change.
    INSERT INTO fulfillment.cvs_shipment_events(tenant_id,store_id,order_id,attempt,source,event_code,from_state,to_state)
     VALUES(c.tenant_id,c.store_id,c.order_id,c.attempt,'local','alert.collection_conflict',o.collection_state,v_collect);
   END IF;
  END IF;
 END IF;
 RETURN 'applied';
END $$;
ALTER FUNCTION fulfillment.ingest_ecpay_status(uuid,bytea,text,text,text,text,text,text,text,text) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION fulfillment.ingest_ecpay_status(uuid,bytea,text,text,text,text,text,text,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION fulfillment.ingest_ecpay_status(uuid,bytea,text,text,text,text,text,text,text,text) TO commerce_runtime;

-- ---------------------------------------------------------------------------------------------------
-- abandon_cvs_shipment (§4.3, §6). Extra trailing parameters vs the contract list (recorded): the UNKNOWN acknowledgement and the
-- found-trade codes of the MAC-verified Query/V5 Go ran with the merchant key loader. The definer never calls ECPay.
-- A found trade is a RETURNED outcome (not a RAISE) so the UNKNOWN -> CREATED write commits (never leave a bought label in UNKNOWN).
-- ---------------------------------------------------------------------------------------------------
CREATE FUNCTION fulfillment.abandon_cvs_shipment(p_hash bytea,p_store uuid,p_order uuid,p_key text,p_request_hash bytea,
 p_expected_version bigint,p_query_status text,p_query_at timestamptz,p_acknowledged boolean,p_found_logistics_id text,
 p_found_payment_no text,p_found_validation_no text,p_found_shipment_no text)
RETURNS jsonb LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE s record; v_final record; o record; c fulfillment.cvs_shipments%ROWTYPE; v_saved bytea; v_response jsonb; v_now timestamptz;
 v_pay text; v_val text; v_ship text;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR p_order IS NULL OR p_key IS NULL
  OR p_key !~ '^[A-Za-z0-9_.:-]{8,128}$' OR p_request_hash IS NULL OR octet_length(p_request_hash)<>32
  OR p_expected_version IS NULL OR p_expected_version<1 OR p_expected_version>=9223372036854775807 OR p_acknowledged IS NULL
  OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid cvs abandon' USING ERRCODE='PT400'; END IF;
 SELECT * INTO s FROM identity.resolve_access(p_hash,p_store,'fulfillment:write');
 IF s.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF s.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF s.access_status IS DISTINCT FROM 'ok' THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 PERFORM set_config('app.tenant_id',s.tenant_id::text,true),set_config('app.store_id',p_store::text,true),
  set_config('app.principal_id',s.principal_id::text,true);
 SELECT k.owner_id INTO o FROM checkout.orders k WHERE k.tenant_id=s.tenant_id AND k.store_id=p_store AND k.id=p_order FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'order not found' USING ERRCODE='PT404'; END IF;
 PERFORM set_config('app.buyer_id',o.owner_id::text,true);
 SELECT r.request_hash,r.response INTO v_saved,v_response FROM ops.command_results r WHERE r.tenant_id=s.tenant_id
  AND r.store_id=p_store AND r.operation='fulfillment.cvs_shipment.abandon' AND r.idempotency_key=p_key;
 IF FOUND THEN
  IF v_saved<>p_request_hash THEN RAISE EXCEPTION 'idempotency_conflict' USING ERRCODE='PT409'; END IF;
 ELSE
  PERFORM fulfillment.settle_cvs_attempt(s.tenant_id,p_store,p_order);
  SELECT x.* INTO c FROM fulfillment.cvs_shipments x WHERE x.tenant_id=s.tenant_id AND x.store_id=p_store AND x.order_id=p_order
   ORDER BY x.attempt DESC LIMIT 1 FOR UPDATE;
  IF NOT FOUND THEN RAISE EXCEPTION 'no_shipment' USING ERRCODE='PT404'; END IF;
  IF c.version<>p_expected_version THEN RAISE EXCEPTION 'version_changed' USING ERRCODE='PT409'; END IF;
  v_now:=clock_timestamp();
  IF c.state='CREATED' THEN
   -- F9 lapse window, no status movement recorded, and a FRESH created-only Query/V5 (status reports are not real-time, F7).
   IF v_now<=c.created_at+make_interval(days=>fulfillment.ecpay_validity_days(c.logistics_subtype)) THEN
    RAISE EXCEPTION 'not_lapsed' USING ERRCODE='PT409'; END IF;
   IF EXISTS(SELECT 1 FROM fulfillment.cvs_shipment_events e WHERE e.tenant_id=c.tenant_id AND e.store_id=c.store_id
     AND e.order_id=c.order_id AND e.attempt=c.attempt AND e.source='ecpay_status')
    OR p_query_status IS NULL OR NOT p_query_status=ANY(fulfillment.ecpay_created_only_codes())
    OR p_query_at IS NULL OR p_query_at>v_now OR p_query_at<v_now-interval '10 minutes' THEN
    RAISE EXCEPTION 'ecpay_shows_movement' USING ERRCODE='PT409'; END IF;
  ELSIF c.state='UNKNOWN' THEN
   IF NOT p_acknowledged THEN RAISE EXCEPTION 'acknowledgement_required' USING ERRCODE='PT422'; END IF;
   -- Only once dispatch has given up (budget exhausted / binding changed / stale lease): otherwise the reconcile still runs.
   IF c.result_code IS NULL OR c.result_code NOT IN ('ecpay.reconcile_budget_exhausted','ecpay.binding_changed','ecpay.settle_stale_lease') THEN
    RAISE EXCEPTION 'reconcile_in_progress' USING ERRCODE='PT409'; END IF;
   IF p_found_logistics_id IS NOT NULL AND p_found_logistics_id ~ '^[0-9A-Za-z_-]{1,40}$' THEN
    v_pay:=CASE WHEN p_found_payment_no ~ '^[0-9A-Za-z_-]{1,40}$' THEN p_found_payment_no END;
    v_val:=CASE WHEN p_found_validation_no ~ '^[0-9A-Za-z_-]{1,40}$' THEN p_found_validation_no END;
    v_ship:=CASE WHEN p_found_shipment_no ~ '^[0-9A-Za-z_-]{1,40}$' THEN p_found_shipment_no END;
    UPDATE fulfillment.cvs_shipments SET state='CREATED',provider_logistics_id=p_found_logistics_id,cvs_payment_no=v_pay,
     cvs_validation_no=v_val,shipment_no=v_ship,result_code='ecpay.created',updated_at=v_now,version=version+1
     WHERE tenant_id=c.tenant_id AND store_id=c.store_id AND order_id=c.order_id AND attempt=c.attempt;
    UPDATE checkout.orders SET fulfillment_state='PROVIDER_LABEL_CREATED',updated_at=v_now
     WHERE tenant_id=c.tenant_id AND store_id=c.store_id AND id=c.order_id;
    INSERT INTO fulfillment.cvs_shipment_events(tenant_id,store_id,order_id,attempt,source,event_code,from_state,to_state)
     VALUES(c.tenant_id,c.store_id,c.order_id,c.attempt,'ecpay_query','ecpay.created','UNKNOWN','CREATED');
    RETURN jsonb_build_object('outcome','trade_found','attempt',c.attempt);
   END IF;
  ELSIF c.state='REQUESTED' THEN RAISE EXCEPTION 'attempt_in_flight' USING ERRCODE='PT409';
  ELSE RAISE EXCEPTION 'not_abandonable' USING ERRCODE='PT422'; END IF;
  UPDATE fulfillment.cvs_shipments SET state='ABANDONED',result_code='ecpay.abandoned',updated_at=v_now,version=version+1
   WHERE tenant_id=c.tenant_id AND store_id=c.store_id AND order_id=c.order_id AND attempt=c.attempt;
  IF c.state='CREATED' THEN
   UPDATE checkout.orders SET fulfillment_state='MANUAL_UNASSIGNED',updated_at=v_now
    WHERE tenant_id=c.tenant_id AND store_id=c.store_id AND id=c.order_id AND fulfillment_state='PROVIDER_LABEL_CREATED';
  END IF;
  INSERT INTO fulfillment.cvs_shipment_events(tenant_id,store_id,order_id,attempt,source,event_code,from_state,to_state)
   VALUES(c.tenant_id,c.store_id,c.order_id,c.attempt,'local','shipment.abandoned',c.state,'ABANDONED');
  INSERT INTO ops.audit_events(tenant_id,store_id,principal_id,action)
   VALUES(c.tenant_id,c.store_id,s.principal_id,'fulfillment.cvs_shipment_abandoned');
  v_response:=jsonb_build_object('attempt',c.attempt,'state','ABANDONED');
  INSERT INTO ops.command_results(tenant_id,store_id,operation,idempotency_key,request_hash,response,principal_id)
   VALUES(s.tenant_id,p_store,'fulfillment.cvs_shipment.abandon',p_key,p_request_hash,v_response,s.principal_id);
 END IF;
 SELECT * INTO v_final FROM identity.resolve_access(p_hash,p_store,'fulfillment:write');
 IF v_final.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF v_final.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF v_final.access_status<>'ok' OR v_final.tenant_id IS DISTINCT FROM s.tenant_id
  OR v_final.principal_id IS DISTINCT FROM s.principal_id OR v_final.authz_revision IS DISTINCT FROM s.authz_revision THEN
  RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 RETURN v_response;
END $$;
ALTER FUNCTION fulfillment.abandon_cvs_shipment(bytea,uuid,uuid,text,bytea,bigint,text,timestamptz,boolean,text,text,text,text) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION fulfillment.abandon_cvs_shipment(bytea,uuid,uuid,text,bytea,bigint,text,timestamptz,boolean,text,text,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION fulfillment.abandon_cvs_shipment(bytea,uuid,uuid,text,bytea,bigint,text,timestamptz,boolean,text,text,text,text) TO commerce_runtime;

-- ---------------------------------------------------------------------------------------------------
-- Reads. Merchant (orders:read, VOLATILE because it settles first) and buyer (STABLE, current attempt only).
-- ---------------------------------------------------------------------------------------------------
CREATE FUNCTION fulfillment.read_cvs_shipment(p_hash bytea,p_store uuid,p_order uuid) RETURNS jsonb
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE s record; v_final record; v_owner uuid; v_last record; v_items jsonb;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR p_order IS NULL
  OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid cvs read' USING ERRCODE='PT400'; END IF;
 SELECT * INTO s FROM identity.resolve_access(p_hash,p_store,'orders:read');
 IF s.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF s.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF s.access_status IS DISTINCT FROM 'ok' THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 PERFORM set_config('app.tenant_id',s.tenant_id::text,true),set_config('app.store_id',p_store::text,true),
  set_config('app.principal_id',s.principal_id::text,true);
 SELECT k.owner_id INTO v_owner FROM checkout.orders k WHERE k.tenant_id=s.tenant_id AND k.store_id=p_store AND k.id=p_order;
 IF NOT FOUND THEN RAISE EXCEPTION 'order not found' USING ERRCODE='PT404'; END IF;
 PERFORM set_config('app.buyer_id',v_owner::text,true);
 PERFORM fulfillment.settle_cvs_attempt(s.tenant_id,p_store,p_order);
 SELECT coalesce(jsonb_agg(jsonb_build_object(
   'attempt',c.attempt,'state',c.state,'subtype',c.logistics_subtype,'environment',c.environment,
   'receiver_store_id',c.receiver_store_id,'goods_amount',c.goods_amount,'collection_amount',c.collection_amount,
   'provider_logistics_id',c.provider_logistics_id,
   -- 7-ELEVEN C2C code = payment no + validation no; others: payment no. Only once a label exists.
   'code',CASE WHEN c.state IN ('CREATED','AT_DC','AT_STORE','PICKED_UP','UNCLAIMED') AND c.cvs_payment_no IS NOT NULL THEN
     CASE WHEN c.logistics_subtype LIKE 'UNIMART%' THEN c.cvs_payment_no||coalesce(c.cvs_validation_no,'') ELSE c.cvs_payment_no END END,
   'print_available',c.state IN ('CREATED','AT_DC','AT_STORE','PICKED_UP','UNCLAIMED') AND c.cvs_payment_no IS NOT NULL
     AND c.logistics_subtype<>'OKMARTC2C',
   'result_code',c.result_code,'last_status_code',c.last_status_code,
   'last_status_at',to_char(c.last_status_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'alerts',coalesce((SELECT jsonb_agg(DISTINCT substr(e.event_code,7)) FROM fulfillment.cvs_shipment_events e
     WHERE e.tenant_id=c.tenant_id AND e.store_id=c.store_id AND e.order_id=c.order_id AND e.attempt=c.attempt
      AND e.event_code LIKE 'alert.%'),'[]'::jsonb),
   'created_at',to_char(c.created_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'updated_at',to_char(c.updated_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),'version',c.version,
   'events',coalesce((SELECT jsonb_agg(jsonb_build_object('source',e.source,'event_code',e.event_code,
      'provider_code',e.provider_code,'provider_message',e.provider_message,'from_state',e.from_state,'to_state',e.to_state,
      'received_at',to_char(e.received_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"')) ORDER BY e.received_at,e.id)
     FROM (SELECT * FROM fulfillment.cvs_shipment_events x WHERE x.tenant_id=c.tenant_id AND x.store_id=c.store_id
       AND x.order_id=c.order_id AND x.attempt=c.attempt ORDER BY x.received_at DESC,x.id DESC LIMIT 100) e),'[]'::jsonb))
   ORDER BY c.attempt),'[]'::jsonb) INTO v_items
  FROM fulfillment.cvs_shipments c WHERE c.tenant_id=s.tenant_id AND c.store_id=p_store AND c.order_id=p_order;
 SELECT x.attempt,x.version,x.logistics_subtype INTO v_last FROM fulfillment.cvs_shipments x
  WHERE x.tenant_id=s.tenant_id AND x.store_id=p_store AND x.order_id=p_order ORDER BY x.attempt DESC LIMIT 1;
 SELECT * INTO v_final FROM identity.resolve_access(p_hash,p_store,'orders:read');
 IF v_final.access_status<>'ok' OR v_final.principal_id IS DISTINCT FROM s.principal_id
  OR v_final.authz_revision IS DISTINCT FROM s.authz_revision THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 RETURN jsonb_build_object('current_attempt',v_last.attempt,'expected_version',coalesce(v_last.version,0),
  'validity_days',fulfillment.ecpay_validity_days(v_last.logistics_subtype),'attempts',v_items);
END $$;
ALTER FUNCTION fulfillment.read_cvs_shipment(bytea,uuid,uuid) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION fulfillment.read_cvs_shipment(bytea,uuid,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION fulfillment.read_cvs_shipment(bytea,uuid,uuid) TO commerce_runtime;

CREATE FUNCTION fulfillment.read_buyer_cvs_shipment(p_buyer_hash bytea,p_store uuid,p_order uuid) RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_scope record; r record;
BEGIN
 IF p_buyer_hash IS NULL OR octet_length(p_buyer_hash)<>32 OR p_store IS NULL OR p_order IS NULL THEN
  RAISE EXCEPTION 'invalid buyer shipment read' USING ERRCODE='PT400'; END IF;
 SELECT * INTO v_scope FROM buyer.resolve_scope(p_buyer_hash,p_store);
 IF NOT FOUND THEN RAISE EXCEPTION 'invalid buyer capability' USING ERRCODE='PT401'; END IF;
 -- Never the trade no, the codes or provider ids: only what the buyer page shows (§5.3).
 SELECT c.state,pv.kind,pv.name,pv.code,c.updated_at INTO r
  FROM fulfillment.cvs_shipments c
  JOIN fulfillment.pickup_versions pv ON pv.tenant_id=c.tenant_id AND pv.store_id=c.store_id AND pv.id=c.pickup_id
  WHERE c.tenant_id=v_scope.tenant_id AND c.store_id=p_store AND c.owner_id=v_scope.owner_id AND c.order_id=p_order
  ORDER BY c.attempt DESC LIMIT 1;
 IF NOT FOUND THEN RETURN NULL; END IF;
 RETURN jsonb_build_object('state',r.state,'chain',r.kind,'store_name',r.name,'store_code',r.code,
  'updated_at',to_char(r.updated_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'));
END $$;
ALTER FUNCTION fulfillment.read_buyer_cvs_shipment(bytea,uuid,uuid) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION fulfillment.read_buyer_cvs_shipment(bytea,uuid,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION fulfillment.read_buyer_cvs_shipment(bytea,uuid,uuid) TO commerce_checkout_runtime;

-- ---------------------------------------------------------------------------------------------------
-- §16.5 store settings and §16.4 collection.
-- ---------------------------------------------------------------------------------------------------
CREATE FUNCTION fulfillment.set_cvs_store_settings(p_hash bytea,p_store uuid,p_key text,p_request_hash bytea,
 p_expected_version bigint,p_enabled_chains text[],p_pay_at_pickup_enabled boolean,p_pay_at_pickup_max_twd integer,
 p_pay_at_pickup_max_open integer)
RETURNS jsonb LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE s record; v_final record; cur record; v_saved bytea; v_response jsonb; v_now timestamptz; v_ver bigint;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR p_key IS NULL OR p_key !~ '^[A-Za-z0-9_.:-]{8,128}$'
  OR p_request_hash IS NULL OR octet_length(p_request_hash)<>32 OR p_expected_version IS NULL OR p_expected_version<0
  OR p_expected_version>=9223372036854775807 OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid cvs settings' USING ERRCODE='PT400'; END IF;
 SELECT * INTO s FROM identity.resolve_access(p_hash,p_store,'integration:manage');
 IF s.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF s.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF s.access_status IS DISTINCT FROM 'ok' THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 PERFORM set_config('app.tenant_id',s.tenant_id::text,true),set_config('app.store_id',p_store::text,true),
  set_config('app.principal_id',s.principal_id::text,true);
 PERFORM pg_advisory_xact_lock(hashtextextended('cvs.settings|'||s.tenant_id||'|'||p_store,0));
 SELECT r.request_hash,r.response INTO v_saved,v_response FROM ops.command_results r WHERE r.tenant_id=s.tenant_id
  AND r.store_id=p_store AND r.operation='fulfillment.cvs_settings.set' AND r.idempotency_key=p_key;
 IF FOUND THEN
  IF v_saved<>p_request_hash THEN RAISE EXCEPTION 'idempotency_conflict' USING ERRCODE='PT409'; END IF;
 ELSE
  -- CHECK-equivalent validation first so a bad body is a 422, never a 500 from a constraint.
  IF p_enabled_chains IS NULL OR p_pay_at_pickup_enabled IS NULL OR NOT p_enabled_chains<@ARRAY['cvs_711','cvs_familymart','cvs_hilife','cvs_okmart']::text[]
   OR array_position(p_enabled_chains,NULL) IS NOT NULL
   OR (SELECT count(DISTINCT x) FROM unnest(p_enabled_chains) x)<>coalesce(array_length(p_enabled_chains,1),0)
   OR (p_pay_at_pickup_max_twd IS NOT NULL AND p_pay_at_pickup_max_twd NOT BETWEEN 1 AND 20000)
   OR p_pay_at_pickup_max_open IS NULL OR p_pay_at_pickup_max_open NOT BETWEEN 1 AND 500
   OR (p_pay_at_pickup_enabled AND p_pay_at_pickup_max_twd IS NULL) THEN
   RAISE EXCEPTION 'invalid_settings' USING ERRCODE='PT422'; END IF;
  SELECT c.version INTO cur FROM fulfillment.cvs_store_settings c WHERE c.tenant_id=s.tenant_id AND c.store_id=p_store FOR UPDATE;
  v_now:=clock_timestamp();
  IF NOT FOUND THEN
   IF p_expected_version<>0 THEN RAISE EXCEPTION 'version_changed' USING ERRCODE='PT409'; END IF;
   v_ver:=1;
   INSERT INTO fulfillment.cvs_store_settings(tenant_id,store_id,enabled_chains,pay_at_pickup_enabled,pay_at_pickup_max_twd,
    pay_at_pickup_max_open,version,updated_at)
   VALUES(s.tenant_id,p_store,p_enabled_chains,p_pay_at_pickup_enabled,p_pay_at_pickup_max_twd,p_pay_at_pickup_max_open,1,v_now);
  ELSE
   IF cur.version<>p_expected_version THEN RAISE EXCEPTION 'version_changed' USING ERRCODE='PT409'; END IF;
   v_ver:=cur.version+1;
   UPDATE fulfillment.cvs_store_settings SET enabled_chains=p_enabled_chains,pay_at_pickup_enabled=p_pay_at_pickup_enabled,
    pay_at_pickup_max_twd=p_pay_at_pickup_max_twd,pay_at_pickup_max_open=p_pay_at_pickup_max_open,version=v_ver,updated_at=v_now
    WHERE tenant_id=s.tenant_id AND store_id=p_store;
  END IF;
  -- A change never touches placed orders (only future begin_hold calls read this row).
  INSERT INTO ops.audit_events(tenant_id,store_id,principal_id,action)
   VALUES(s.tenant_id,p_store,s.principal_id,'fulfillment.cvs_settings_changed');
  v_response:=jsonb_build_object('version',v_ver,'enabled_chains',to_jsonb(p_enabled_chains),
   'pay_at_pickup_enabled',p_pay_at_pickup_enabled,'pay_at_pickup_max_twd',p_pay_at_pickup_max_twd,
   'pay_at_pickup_max_open',p_pay_at_pickup_max_open);
  INSERT INTO ops.command_results(tenant_id,store_id,operation,idempotency_key,request_hash,response,principal_id)
   VALUES(s.tenant_id,p_store,'fulfillment.cvs_settings.set',p_key,p_request_hash,v_response,s.principal_id);
 END IF;
 SELECT * INTO v_final FROM identity.resolve_access(p_hash,p_store,'integration:manage');
 IF v_final.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF v_final.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF v_final.access_status<>'ok' OR v_final.tenant_id IS DISTINCT FROM s.tenant_id
  OR v_final.principal_id IS DISTINCT FROM s.principal_id OR v_final.authz_revision IS DISTINCT FROM s.authz_revision THEN
  RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 RETURN v_response;
END $$;
ALTER FUNCTION fulfillment.set_cvs_store_settings(bytea,uuid,text,bytea,bigint,text[],boolean,integer,integer) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION fulfillment.set_cvs_store_settings(bytea,uuid,text,bytea,bigint,text[],boolean,integer,integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION fulfillment.set_cvs_store_settings(bytea,uuid,text,bytea,bigint,text[],boolean,integer,integer) TO commerce_runtime;

CREATE FUNCTION fulfillment.record_collection(p_hash bytea,p_store uuid,p_order uuid,p_key text,p_request_hash bytea,
 p_expected_state text,p_state text)
RETURNS jsonb LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE s record; v_final record; o record; v_saved bytea; v_response jsonb; v_to text; v_now timestamptz;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR p_order IS NULL OR p_key IS NULL
  OR p_key !~ '^[A-Za-z0-9_.:-]{8,128}$' OR p_request_hash IS NULL OR octet_length(p_request_hash)<>32
  OR p_expected_state IS NULL OR p_expected_state NOT IN ('PENDING','COLLECTED','RETURNED','REFUNDED_OFFLINE','CANCELLED','RESTOCKED')
  OR p_state IS NULL OR p_state NOT IN ('collected','returned','refunded_offline')
  OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid collection record' USING ERRCODE='PT400'; END IF;
 SELECT * INTO s FROM identity.resolve_access(p_hash,p_store,'fulfillment:write');
 IF s.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF s.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF s.access_status IS DISTINCT FROM 'ok' THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 PERFORM set_config('app.tenant_id',s.tenant_id::text,true),set_config('app.store_id',p_store::text,true),
  set_config('app.principal_id',s.principal_id::text,true);
 SELECT k.owner_id,k.payment_mode,k.collection_state,k.fulfillment_state INTO o FROM checkout.orders k
  WHERE k.tenant_id=s.tenant_id AND k.store_id=p_store AND k.id=p_order FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'order not found' USING ERRCODE='PT404'; END IF;
 PERFORM set_config('app.buyer_id',o.owner_id::text,true);
 SELECT r.request_hash,r.response INTO v_saved,v_response FROM ops.command_results r WHERE r.tenant_id=s.tenant_id
  AND r.store_id=p_store AND r.operation='fulfillment.collection.record' AND r.idempotency_key=p_key;
 IF FOUND THEN
  IF v_saved<>p_request_hash THEN RAISE EXCEPTION 'idempotency_conflict' USING ERRCODE='PT409'; END IF;
 ELSE
  IF o.payment_mode<>'pay_at_pickup' THEN RAISE EXCEPTION 'not_pay_at_pickup' USING ERRCODE='PT422'; END IF;
  v_to:=upper(p_state);
  IF p_state IN ('collected','returned') THEN
   IF o.fulfillment_state NOT IN ('MERCHANT_SHIPPED','PROVIDER_LABEL_CREATED') THEN
    RAISE EXCEPTION 'not_shipped' USING ERRCODE='PT422'; END IF;
   IF p_expected_state<>'PENDING' OR o.collection_state<>'PENDING' THEN
    RAISE EXCEPTION 'collection_state_changed' USING ERRCODE='PT409'; END IF;
  ELSE
   IF p_expected_state<>'COLLECTED' OR o.collection_state<>'COLLECTED' THEN
    RAISE EXCEPTION 'collection_state_changed' USING ERRCODE='PT409'; END IF;
  END IF;
  v_now:=clock_timestamp();
  UPDATE checkout.orders SET collection_state=v_to,updated_at=v_now WHERE tenant_id=s.tenant_id AND store_id=p_store AND id=p_order;
  -- §11.5 n/a: no stock, ledger, payment or refund row; the merchant collects through its own channel or ECPay does.
  INSERT INTO ops.audit_events(tenant_id,store_id,principal_id,action)
   VALUES(s.tenant_id,p_store,s.principal_id,'fulfillment.collection_recorded');
  v_response:=jsonb_build_object('order_id',p_order,'collection_state',v_to);
  INSERT INTO ops.command_results(tenant_id,store_id,operation,idempotency_key,request_hash,response,principal_id)
   VALUES(s.tenant_id,p_store,'fulfillment.collection.record',p_key,p_request_hash,v_response,s.principal_id);
 END IF;
 SELECT * INTO v_final FROM identity.resolve_access(p_hash,p_store,'fulfillment:write');
 IF v_final.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF v_final.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF v_final.access_status<>'ok' OR v_final.tenant_id IS DISTINCT FROM s.tenant_id
  OR v_final.principal_id IS DISTINCT FROM s.principal_id OR v_final.authz_revision IS DISTINCT FROM s.authz_revision THEN
  RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 RETURN v_response;
END $$;
ALTER FUNCTION fulfillment.record_collection(bytea,uuid,uuid,text,bytea,text,text) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION fulfillment.record_collection(bytea,uuid,uuid,text,bytea,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION fulfillment.record_collection(bytea,uuid,uuid,text,bytea,text,text) TO commerce_runtime;

-- ---------------------------------------------------------------------------------------------------
-- §16.8 inventory.release_pay_at_pickup: the ONE audited DEALLOCATE writer (X8). resolve_access -> GUCs -> order lock ->
-- buyer GUCs from the order -> replay -> guards -> order update -> reservation -> per-line DEALLOCATE -> audit -> command.
-- ---------------------------------------------------------------------------------------------------
CREATE FUNCTION inventory.release_pay_at_pickup(p_hash bytea,p_store uuid,p_order uuid,p_key text,p_request_hash bytea,
 p_action text,p_expected_state text)
RETURNS jsonb LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE s record; v_final record; o record; r record; l record; v_saved bytea; v_response jsonb; v_op text; v_to text; v_lines integer:=0;
 v_now timestamptz;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR p_order IS NULL OR p_key IS NULL
  OR p_key !~ '^[A-Za-z0-9_.:-]{8,128}$' OR p_request_hash IS NULL OR octet_length(p_request_hash)<>32
  OR p_action IS NULL OR p_action NOT IN ('cancel','restock')
  OR p_expected_state IS NULL OR p_expected_state NOT IN ('PENDING','RETURNED')
  OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid release request' USING ERRCODE='PT400'; END IF;
 -- Authorize before any lock (A2, 0063:299-305); tenant/store/principal GUCs come from this result.
 SELECT * INTO s FROM identity.resolve_access(p_hash,p_store,'fulfillment:write');
 IF s.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF s.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF s.access_status IS DISTINCT FROM 'ok' THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 PERFORM set_config('app.tenant_id',s.tenant_id::text,true),set_config('app.store_id',p_store::text,true),
  set_config('app.principal_id',s.principal_id::text,true);
 SELECT k.owner_id,k.creator_session_id,k.payment_mode,k.commercial_state,k.fulfillment_state,k.collection_state INTO o
  FROM checkout.orders k WHERE k.tenant_id=s.tenant_id AND k.store_id=p_store AND k.id=p_order FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'order not found' USING ERRCODE='PT404'; END IF;
 -- The ledger guard compares the buyer columns with these GUCs (0063:330 pattern): they come from the locked order row.
 PERFORM set_config('app.buyer_id',o.owner_id::text,true),set_config('app.buyer_session_id',o.creator_session_id::text,true);
 SELECT c.request_hash,c.response INTO v_saved,v_response FROM ops.command_results c WHERE c.tenant_id=s.tenant_id
  AND c.store_id=p_store AND c.operation='inventory.pay_at_pickup.release' AND c.idempotency_key=p_key;
 IF FOUND THEN
  IF v_saved<>p_request_hash THEN RAISE EXCEPTION 'idempotency_conflict' USING ERRCODE='PT409'; END IF;
 ELSE
  IF o.payment_mode<>'pay_at_pickup' THEN RAISE EXCEPTION 'not_pay_at_pickup' USING ERRCODE='PT422'; END IF;  -- card orders: RD6
  IF p_action='cancel' AND p_expected_state<>'PENDING' OR p_action='restock' AND p_expected_state<>'RETURNED' THEN
   RAISE EXCEPTION 'invalid release request' USING ERRCODE='PT400'; END IF;
  IF o.collection_state IS DISTINCT FROM p_expected_state THEN
   RAISE EXCEPTION 'collection_state_changed' USING ERRCODE='PT409'; END IF;
  PERFORM fulfillment.settle_cvs_attempt(s.tenant_id,p_store,p_order);
  IF p_action='cancel' THEN
   IF o.commercial_state<>'CONFIRMED' OR o.fulfillment_state<>'MANUAL_UNASSIGNED' THEN
    RAISE EXCEPTION 'not_cancellable' USING ERRCODE='PT422'; END IF;
   -- Not handed to a provider: every attempt FAILED or ABANDONED (REQUESTED/UNKNOWN may already be at ECPay).
   IF EXISTS(SELECT 1 FROM fulfillment.cvs_shipments c WHERE c.tenant_id=s.tenant_id AND c.store_id=p_store AND c.order_id=p_order
     AND c.state IN ('REQUESTED','UNKNOWN')) THEN RAISE EXCEPTION 'cvs_attempt_in_flight' USING ERRCODE='PT409'; END IF;
   IF EXISTS(SELECT 1 FROM fulfillment.cvs_shipments c WHERE c.tenant_id=s.tenant_id AND c.store_id=p_store AND c.order_id=p_order
     AND c.state NOT IN ('FAILED','ABANDONED')) THEN RAISE EXCEPTION 'not_cancellable' USING ERRCODE='PT422'; END IF;
   v_op:='fulfillment.pay_at_pickup.cancel'; v_to:='CANCELLED';
  ELSE
   -- The latest live shipment, if any, must be UNCLAIMED: a 2098 re-delivery puts it back to AT_STORE.
   IF EXISTS(SELECT 1 FROM fulfillment.cvs_shipments c WHERE c.tenant_id=s.tenant_id AND c.store_id=p_store AND c.order_id=p_order
     AND c.state IN ('CREATED','AT_DC','AT_STORE')) THEN RAISE EXCEPTION 'parcel_not_returned' USING ERRCODE='PT409'; END IF;
   v_op:='fulfillment.pay_at_pickup.restock'; v_to:='RESTOCKED';
  END IF;
  v_now:=clock_timestamp();
  IF p_action='cancel' THEN
   UPDATE checkout.orders SET commercial_state='CANCELLED',fulfillment_state='CANCELLED',collection_state='CANCELLED',updated_at=v_now
    WHERE tenant_id=s.tenant_id AND store_id=p_store AND owner_id=o.owner_id AND id=p_order;
  ELSE
   UPDATE checkout.orders SET collection_state='RESTOCKED',updated_at=v_now
    WHERE tenant_id=s.tenant_id AND store_id=p_store AND owner_id=o.owner_id AND id=p_order;
  END IF;
  SELECT x.state INTO r FROM inventory.reservations x WHERE x.tenant_id=s.tenant_id AND x.store_id=p_store AND x.id=p_order FOR UPDATE;
  IF NOT FOUND OR r.state<>'COMMITTED' THEN RAISE EXCEPTION 'collection_state_changed' USING ERRCODE='PT409'; END IF;
  UPDATE inventory.reservations SET state='RELEASED' WHERE tenant_id=s.tenant_id AND store_id=p_store AND id=p_order;
  -- §11.5: evidence = order id + collection_state + actor, all on the ledger row (operation, command_key, reason, principal).
  FOR l IN SELECT x.warehouse_id,x.sku_id,x.quantity FROM inventory.reservation_lines x
   WHERE x.tenant_id=s.tenant_id AND x.store_id=p_store AND x.reservation_id=p_order ORDER BY x.warehouse_id,x.sku_id LOOP
   PERFORM 1 FROM inventory.lock_balance(l.warehouse_id,l.sku_id);
   INSERT INTO inventory.ledger(tenant_id,store_id,warehouse_id,sku_id,kind,delta_allocated,operation,command_key,reservation_id,
    reason,principal_id,checkout_id,buyer_owner_id,buyer_session_id,actor_kind)
   VALUES(s.tenant_id,p_store,l.warehouse_id,l.sku_id,'DEALLOCATE',-l.quantity,v_op,p_order::text,p_order,p_expected_state,
    s.principal_id,p_order,o.owner_id,o.creator_session_id,'MERCHANT');
   v_lines:=v_lines+1;
  END LOOP;
  INSERT INTO ops.audit_events(tenant_id,store_id,principal_id,action)
   VALUES(s.tenant_id,p_store,s.principal_id,CASE p_action WHEN 'cancel' THEN 'fulfillment.pay_at_pickup_cancelled' ELSE 'fulfillment.pay_at_pickup_restocked' END);
  v_response:=jsonb_build_object('order_id',p_order,'collection_state',v_to,
   'commercial_state',CASE WHEN p_action='cancel' THEN 'CANCELLED' ELSE o.commercial_state END,'released_lines',v_lines);
  INSERT INTO ops.command_results(tenant_id,store_id,operation,idempotency_key,request_hash,response,principal_id)
   VALUES(s.tenant_id,p_store,'inventory.pay_at_pickup.release',p_key,p_request_hash,v_response,s.principal_id);
 END IF;
 SELECT * INTO v_final FROM identity.resolve_access(p_hash,p_store,'fulfillment:write');
 IF v_final.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF v_final.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF v_final.access_status<>'ok' OR v_final.tenant_id IS DISTINCT FROM s.tenant_id
  OR v_final.principal_id IS DISTINCT FROM s.principal_id OR v_final.authz_revision IS DISTINCT FROM s.authz_revision THEN
  RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 RETURN v_response;
END $$;
ALTER FUNCTION inventory.release_pay_at_pickup(bytea,uuid,uuid,text,bytea,text,text) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION inventory.release_pay_at_pickup(bytea,uuid,uuid,text,bytea,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION inventory.release_pay_at_pickup(bytea,uuid,uuid,text,bytea,text,text) TO commerce_runtime;

-- ---------------------------------------------------------------------------------------------------
-- Source rows for the two merchant actions that need provider values Go must not derive from the GET projection (no trade no,
-- no separate code parts there): print-form and the Query/V5 before abandon. Same shape as the other merchant definers.
-- ---------------------------------------------------------------------------------------------------
CREATE FUNCTION fulfillment.read_cvs_action_source(p_hash bytea,p_store uuid,p_order uuid,p_purpose text) RETURNS jsonb
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE s record; v_final record; v_owner uuid; c fulfillment.cvs_shipments%ROWTYPE;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR p_order IS NULL OR p_purpose IS NULL
  OR p_purpose NOT IN ('print','abandon') OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid cvs action source' USING ERRCODE='PT400'; END IF;
 SELECT * INTO s FROM identity.resolve_access(p_hash,p_store,'fulfillment:write');
 IF s.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF s.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF s.access_status IS DISTINCT FROM 'ok' THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 PERFORM set_config('app.tenant_id',s.tenant_id::text,true),set_config('app.store_id',p_store::text,true),
  set_config('app.principal_id',s.principal_id::text,true);
 SELECT k.owner_id INTO v_owner FROM checkout.orders k WHERE k.tenant_id=s.tenant_id AND k.store_id=p_store AND k.id=p_order FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'order not found' USING ERRCODE='PT404'; END IF;
 PERFORM set_config('app.buyer_id',v_owner::text,true);
 PERFORM fulfillment.settle_cvs_attempt(s.tenant_id,p_store,p_order);
 SELECT x.* INTO c FROM fulfillment.cvs_shipments x WHERE x.tenant_id=s.tenant_id AND x.store_id=p_store AND x.order_id=p_order
  ORDER BY x.attempt DESC LIMIT 1;
 IF NOT FOUND THEN RAISE EXCEPTION 'no_shipment' USING ERRCODE='PT404'; END IF;
 IF p_purpose='print' THEN
  IF c.state NOT IN ('CREATED','AT_DC','AT_STORE') THEN RAISE EXCEPTION 'not_created' USING ERRCODE='PT422'; END IF;
  IF c.logistics_subtype='OKMARTC2C' OR c.cvs_payment_no IS NULL OR c.provider_logistics_id IS NULL THEN
   RAISE EXCEPTION 'print_unsupported' USING ERRCODE='PT422'; END IF;
  INSERT INTO ops.audit_events(tenant_id,store_id,principal_id,action)
   VALUES(s.tenant_id,p_store,s.principal_id,'fulfillment.cvs_label_printed');
 ELSIF c.state NOT IN ('CREATED','UNKNOWN') THEN
  RAISE EXCEPTION 'not_abandonable' USING ERRCODE='PT422';
 END IF;
 SELECT * INTO v_final FROM identity.resolve_access(p_hash,p_store,'fulfillment:write');
 IF v_final.access_status<>'ok' OR v_final.principal_id IS DISTINCT FROM s.principal_id
  OR v_final.authz_revision IS DISTINCT FROM s.authz_revision THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 RETURN jsonb_build_object('attempt',c.attempt,'version',c.version,'state',c.state,'subtype',c.logistics_subtype,
  'environment',c.environment,'merchant_trade_no',c.merchant_trade_no,'logistics_id',c.provider_logistics_id,
  'payment_no',c.cvs_payment_no,'validation_no',c.cvs_validation_no);
END $$;
ALTER FUNCTION fulfillment.read_cvs_action_source(bytea,uuid,uuid,text) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION fulfillment.read_cvs_action_source(bytea,uuid,uuid,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION fulfillment.read_cvs_action_source(bytea,uuid,uuid,text) TO commerce_runtime;

-- Merchant projections re-derived from 0063 (static copies, PROCESS: never execute mutable pg_get_functiondef); the only body deltas
-- are the C4 keys pickup_source / payment_mode / collection_state, the cvs_pending list filter and the appended export column.
CREATE OR REPLACE FUNCTION identity.export_unshipped_orders(p_hash bytea,p_store uuid,p_row_limit integer)
RETURNS jsonb LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE s record; s2 record; v_rows jsonb; v_err text;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR p_row_limit IS NULL
  OR p_row_limit NOT BETWEEN 1 AND 1001 OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid export request' USING ERRCODE='PT400'; END IF;
 SELECT * INTO s FROM identity.resolve_access(p_hash,p_store,'orders:export');
 IF s.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF s.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF s.access_status IS DISTINCT FROM 'ok' THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 SELECT * INTO s2 FROM identity.resolve_access(p_hash,p_store,'orders:read');
 IF s2.access_status IS DISTINCT FROM 'ok' THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 -- E4: the audit policy compares the GUCs, so they come from the authenticated result, not the caller.
 PERFORM set_config('app.tenant_id',s.tenant_id::text,true),set_config('app.store_id',p_store::text,true),
  set_config('app.principal_id',s.principal_id::text,true);
 SELECT coalesce(jsonb_agg(x.j ORDER BY x.created_at,x.id),'[]'::jsonb) INTO v_rows FROM (
  SELECT o.created_at,o.id,jsonb_build_object(
   'order_id',o.id,'created_at_utc',to_char(o.created_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"'),
   'service_code',o.service_code,'destination_kind',coalesce(o.snapshot#>>'{destination,kind}',''),
   'recipient_name',coalesce(o.snapshot#>>'{destination,recipient_name}',''),
   'phone',coalesce(o.snapshot#>>'{destination,phone}',''),'country',o.country,
   'region',coalesce(o.snapshot#>>'{destination,home_address,region}',''),
   'city',coalesce(o.snapshot#>>'{destination,home_address,city}',''),
   'postal_code',coalesce(o.snapshot#>>'{destination,home_address,postal_code}',''),
   'line1',coalesce(o.snapshot#>>'{destination,home_address,line1}',''),
   'line2',coalesce(o.snapshot#>>'{destination,home_address,line2}',''),
   'pickup_namespace',coalesce(o.snapshot#>>'{destination,pickup,namespace}',''),
   'pickup_code',coalesce(o.snapshot#>>'{destination,pickup,code}',''),
   'pickup_name',coalesce(o.snapshot#>>'{destination,pickup,name}',''),
   'pickup_address',coalesce(o.snapshot#>>'{destination,pickup,address}',''),
   'items',coalesce((SELECT jsonb_agg(jsonb_build_object('code',line->'code','quantity',line->'quantity') ORDER BY position)
      FROM jsonb_array_elements(o.snapshot#>'{quote,lines}') WITH ORDINALITY AS item(line,position)),'[]'::jsonb),
   'total_minor',o.total_minor,'currency',o.currency,
   -- C4 / §16.1: the merchant sees how the store was obtained; appended as the LAST CSV column (ruling B19).
   'pickup_source',CASE o.snapshot#>>'{destination,pickup,verification_kind}' WHEN 'PROVIDER_DIRECTORY_VERIFIED' THEN 'ecpay_directory'
     WHEN 'BUYER_ENTERED' THEN 'buyer_entered' WHEN 'MANUAL_ATTESTED' THEN 'merchant_attested' ELSE '' END) AS j
  FROM checkout.orders o
  WHERE o.tenant_id=s.tenant_id AND o.store_id=p_store AND o.commercial_state='CONFIRMED'
   AND o.fulfillment_state='MANUAL_UNASSIGNED'
   AND fulfillment.manual_shipment_eligible(o.tenant_id,o.store_id,o.id)
  ORDER BY o.created_at ASC,o.id ASC LIMIT p_row_limit) x;
 v_err:=identity.merchant_access_denied(p_hash,p_store,ARRAY['orders:export','orders:read'],s.tenant_id,s.principal_id,s.authz_revision);
 IF v_err IS NOT NULL THEN RAISE EXCEPTION 'export access denied' USING ERRCODE=v_err; END IF;
 IF octet_length(v_rows::text)>8388608 THEN RAISE EXCEPTION 'export unavailable' USING ERRCODE='PT503'; END IF;
 INSERT INTO ops.audit_events(tenant_id,store_id,principal_id,action)
 VALUES(s.tenant_id,p_store,s.principal_id,'orders.export_unshipped');
 RETURN v_rows;
END $$;

CREATE OR REPLACE FUNCTION identity.read_merchant_orders(p_hash bytea,p_store uuid,p_order uuid,
 p_limit integer,p_after_created_at timestamptz,p_after_id uuid,p_state text)
RETURNS jsonb LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE s record; v_result jsonb; v_auth_error text;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL
 OR p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 101
 OR p_state IS NULL OR p_state NOT IN ('all','DRAFT','AWAITING_PAYMENT','CONFIRMED','CANCELLED','shipped','unshipped','cvs_pending')
 OR (p_after_created_at IS NULL)<>(p_after_id IS NULL)
 OR (p_after_created_at IS NOT NULL AND NOT isfinite(p_after_created_at))
 OR (p_order IS NOT NULL AND (p_limit<>1 OR p_after_id IS NOT NULL OR p_state<>'all'))
 OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid order read' USING ERRCODE='PT400'; END IF;
 SELECT * INTO s FROM identity.resolve_access(p_hash,p_store,'orders:read');
 IF s.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF s.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF s.access_status IS DISTINCT FROM 'ok' THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 IF current_setting('app.tenant_id',true) IS DISTINCT FROM s.tenant_id::text
 OR current_setting('app.store_id',true) IS DISTINCT FROM p_store::text
 OR current_setting('app.principal_id',true) IS DISTINCT FROM s.principal_id::text THEN
  RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;

 -- ponytail: one bounded projection, no duplicate order engine. All data is
 -- derived in ONE statement snapshot, so capture cannot split state/facts/work.
 WITH owned AS MATERIALIZED (
  SELECT o.id,o.tenant_id,o.store_id,o.owner_id,o.created_at,o.updated_at,o.currency,
   o.total_minor,o.commercial_state,o.fulfillment_state,o.country,o.service_code,
   o.payment_mode,o.collection_state,o.snapshot#>>'{destination,pickup,verification_kind}' AS pickup_vk,
   CASE WHEN p_order IS NULL THEN NULL ELSE o.snapshot END AS snapshot
  FROM checkout.orders o WHERE o.tenant_id=s.tenant_id AND o.store_id=p_store
   AND (p_order IS NULL OR o.id=p_order)
   AND (p_state='all'
    OR (p_state IN ('DRAFT','AWAITING_PAYMENT','CONFIRMED','CANCELLED') AND o.commercial_state=p_state)
    OR (p_state='shipped' AND o.fulfillment_state='MERCHANT_SHIPPED')
    -- MD6: the same predicate the record command and the export use.
    OR (p_state='unshipped' AND o.commercial_state='CONFIRMED' AND o.fulfillment_state='MANUAL_UNASSIGNED'
      AND fulfillment.manual_shipment_eligible(o.tenant_id,o.store_id,o.id))
    -- C4: the forwarder's daily drop list = PROVIDER_LABEL_CREATED with the current attempt CREATED.
    OR (p_state='cvs_pending' AND o.fulfillment_state='PROVIDER_LABEL_CREATED' AND EXISTS(SELECT 1 FROM fulfillment.cvs_shipments cs
      WHERE cs.tenant_id=o.tenant_id AND cs.store_id=o.store_id AND cs.order_id=o.id AND cs.state='CREATED')))
   AND (p_after_id IS NULL OR (o.created_at,o.id)<(p_after_created_at,p_after_id))
  ORDER BY o.created_at DESC,o.id DESC LIMIT p_limit
 ), projected AS (
  SELECT o.created_at,o.id,
  CASE WHEN a.id IS NOT NULL AND (a.currency<>o.currency OR a.amount_minor<>o.total_minor)
   THEN NULL ELSE jsonb_build_object(
   'order_id',o.id,'created_at',to_char(o.created_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'updated_at',to_char(o.updated_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'currency',o.currency,'total_minor',o.total_minor,'commercial_state',o.commercial_state,
   'fulfillment_state',o.fulfillment_state,
   -- Precedence NOT_STARTED > REVIEW_REQUIRED > REFUNDED > PARTIALLY_REFUNDED > CAPTURED > AUTHORIZED > PENDING.
   'payment_state',CASE WHEN a.id IS NULL THEN 'NOT_STARTED'
     WHEN EXISTS(SELECT 1 FROM payments.review_cases rc WHERE rc.tenant_id=a.tenant_id
       AND rc.store_id=a.store_id AND rc.attempt_id=a.id) THEN 'REVIEW_REQUIRED'
     WHEN f.captured AND rf.refunded_minor>0 AND rf.refunded_minor>=o.total_minor THEN 'REFUNDED'
     WHEN f.captured AND rf.refunded_minor>0 THEN 'PARTIALLY_REFUNDED'
     WHEN f.captured THEN 'CAPTURED' WHEN f.authorized THEN 'AUTHORIZED' ELSE 'PENDING' END,
   'test_mode',CASE WHEN a.id IS NULL THEN false ELSE a.execution_profile<>'LIVE' OR a.environment<>'LIVE' END,
   'work_state',coalesce(w.state,'NONE'),
   -- C4 / §16.1 / §16.2: how the store was obtained, how it is paid, and where the pay-at-pickup collection stands.
   'pickup_source',CASE o.pickup_vk WHEN 'PROVIDER_DIRECTORY_VERIFIED' THEN 'ecpay_directory' WHEN 'BUYER_ENTERED' THEN 'buyer_entered'
     WHEN 'MANUAL_ATTESTED' THEN 'merchant_attested' END,'payment_mode',o.payment_mode,'collection_state',o.collection_state,
   'refunded_minor',coalesce(rf.refunded_minor,0),'refund_pending_minor',coalesce(rf.pending_minor,0)) ||
  CASE WHEN p_order IS NULL THEN '{}'::jsonb ELSE jsonb_build_object(
   'country',o.country,'service_code',o.service_code,
   'items',(SELECT jsonb_agg(jsonb_build_object('sku_id',line->'sku_id','code',line->'code',
     'name',line->'name','quantity',line->'quantity','unit_price_minor',line->'unit_price_minor',
     'amount',jsonb_build_object('subtotal_minor',line#>'{amount,subtotal_minor}',
       'discount_minor',line#>'{amount,discount_minor}','tax_minor',line#>'{amount,tax_minor}',
       'total_minor',line#>'{amount,total_minor}')) ORDER BY position)
     FROM jsonb_array_elements(o.snapshot#>'{quote,lines}') WITH ORDINALITY AS item(line,position)),
   'totals',jsonb_build_object('subtotal_minor',o.snapshot#>'{quote,amount,subtotal_minor}',
     'discount_minor',o.snapshot#>'{quote,amount,discount_minor}',
     'shipping_minor',o.snapshot#>'{quote,amount,shipping_minor}',
     'shipping_tax_minor',o.snapshot#>'{quote,amount,shipping_tax_minor}',
     'tax_minor',o.snapshot#>'{quote,amount,tax_minor}','total_minor',o.snapshot#>'{quote,amount,total_minor}'),
   'destination',jsonb_build_object('kind',o.snapshot#>'{destination,kind}',
     'country',o.snapshot#>'{destination,country}','recipient_name',o.snapshot#>'{destination,recipient_name}',
     'phone',o.snapshot#>'{destination,phone}',
     'home_address',jsonb_build_object('region',o.snapshot#>'{destination,home_address,region}',
       'city',o.snapshot#>'{destination,home_address,city}','postal_code',o.snapshot#>'{destination,home_address,postal_code}',
       'line1',o.snapshot#>'{destination,home_address,line1}','line2',o.snapshot#>'{destination,home_address,line2}'),
     'pickup',CASE WHEN o.snapshot#>'{destination,pickup}' IS NULL
        OR o.snapshot#>'{destination,pickup}'='null'::jsonb THEN NULL ELSE
       jsonb_build_object('kind',o.snapshot#>'{destination,pickup,kind}',
        'namespace',o.snapshot#>'{destination,pickup,namespace}','code',o.snapshot#>'{destination,pickup,code}',
        'name',o.snapshot#>'{destination,pickup,name}','address',o.snapshot#>'{destination,pickup,address}',
        'verification_kind',o.snapshot#>'{destination,pickup,verification_kind}') END),
   -- Current head, same snapshot; only a SHIPPED head is shown (a voided head is null).
   'shipment',CASE WHEN sh.version IS NULL THEN NULL ELSE jsonb_build_object('version',sh.version,
     'status',sh.status,'carrier_code',sh.carrier_code,'carrier_name',sh.carrier_name,
     'tracking_number',sh.tracking_number,'tracking_url',sh.tracking_url,
     'recorded_at',to_char(sh.recorded_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"')) END) END END AS value
  FROM owned o LEFT JOIN checkout.payment_attempts a ON a.tenant_id=o.tenant_id
   AND a.store_id=o.store_id AND a.owner_id=o.owner_id AND a.order_id=o.id
  LEFT JOIN LATERAL (
   SELECT bool_or(fact.kind='CAPTURED') AS captured,bool_or(fact.kind='AUTHORIZED') AS authorized
    FROM payments.facts fact WHERE fact.tenant_id=a.tenant_id AND fact.store_id=a.store_id
     AND fact.attempt_id=a.id AND fact.connection_id=a.connection_id
     AND fact.execution_profile=a.execution_profile AND fact.environment=a.environment
     AND fact.currency=a.currency AND fact.amount_minor=a.amount_minor
     AND fact.currency=o.currency AND fact.amount_minor=o.total_minor
  ) f ON true
  -- Refund amounts (stripe-refund-v1 §7.1): held = no FAILED/CANCELED/REJECTED fact; refunded = held with a
  -- SUCCEEDED fact; pending = held without one. A late FAILED/CANCELED after SUCCEEDED leaves both.
  LEFT JOIN LATERAL (
   SELECT coalesce(sum(r.amount_minor) FILTER (WHERE r.held AND r.succeeded),0)::bigint AS refunded_minor,
    coalesce(sum(r.amount_minor) FILTER (WHERE r.held AND NOT r.succeeded),0)::bigint AS pending_minor
    FROM (SELECT x.amount_minor,
      NOT EXISTS(SELECT 1 FROM payments.refund_facts rfx WHERE rfx.tenant_id=x.tenant_id AND rfx.store_id=x.store_id
        AND rfx.refund_id=x.id AND rfx.kind IN ('FAILED','CANCELED','REJECTED')) AS held,
      EXISTS(SELECT 1 FROM payments.refund_facts rfs WHERE rfs.tenant_id=x.tenant_id AND rfs.store_id=x.store_id
        AND rfs.refund_id=x.id AND rfs.kind='SUCCEEDED') AS succeeded
      FROM payments.stripe_refunds x WHERE x.tenant_id=a.tenant_id AND x.store_id=a.store_id AND x.attempt_id=a.id) r
  ) rf ON true
  LEFT JOIN fulfillment.payment_work_items w ON w.tenant_id=o.tenant_id AND w.store_id=o.store_id
   AND w.owner_id=o.owner_id AND w.order_id=o.id AND w.attempt_id=a.id
  LEFT JOIN LATERAL (
   SELECT v.version,v.status,v.carrier_code,v.carrier_name,v.tracking_number,v.tracking_url,v.recorded_at
    FROM fulfillment.manual_shipment_heads h JOIN fulfillment.manual_shipment_versions v
     ON v.tenant_id=h.tenant_id AND v.store_id=h.store_id AND v.order_id=h.order_id AND v.version=h.current_version
    WHERE h.tenant_id=o.tenant_id AND h.store_id=o.store_id AND h.order_id=o.id AND v.status='SHIPPED'
  ) sh ON true
 )
 SELECT coalesce(jsonb_agg(value ORDER BY created_at DESC,id DESC),'[]'::jsonb) INTO v_result FROM projected;

 -- A nested STABLE resolve_access retains the outer statement's snapshot/time.
 -- This direct SELECT is a fresh VOLATILE inner statement after any data wait.
 -- Fence BEFORE empty/not-found branches: no post-revocation existence oracle.
 SELECT CASE WHEN p.id IS NULL THEN 'PT401'
   WHEN st.id IS NULL OR t.id IS NULL OR m.principal_id IS NULL OR gr.permission IS NULL THEN 'PT404'
   WHEN og.permission IS NULL OR st.tenant_id<>s.tenant_id OR p.id<>s.principal_id
     OR m.authz_revision<>s.authz_revision THEN 'PT403' ELSE NULL END INTO v_auth_error
 FROM (VALUES(1)) gate(n)
 LEFT JOIN identity.sessions login ON login.token_hash=p_hash AND login.audience='merchant'
   AND login.revoked_at IS NULL AND login.expires_at>clock_timestamp()
 LEFT JOIN identity.principals p ON p.id=login.principal_id AND p.active
 LEFT JOIN control.stores st ON st.id=p_store AND st.active
 LEFT JOIN control.tenants t ON t.id=st.tenant_id AND t.active
 LEFT JOIN identity.memberships m ON m.tenant_id=st.tenant_id AND m.principal_id=p.id AND m.active
 LEFT JOIN identity.store_grants gr ON gr.tenant_id=st.tenant_id AND gr.store_id=st.id
   AND gr.principal_id=p.id AND gr.permission='store:read'
 LEFT JOIN identity.store_grants og ON og.tenant_id=st.tenant_id AND og.store_id=st.id
   AND og.principal_id=p.id AND og.permission='orders:read';
 IF v_auth_error IS NOT NULL THEN RAISE EXCEPTION 'order read access denied' USING ERRCODE=v_auth_error; END IF;
 IF p_order IS NOT NULL AND jsonb_array_length(v_result)=0 THEN
  RAISE EXCEPTION 'order not found' USING ERRCODE='PT404'; END IF;
 IF octet_length(v_result::text)>(CASE WHEN p_order IS NULL THEN 131072 ELSE 262144 END) THEN
  RAISE EXCEPTION 'order read unavailable' USING ERRCODE='PT503'; END IF;
 RETURN v_result;
END $$;

-- ---------------------------------------------------------------------------------------------------
-- Documentation (PROCESS §5): owning package, allowed roles, non-goals, for every object of this file.
-- ---------------------------------------------------------------------------------------------------
COMMENT ON FUNCTION fulfillment.ecpay_recipient_ok(text,text) IS 'internal/fulfillment (CVS) pure rule of §4.3/C3: real recipient name (no digits/symbols/emoji, width 4..10, CJK counts 2) and a Taiwan mobile ^09[0-9]{8}$ (+8869... normalised). IMMUTABLE, locale-independent. Go twin ecpay.RecipientOK. Non-goal: authorizes nobody, stores nothing.';
COMMENT ON FUNCTION fulfillment.ecpay_trade_no(uuid) IS 'internal/fulfillment (CVS) C2: LC + base32(sha256(operation uuid text))[:18], the ECPay MerchantTradeNo derived from the operation id (I06). Go twin ecpay.MerchantTradeNo.';
COMMENT ON FUNCTION fulfillment.ecpay_validity_days(text) IS 'internal/fulfillment (CVS) C5/F9: ECPay order validity in calendar days by subtype (7-ELEVEN 5, FamilyMart 6, Hi-Life/OK 7); used by abandon_cvs_shipment and the merchant projection.';
COMMENT ON FUNCTION fulfillment.ecpay_created_only_codes() IS 'internal/fulfillment (CVS) C5/F19: LogisticsStatus codes proving a parcel has not moved; extended only with TCV07/TCV12 evidence.';
COMMENT ON FUNCTION fulfillment.ecpay_connection_id(uuid,uuid,text) IS 'internal/fulfillment (CVS): deterministic ecpay_logistics connection id per (tenant, store, environment), so Go can bind the AEAD AAD before the first insert. Go twin fulfillment.ConnectionID. Not a secret, not an authorization.';
COMMENT ON FUNCTION integration.register_ecpay_logistics(bytea,uuid,text,bytea,bigint,text,text,text,text,bytea,bytea,boolean,text) IS 'internal/fulfillment CVS.Connect only; EXECUTE commerce_runtime. integration:manage with fresh final auth; environment pinned to the deployment payment environment (PT422 ecpay_environment_not_allowed); expected_version 0 inserts binding+account+credential+profile, else appends credential version+1 (credential version = profile version, so Go can bind the AAD). probe_ok must be true. Never returns key material.';
COMMENT ON FUNCTION integration.set_ecpay_logistics_enabled(bytea,uuid,text,bytea,bigint,boolean) IS 'internal/fulfillment CVS.Enable only; EXECUTE commerce_runtime. integration:manage, profile version CAS; enabling needs the credential to be qualified (PT422 not_qualified). Disabling stops new selections and creates; status ingress and query continue.';
COMMENT ON FUNCTION fulfillment.read_ecpay_logistics(bytea,uuid) IS 'internal/fulfillment CVS.Profile only; EXECUTE commerce_runtime. integration:read; the enabled profile else the newest; never keys or sender data.';
COMMENT ON FUNCTION fulfillment.read_cvs_settings(bytea,uuid) IS 'internal/fulfillment CVS.Settings only; EXECUTE commerce_runtime. integration:read; no row = the §16.5 defaults at version 0 (C8).';
COMMENT ON FUNCTION integration.load_ecpay_key_for_status(uuid) IS 'internal/fulfillment status route only; EXECUTE commerce_runtime. Current credential of the ONE connection named by the endpoint id (any enabled state); decryption happens in Go.';
COMMENT ON FUNCTION integration.load_ecpay_key_for_selection(uuid) IS 'internal/fulfillment map-return route (commerce_runtime) and buyer verify retry (commerce_checkout_runtime, after read_cvs_selection); only OPEN/RETURNED unexpired selections; current credential of the selection connection.';
COMMENT ON FUNCTION integration.load_ecpay_key_for_merchant(bytea,uuid) IS 'internal/fulfillment print-form and abandon; EXECUTE commerce_runtime. fulfillment:write with fresh auth; the store''s enabled connection else its newest.';
COMMENT ON FUNCTION fulfillment.open_cvs_selection(bytea,uuid,text,bytea,bigint,uuid,text,bytea,text,text,text) IS 'internal/checkout BuyerCVS.Open only; EXECUTE commerce_checkout_runtime (checkout pool, X9). Buyer scope first; return_origin must be an ACTIVE published domain of the store; the profile environment must equal p_payment_environment; <=10 OPEN per owner/cart (PT429). A replay is PT409 selection_replay_new_key (the nonce is not re-derivable).';
COMMENT ON FUNCTION fulfillment.record_cvs_map_return(uuid,bytea,text,text,text,text) IS 'internal/fulfillment map-return route only; EXECUTE commerce_runtime (unauthenticated). Check order is contract: nonce digest first (wrong nonce writes nothing), then expiry, merchant, subtype, store id shape. Returns the STORED return_origin/return_path, never a body value.';
COMMENT ON FUNCTION fulfillment.verify_cvs_selection(uuid,text,boolean,text,text,timestamptz) IS 'internal/fulfillment map-return route (commerce_runtime) and internal/checkout BuyerCVS.Verify (commerce_checkout_runtime, after read_cvs_selection). Writes or reuses the directory-verified pickup source (namespace ecpay.<env>.<subtype>, same advisory key as AttestPickup); store codes longer than 6 are REJECTED store_code_length.';
COMMENT ON FUNCTION fulfillment.cvs_selection_projection(uuid) IS 'fulfillment helper (no caller EXECUTE): the selection projection shared by verify_cvs_selection and read_cvs_selection. DEFINER commerce_checkout_writer.';
COMMENT ON FUNCTION fulfillment.read_cvs_selection(bytea,uuid,uuid) IS 'internal/checkout BuyerCVS.Get/Verify only; EXECUTE commerce_checkout_runtime. Owner + session + cart must match the buyer scope; another buyer''s selection is an indistinguishable PT404.';
COMMENT ON FUNCTION fulfillment.record_buyer_cvs_store(bytea,uuid,text,bytea,bigint,uuid,text,text,text,text) IS 'internal/checkout BuyerCVS.EnterStore only; EXECUTE commerce_checkout_runtime. §16.1: buyer-entered store (BUYER_ENTERED, NULL principal, namespace buyer.<owner>), per-chain code format, refused when the store is in ecpay_map mode; <=20 per owner per hour (PT429). Never labelled verified.';
COMMENT ON FUNCTION fulfillment.read_cvs_offer(bytea,uuid) IS 'internal/checkout options only; STABLE, EXECUTE commerce_checkout_runtime (the options login is not a commerce_runtime member). Mode ecpay_map iff the store has an enabled qualified profile, else buyer_entered; asserts the caller GUCs equal the buyer scope.';
COMMENT ON FUNCTION fulfillment.order_money_shippable(uuid,uuid,uuid) IS 'internal/fulfillment: the MD6 payment/review/refund clauses of 0063 (card) plus the pay_at_pickup branch (§16.2). SECURITY INVOKER: runs with the calling definer''s grants (commerce_checkout_writer, commerce_auth). Non-goal: does not authorize anyone or check fulfillment state.';
COMMENT ON FUNCTION fulfillment.manual_shipment_eligible(uuid,uuid,uuid) IS 'MD6 shipping eligibility (0063, replaced in 0073): order_money_shippable AND CONFIRMED + MANUAL_UNASSIGNED AND no live ECPay attempt. Single source for record_manual_shipment, request_cvs_shipment, the list filter and the export.';
COMMENT ON FUNCTION fulfillment.cvs_order_payable(uuid,uuid,uuid) IS 'internal/integrations shipping route via integration.load_cvs_create only; EXECUTE commerce_integration_writer. Dispatch-time money check (refund/review committed after the request stops the label purchase); takes the order row FOR SHARE so an uncommitted refund is waited for; boolean only; resets the scope GUCs it set.';
COMMENT ON FUNCTION fulfillment.settle_cvs_attempt(uuid,uuid,uuid) IS 'fulfillment helper (no caller EXECUTE): settles a latest REQUESTED/UNKNOWN attempt from its operation for gaps no dispatcher completion reaches (claim-side STALE_BINDING, exhausted budget, stale lease > 1 h). Idempotent; callers hold the tenant/store GUCs.';
COMMENT ON FUNCTION integration.plan_cvs_create(uuid,uuid,uuid,smallint,uuid,uuid,uuid,bigint) IS 'fulfillment.request_cvs_shipment only; EXECUTE commerce_checkout_writer. Verifies the River job is this transaction''s external_operation_v1 row, locks the binding FOR SHARE and inserts the READY ecpay.cvs_create operation (request = {order_id, attempt} only, TD8) plus its event. No integration:execute, no core.Service.Plan.';
COMMENT ON FUNCTION fulfillment.request_cvs_shipment(bytea,uuid,uuid,text,bytea,bigint,uuid,bigint) IS 'internal/fulfillment CVS.Request only; EXECUTE commerce_runtime. fulfillment:write via resolve_access before any lock; a replayed key RAISEs PT2RP (Go rolls back its job). Freezes connection, credential version, environment, subtype, store, amounts (I05: server-derived TWD), trade no. Errors PT422 not_shippable|no_cvs_destination|connection_unavailable|cvs_environment_mismatch|cvs_amount_exceeds|cvs_recipient_rejected, PT409 version_changed.';
COMMENT ON FUNCTION fulfillment.read_cvs_shipment_command(bytea,uuid,text,bytea) IS 'internal/fulfillment CVS.Request PT2RP path only; EXECUTE commerce_runtime. Writes nothing; returns the stored body or PT409 idempotency_conflict.';
COMMENT ON FUNCTION integration.load_cvs_create(uuid,bigint,bytea,text) IS 'cmd/claims-worker ecpayroute LoadSecret only (commerce_worker). Lease-fenced; frozen shipment fields and frozen credential version; dispatch mode also requires REQUESTED + enabled qualified profile + payable order (PT409 = policy refusal); recipient name/phone only in dispatch mode (TD8). 40001 = lease fence.';
COMMENT ON FUNCTION fulfillment.apply_cvs_create_result(uuid,uuid,uuid,smallint,uuid,text,text,text,text,text,text,text) IS 'integration.finish_cvs_create only; EXECUTE commerce_integration_writer. Every fulfillment write of a create/query outcome; total over provider data (non-conforming codes stored NULL + ecpay.code_nonconforming event); idempotent; a late result for FAILED/ABANDONED is event + alert only.';
COMMENT ON FUNCTION integration.finish_cvs_create(uuid,bigint,bytea,text,text,text,text,text,text,text) IS 'cmd/claims-worker ecpayroute Finish hook only (commerce_worker), inside completeOperation''s tx before integration.complete_operation. Lease-fenced; delegates to fulfillment.apply_cvs_create_result and writes no fulfillment table itself.';
COMMENT ON FUNCTION fulfillment.ingest_ecpay_status(uuid,bytea,text,text,text,text,text,text,text,text) IS 'internal/fulfillment status route only; EXECUTE commerce_runtime (unauthenticated; Go verified CheckMacValue first). Returns applied|duplicate|retry|ignored; never raises on vendor data; exact §6 code table; pay_at_pickup collection moves with 2067/3022 and 2074/3020 (audited).';
COMMENT ON FUNCTION fulfillment.abandon_cvs_shipment(bytea,uuid,uuid,text,bytea,bigint,text,timestamptz,boolean,text,text,text,text) IS 'internal/fulfillment CVS.Abandon only; EXECUTE commerce_runtime. fulfillment:write; settles first; CREATED needs lapse + no status movement + a fresh created-only Query/V5; UNKNOWN needs acknowledgement and a finished dispatch, and a found trade is applied as CREATED (returned outcome trade_found). Never calls ECPay.';
COMMENT ON FUNCTION fulfillment.read_cvs_action_source(bytea,uuid,uuid,text) IS 'internal/fulfillment CVS.PrintForm and CVS.Abandon only; EXECUTE commerce_runtime. fulfillment:write; settles first; returns the latest attempt''s provider values (trade no, codes) that the GET projection deliberately omits; purpose print refuses OK mart / missing code (PT422 print_unsupported) and audits fulfillment.cvs_label_printed. Never calls ECPay.';
COMMENT ON FUNCTION fulfillment.read_cvs_shipment(bytea,uuid,uuid) IS 'internal/fulfillment CVS.Shipment only; EXECUTE commerce_runtime. orders:read; VOLATILE because it settles first; all attempts and events, no PII, no trade no.';
COMMENT ON FUNCTION fulfillment.read_buyer_cvs_shipment(bytea,uuid,uuid) IS 'internal/checkout Get only; STABLE, EXECUTE commerce_checkout_runtime. Current attempt {state, chain, store_name, store_code, updated_at} only; never trade no, codes or provider ids.';
COMMENT ON FUNCTION fulfillment.set_cvs_store_settings(bytea,uuid,text,bytea,bigint,text[],boolean,integer,integer) IS 'internal/fulfillment CVS.SetSettings only; EXECUTE commerce_runtime. integration:manage, version CAS (0 inserts); a change never touches placed orders.';
COMMENT ON FUNCTION fulfillment.record_collection(bytea,uuid,uuid,text,bytea,text,text) IS 'internal/fulfillment CVS.RecordCollection only; EXECUTE commerce_runtime. fulfillment:write; manual collected/returned/refunded_offline of a pay_at_pickup order; no money movement, no ledger row.';
COMMENT ON FUNCTION inventory.release_pay_at_pickup(bytea,uuid,uuid,text,bytea,text,text) IS 'internal/fulfillment CVS.Release only; EXECUTE commerce_runtime. §16.8: the ONLY writer of DEALLOCATE ledger rows; cancel (PENDING, unshipped) and restock (RETURNED) release the order allocation for exactly the allocated quantity; §11.5 evidence = order id + collection_state + actor on the ledger row; no payments/refund/operation/river row.';

-- Every policy, grant-matrix or 0072 policy created by this migration pair is described from the catalog (role, command, table) so no
-- policy is undocumented; the text names the contract section that owns it.
DO $$
DECLARE p record;
BEGIN
 FOR p IN SELECT pol.polname,pol.polrelid::regclass::text AS rel,
   CASE pol.polcmd WHEN 'r' THEN 'SELECT' WHEN 'a' THEN 'INSERT' WHEN 'w' THEN 'UPDATE' WHEN 'd' THEN 'DELETE' ELSE 'ALL' END AS cmd,
   (SELECT string_agg(r::regrole::text,',') FROM unnest(pol.polroles) r) AS roles
  FROM pg_policy pol WHERE pol.polname IN ('cvs_profile_writer_read','cvs_profile_writer_lock','cvs_selection_writer','cvs_shipment_writer',
   'cvs_event_writer','cvs_settings_rw','cvs_settings_rw_insert','cvs_settings_rw_update','cvs_ecpay_pickup_version_insert',
   'cvs_buyer_pickup_version_insert','cvs_ecpay_pickup_head_insert','cvs_buyer_pickup_head_insert','cvs_ecpay_pickup_head_update',
   'cvs_buyer_pickup_head_update','checkout_cvs_operation_read','cvs_domain_writer_read','cvs_publication_writer_read','cvs_receipt_read',
   'cvs_receipt_insert','cvs_shipment_auth_read','checkout_writer_pay_at_pickup_release','cvs_profile_integration_read',
   'cvs_profile_integration_insert','cvs_profile_integration_update','ecpay_registrar_account_insert','ecpay_registrar_account_update',
   'ecpay_registrar_credential_insert','ecpay_registrar_binding_insert','ecpay_registrar_command_read','ecpay_registrar_command_insert',
   'ecpay_registrar_audit','cvs_create_insert','cvs_shipment_integration_read','cvs_selection_integration_read','cvs_order_snapshot_integration') LOOP
  EXECUTE format('COMMENT ON POLICY %I ON %s IS %L',p.polname,p.rel,
   '0073 CVS grant matrix (contract taiwan-cvs-logistics-v1 §4.3/§16): '||p.cmd||' on '||p.rel||' for '||p.roles||'; reached only through the CVS definers; no runtime or buyer role gets this access.');
 END LOOP;
END $$;
