-- Post-River 0017 checkout.begin_hold, 10 arguments (contracts/taiwan-cvs-logistics-v1.md §4.3 begin_hold row, §16.2, R-6, rulings
-- X5/X9; docs/delivery/units/cvs-core.md default C1). Applied AFTER post_river/0005 on every fresh install and upgrade (migrate.go runs
-- all main migrations, then River, then post_river), so the 8-argument body of 0005:98 is DROPPED here and the new signature is the
-- only checkout.begin_hold. Same owner and EXECUTE grant as 0013:606-611.
--
-- Owns: buyer checkout begin (order + stock hold), now also (a) directory-verified and buyer-entered CVS pickup sources,
-- (b) API delivery services bound to the store's ECPay profile, (c) ECPay amount/recipient/environment guards, (d) the
-- pay_at_pickup payment mode (order CONFIRMED at placement, HELD -> COMMITTED, no payment attempt, no Stripe session).
-- Non-goals: no ECPay call, no River job insert (Go inserts the expiry job first, exactly as before), no price computation
-- (the server quote is the only monetary authority), no payment start (checkout.start_payment / start_stripe_payment refuse a
-- non-DRAFT order).
-- Caller: internal/checkout Service.Begin only (commerce_checkout_runtime login), passing p_payment_environment from
-- COMMERCE_PAYMENT_PROFILE (PROVIDER_MOCK => SANDBOX) and p_payment_mode from the buyer body ("" == card).
-- Every other line of the body is the 0005:98 body unchanged.

DROP FUNCTION checkout.begin_hold(bytea,uuid,text,bytea,uuid,jsonb,jsonb,bigint);
-- Idempotent re-creation: the historical (pre-0032) upgrade fixtures pre-install a 10-argument delegating shim so the current Go can
-- run card orders on the old 8-argument body; on every real database this DROP matches nothing.
DROP FUNCTION IF EXISTS checkout.begin_hold(bytea,uuid,text,bytea,uuid,jsonb,jsonb,bigint,text,text);

CREATE FUNCTION checkout.begin_hold(p_hash bytea,p_store uuid,p_key text,p_request_hash bytea,
 p_order uuid,p_snapshot jsonb,p_lines jsonb,p_job_id bigint,p_payment_environment text,p_payment_mode text) RETURNS jsonb
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_scope record; v_tenant uuid; v_owner uuid; v_session uuid;
 v_previous record; v_cart storefront.carts%ROWTYPE; v_quote storefront.quotes%ROWTYPE;
 v_dest storefront.destination_snapshots%ROWTYPE; v_market pricing.markets%ROWTYPE;
 v_policy pricing.policy_versions%ROWTYPE; v_service fulfillment.service_versions%ROWTYPE;
 v_allocation fulfillment.allocation_versions%ROWTYPE;
 v_source record; v_source_enabled boolean;
 v_head_version bigint; v_head_id uuid; v_line record; v_quote_line record;
 v_now timestamptz; v_expires timestamptz; v_result jsonb;
 v_count integer; v_distinct integer; v_mismatch integer;
 v_profile record; v_profiled boolean:=false; v_settings record; v_subtotal bigint; v_total bigint; v_ns text[];
 v_dir boolean:=false; v_api boolean:=false; v_pap boolean:=false; v_sub text;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR p_order IS NULL
    OR p_key IS NULL OR p_key !~ '^[A-Za-z0-9_.:-]{8,128}$'
    OR p_request_hash IS NULL OR octet_length(p_request_hash)<>32 OR p_job_id IS NULL OR p_job_id<1
    OR p_payment_environment IS NULL OR p_payment_environment NOT IN ('SANDBOX','LIVE','')
    OR p_payment_mode IS NULL OR p_payment_mode NOT IN ('card','pay_at_pickup')
    OR p_snapshot IS NULL OR jsonb_typeof(p_snapshot)<>'object' OR octet_length(p_snapshot::text)>1048576
    OR p_lines IS NULL OR jsonb_typeof(p_lines)<>'array' OR octet_length(p_lines::text)>1048576
    OR jsonb_typeof(p_snapshot->'quote')<>'object'
    OR jsonb_typeof(p_snapshot->'destination')<>'object'
    OR jsonb_typeof(p_snapshot->'service')<>'object'
    OR jsonb_typeof(p_snapshot->'allocation')<>'object'
    OR (SELECT count(*) FROM jsonb_object_keys(p_snapshot))<>4 THEN
  RAISE EXCEPTION 'invalid checkout input' USING ERRCODE='PT400';
 END IF;
 SELECT * INTO v_scope FROM buyer.resolve_scope(p_hash,p_store);
 IF NOT FOUND THEN RAISE EXCEPTION 'invalid buyer capability' USING ERRCODE='PT401'; END IF;
 v_tenant:=v_scope.tenant_id; v_owner:=v_scope.owner_id; v_session:=v_scope.session_id;
 PERFORM set_config('app.tenant_id',v_tenant::text,true);
 PERFORM set_config('app.store_id',p_store::text,true);
 PERFORM set_config('app.buyer_id',v_owner::text,true);
 PERFORM set_config('app.buyer_session_id',v_session::text,true);
 PERFORM set_config('app.principal_id','',true);
 PERFORM pg_advisory_xact_lock(hashtextextended(
  'checkout.begin|'||v_tenant||'|'||p_store||'|'||v_owner||'|'||p_key,0));
 SELECT r.request_hash,r.response INTO v_previous FROM checkout.command_results r
  WHERE r.tenant_id=v_tenant AND r.store_id=p_store AND r.owner_id=v_owner
   AND r.operation='checkout.begin' AND r.idempotency_key=p_key;
 IF FOUND THEN
  -- Go resolves an authenticated replay before inserting the River job. A
  -- direct repeat at this writer boundary must abort any newly inserted job.
  RAISE EXCEPTION 'checkout request already recorded' USING ERRCODE='PT409';
 END IF;

 SELECT q.* INTO v_quote FROM storefront.quotes q WHERE q.tenant_id=v_tenant
  AND q.store_id=p_store AND q.owner_id=v_owner AND q.id=(p_snapshot->'quote'->>'id')::uuid;
 IF NOT FOUND OR v_quote.snapshot IS DISTINCT FROM p_snapshot->'quote' THEN
  RAISE EXCEPTION 'quote changed' USING ERRCODE='PT409'; END IF;
 SELECT c.* INTO v_cart FROM storefront.carts c WHERE c.tenant_id=v_tenant
  AND c.store_id=p_store AND c.owner_id=v_owner AND c.id=v_quote.cart_id FOR SHARE;
 IF NOT FOUND OR v_cart.version<>v_quote.cart_version OR v_cart.currency<>v_quote.currency
    OR NOT EXISTS(SELECT 1 FROM storefront.cart_lines l WHERE l.tenant_id=v_tenant
      AND l.store_id=p_store AND l.owner_id=v_owner AND l.cart_id=v_cart.id) THEN
  RAISE EXCEPTION 'cart changed' USING ERRCODE='PT409'; END IF;
 IF EXISTS(SELECT 1 FROM checkout.orders o WHERE o.tenant_id=v_tenant AND o.store_id=p_store
  AND o.owner_id=v_owner AND o.cart_id=v_cart.id AND o.cart_version=v_cart.version
  AND o.commercial_state IN ('DRAFT','AWAITING_PAYMENT','CONFIRMED')) THEN
  RAISE EXCEPTION 'active checkout exists' USING ERRCODE='PT409'; END IF;
 SELECT m.* INTO v_market FROM pricing.markets m WHERE m.tenant_id=v_tenant
  AND m.store_id=p_store AND m.id=v_quote.market_id FOR SHARE;
 IF NOT FOUND OR NOT v_market.active OR v_market.currency<>v_quote.currency
    OR v_market.version<>v_quote.market_version THEN
  RAISE EXCEPTION 'market changed' USING ERRCODE='PT409'; END IF;
 SELECT h.current_version INTO v_head_version FROM pricing.policy_heads h
  WHERE h.tenant_id=v_tenant AND h.store_id=p_store AND h.market_id=v_quote.market_id
   AND h.country=v_quote.country AND h.method=v_quote.method FOR SHARE;
 IF NOT FOUND OR v_head_version<>v_quote.policy_version THEN
  RAISE EXCEPTION 'policy changed' USING ERRCODE='PT409'; END IF;
 SELECT p.* INTO v_policy FROM pricing.policy_versions p WHERE p.tenant_id=v_tenant
  AND p.store_id=p_store AND p.market_id=v_quote.market_id AND p.country=v_quote.country
  AND p.method=v_quote.method AND p.version=v_quote.policy_version;
 IF NOT FOUND OR NOT v_policy.enabled OR v_policy.currency<>v_quote.currency
    OR p_snapshot->'quote'->'policy'->>'method'<>v_quote.method THEN
  RAISE EXCEPTION 'policy changed' USING ERRCODE='PT409'; END IF;

 SELECT d.* INTO v_dest FROM storefront.destination_snapshots d WHERE d.tenant_id=v_tenant
  AND d.store_id=p_store AND d.owner_id=v_owner AND d.id=(p_snapshot->'destination'->>'id')::uuid;
 IF NOT FOUND OR v_dest.cart_id<>v_cart.id OR v_dest.cart_version<>v_cart.version
    OR v_dest.country<>v_quote.country OR v_dest.kind IS DISTINCT FROM p_snapshot->'destination'->>'kind'
    OR p_snapshot->'destination'->>'cart_id' IS DISTINCT FROM v_cart.id::text
    OR p_snapshot->'destination'->>'country' IS DISTINCT FROM v_dest.country
    OR p_snapshot->'destination'->>'recipient_name' IS DISTINCT FROM v_dest.recipient_name
    OR p_snapshot->'destination'->>'phone' IS DISTINCT FROM v_dest.phone
    OR p_snapshot->'destination'->'home_address' IS DISTINCT FROM
     jsonb_build_object('region',v_dest.region,'city',v_dest.city,'postal_code',v_dest.postal_code,
      'line1',v_dest.line1,'line2',v_dest.line2) THEN
  RAISE EXCEPTION 'destination changed' USING ERRCODE='PT409'; END IF;
 IF v_dest.pickup_id IS NOT NULL THEN
  SELECT p.id,p.kind,p.namespace,p.code,p.version,p.country,p.verification_kind,
   p.attested_at,p.valid_until INTO v_source FROM fulfillment.pickup_versions p WHERE p.tenant_id=v_tenant
   AND p.store_id=p_store AND p.id=v_dest.pickup_id;
  IF NOT FOUND OR v_source.kind<>v_dest.kind OR v_source.country<>v_dest.country
     OR v_source.verification_kind NOT IN ('MANUAL_ATTESTED','PROVIDER_DIRECTORY_VERIFIED','BUYER_ENTERED')
     OR p_snapshot->'destination'->'pickup'->>'id' IS DISTINCT FROM v_source.id::text THEN
   RAISE EXCEPTION 'pickup source changed' USING ERRCODE='PT409'; END IF;
  SELECT h.pickup_id,h.current_version,h.enabled INTO v_head_id,v_head_version,v_source_enabled
   FROM fulfillment.pickup_heads h WHERE h.tenant_id=v_tenant AND h.store_id=p_store
    AND h.kind=v_source.kind AND h.namespace=v_source.namespace AND h.code=v_source.code FOR SHARE;
  IF NOT FOUND OR NOT v_source_enabled OR v_head_id<>v_source.id OR v_head_version<>v_source.version THEN
   RAISE EXCEPTION 'pickup source changed' USING ERRCODE='PT409'; END IF;
 ELSIF v_dest.kind<>'home' THEN
  RAISE EXCEPTION 'pickup source missing' USING ERRCODE='PT409';
 END IF;
 SELECT h.destination_id,h.current_version INTO v_head_id,v_head_version
  FROM storefront.destination_heads h WHERE h.tenant_id=v_tenant AND h.store_id=p_store
   AND h.owner_id=v_owner AND h.cart_id=v_cart.id FOR SHARE;
 IF NOT FOUND OR v_head_id<>v_dest.id OR v_head_version<>v_dest.version
    OR (p_snapshot->'destination'->>'version')::bigint IS DISTINCT FROM v_dest.version THEN
  RAISE EXCEPTION 'destination changed' USING ERRCODE='PT409'; END IF;

 IF v_quote.method NOT LIKE 'delivery:%' THEN
  RAISE EXCEPTION 'delivery service missing' USING ERRCODE='PT409'; END IF;
 SELECT h.current_version INTO v_head_version FROM fulfillment.service_heads h
  WHERE h.tenant_id=v_tenant AND h.store_id=p_store AND h.market_id=v_quote.market_id
   AND h.country=v_quote.country AND h.code=substring(v_quote.method FROM 10) FOR SHARE;
 IF NOT FOUND OR v_head_version IS DISTINCT FROM (p_snapshot->'service'->>'version')::bigint THEN
  RAISE EXCEPTION 'service changed' USING ERRCODE='PT409'; END IF;
 SELECT s.* INTO v_service FROM fulfillment.service_versions s WHERE s.tenant_id=v_tenant
  AND s.store_id=p_store AND s.market_id=v_quote.market_id AND s.country=v_quote.country
  AND s.code=substring(v_quote.method FROM 10) AND s.version=v_head_version;
 IF NOT FOUND OR NOT v_service.enabled OR NOT v_service.visible OR v_service.mode NOT IN ('MANUAL','API')
    OR v_service.delivery_kind<>v_dest.kind OR v_service.currency<>v_quote.currency
    OR v_service.policy_version<>v_quote.policy_version
    OR p_snapshot->'service'->>'code' IS DISTINCT FROM v_service.code
    OR p_snapshot->'service'->>'market_id' IS DISTINCT FROM v_service.market_id::text
    OR p_snapshot->'service'->>'country' IS DISTINCT FROM v_service.country THEN
  RAISE EXCEPTION 'service changed' USING ERRCODE='PT409'; END IF;
 -- The store's single enabled profile (lock order: pickup head -> destination head -> service head -> profile FOR SHARE -> allocation).
 SELECT pr.connection_id,pr.mode,pr.ok_verified,pr.hilife_verified,pr.qualified_credential_version,a.environment,
  a.credential_version,a.binding_id INTO v_profile
  FROM integration.ecpay_logistics_profiles pr
  JOIN integration.merchant_accounts a ON a.tenant_id=pr.tenant_id AND a.store_id=pr.store_id AND a.id=pr.connection_id
  WHERE pr.tenant_id=v_tenant AND pr.store_id=p_store AND pr.enabled FOR SHARE OF pr;
 v_profiled:=FOUND AND v_profile.qualified_credential_version IS NOT DISTINCT FROM v_profile.credential_version;
 v_api:=v_service.mode='API';
 IF v_dest.pickup_id IS NOT NULL THEN v_dir:=v_source.verification_kind='PROVIDER_DIRECTORY_VERIFIED'; END IF;
 -- (b) API service: enabled+visible (above) and bound to the store's enabled qualified profile for this destination kind.
 IF v_api AND (NOT v_profiled OR v_service.binding_id IS DISTINCT FROM v_profile.binding_id
    OR (v_dest.kind='cvs_okmart' AND NOT v_profile.ok_verified) OR (v_dest.kind='cvs_hilife' AND NOT v_profile.hilife_verified)
    OR v_dest.kind NOT IN ('cvs_711','cvs_familymart','cvs_hilife','cvs_okmart')) THEN
  RAISE EXCEPTION 'service changed' USING ERRCODE='PT409'; END IF;
 -- (a)/§16.1: a buyer-entered store is never verified: MANUAL service only, only while the store has no qualified profile.
 IF v_dest.pickup_id IS NOT NULL THEN
  IF v_source.verification_kind='BUYER_ENTERED' THEN
   IF v_service.mode<>'MANUAL' OR v_profiled THEN RAISE EXCEPTION 'cvs_source_mismatch' USING ERRCODE='PT422'; END IF;
   IF v_source.namespace<>'buyer.'||v_owner::text THEN RAISE EXCEPTION 'pickup source changed' USING ERRCODE='PT409'; END IF;
  END IF;
 END IF;
 IF v_api AND NOT v_dir THEN RAISE EXCEPTION 'cvs_source_mismatch' USING ERRCODE='PT422'; END IF;
 -- (c) provider-verified source or API service: ECPay limits, all money in TWD minor units (0061 convention, x100; I05: the
 -- server quote, never the client), recipient rule F5, and the ECPay environment equals the deployment payment environment.
 IF v_api OR v_dir THEN
  v_subtotal:=(v_quote.snapshot->'amount'->>'subtotal_minor')::bigint;
  IF v_quote.currency<>'TWD' OR v_subtotal IS NULL OR v_subtotal%100<>0 OR v_subtotal NOT BETWEEN 100 AND 2000000 THEN
   RAISE EXCEPTION 'cvs_amount_exceeds' USING ERRCODE='PT422'; END IF;
  IF NOT fulfillment.ecpay_recipient_ok(v_dest.recipient_name,v_dest.phone) THEN
   RAISE EXCEPTION 'cvs_recipient_rejected' USING ERRCODE='PT422'; END IF;
  IF NOT v_profiled OR p_payment_environment='' OR v_profile.environment<>p_payment_environment THEN
   RAISE EXCEPTION 'cvs_environment_mismatch' USING ERRCODE='PT422'; END IF;
  IF v_dir THEN
   v_ns:=regexp_match(v_source.namespace,'^ecpay\.(sandbox|live)\.([a-z0-9]+)$');
   v_sub:=upper(v_ns[2]);
   IF v_ns IS NULL OR upper(v_ns[1])<>v_profile.environment
    OR v_sub NOT IN ('UNIMARTC2C','FAMIC2C','HILIFEC2C','OKMARTC2C','UNIMART','FAMI','HILIFE')
    OR (v_profile.mode='C2C' AND v_sub NOT LIKE '%C2C') OR (v_profile.mode='B2C' AND v_sub LIKE '%C2C') THEN
    RAISE EXCEPTION 'cvs_environment_mismatch' USING ERRCODE='PT422'; END IF;
  END IF;
 END IF;
 -- §16.2/§16.5 pay_at_pickup: every guard fails with zero holds. Settings are locked FOR UPDATE right after the service head
 -- and profile, which serialises pay-at-pickup placements per store (a missing row means pay-at-pickup is off).
 v_pap:=p_payment_mode='pay_at_pickup';
 IF v_pap THEN
  IF v_dest.kind NOT IN ('cvs_711','cvs_familymart','cvs_hilife','cvs_okmart') THEN
   RAISE EXCEPTION 'pay_at_pickup_unavailable' USING ERRCODE='PT422'; END IF;
  SELECT c.pay_at_pickup_enabled,c.pay_at_pickup_max_twd,c.pay_at_pickup_max_open,c.enabled_chains INTO v_settings
   FROM fulfillment.cvs_store_settings c WHERE c.tenant_id=v_tenant AND c.store_id=p_store FOR UPDATE;
  IF NOT FOUND OR NOT v_settings.pay_at_pickup_enabled OR NOT v_dest.kind=ANY(v_settings.enabled_chains)
   OR v_quote.currency<>'TWD' THEN RAISE EXCEPTION 'pay_at_pickup_unavailable' USING ERRCODE='PT422'; END IF;
  v_total:=(v_quote.snapshot->'amount'->>'total_minor')::bigint;
  IF v_total IS NULL OR v_total%100<>0 OR v_total NOT BETWEEN 100 AND least(v_settings.pay_at_pickup_max_twd,20000)::bigint*100 THEN
   RAISE EXCEPTION 'pay_at_pickup_amount_exceeds' USING ERRCODE='PT422'; END IF;
  -- C3: the recipient must be a real name and a mobile for every pay_at_pickup order, whichever store source.
  IF NOT fulfillment.ecpay_recipient_ok(v_dest.recipient_name,v_dest.phone) THEN
   RAISE EXCEPTION 'cvs_recipient_rejected' USING ERRCODE='PT422'; END IF;
  -- R4-3: open (unshipped, uncollected) pay-at-pickup orders bound what anonymous buyers can lock (partial index orders_pay_at_pickup_open).
  IF (SELECT count(*) FROM checkout.orders x WHERE x.tenant_id=v_tenant AND x.store_id=p_store AND x.payment_mode='pay_at_pickup'
     AND x.collection_state='PENDING' AND x.fulfillment_state='MANUAL_UNASSIGNED')>=v_settings.pay_at_pickup_max_open
   OR EXISTS(SELECT 1 FROM checkout.orders x WHERE x.tenant_id=v_tenant AND x.store_id=p_store AND x.owner_id=v_owner
     AND x.payment_mode='pay_at_pickup' AND x.collection_state='PENDING' AND x.fulfillment_state='MANUAL_UNASSIGNED') THEN
   RAISE EXCEPTION 'pay_at_pickup_limit' USING ERRCODE='PT429'; END IF;
 END IF;
 SELECT h.current_version INTO v_head_version FROM fulfillment.allocation_heads h
  WHERE h.tenant_id=v_tenant AND h.store_id=p_store AND h.market_id=v_quote.market_id
   AND h.country=v_quote.country AND h.code=v_service.code FOR SHARE;
 IF NOT FOUND OR v_head_version IS DISTINCT FROM (p_snapshot->'allocation'->>'version')::bigint THEN
  RAISE EXCEPTION 'allocation changed' USING ERRCODE='PT409'; END IF;
 SELECT a.* INTO v_allocation FROM fulfillment.allocation_versions a WHERE a.tenant_id=v_tenant
  AND a.store_id=p_store AND a.market_id=v_quote.market_id AND a.country=v_quote.country
  AND a.code=v_service.code AND a.version=v_head_version;
 IF NOT FOUND OR v_allocation.warehouse_count<1
    OR p_snapshot->'allocation'->>'market_id' IS DISTINCT FROM v_allocation.market_id::text
    OR p_snapshot->'allocation'->>'country' IS DISTINCT FROM v_allocation.country
    OR p_snapshot->'allocation'->>'code' IS DISTINCT FROM v_allocation.code
    OR p_snapshot->'allocation'->'warehouse_ids' IS DISTINCT FROM
     (SELECT jsonb_agg(a.warehouse_id::text ORDER BY a.position)
       FROM fulfillment.allocation_warehouses a WHERE a.tenant_id=v_tenant AND a.store_id=p_store
        AND a.market_id=v_allocation.market_id AND a.country=v_allocation.country
        AND a.code=v_allocation.code AND a.version=v_allocation.version) THEN
  RAISE EXCEPTION 'allocation unavailable' USING ERRCODE='PT409'; END IF;

 -- Structural plan and exact SKU conservation only. Go's locked, current
 -- price calculation remains the one monetary authority.
 SELECT count(*),count(DISTINCT (l.warehouse_id,l.sku_id)) INTO v_count,v_distinct
  FROM jsonb_to_recordset(p_lines) AS l(warehouse_id uuid,sku_id uuid,quantity bigint);
 IF v_count NOT BETWEEN 1 AND 800 OR v_count<>v_distinct THEN
  RAISE EXCEPTION 'invalid stock plan' USING ERRCODE='PT400'; END IF;
 SELECT count(*),count(DISTINCT q.sku_id) INTO v_count,v_distinct
  FROM jsonb_to_recordset(v_quote.snapshot->'lines') AS q(sku_id uuid,quantity bigint);
 IF v_count NOT BETWEEN 1 AND 50 OR v_count<>v_distinct THEN
  RAISE EXCEPTION 'invalid quote lines' USING ERRCODE='PT400'; END IF;
 SELECT count(*) INTO v_mismatch FROM (
  WITH demand AS (SELECT q.sku_id,sum(q.quantity) AS quantity
    FROM jsonb_to_recordset(v_quote.snapshot->'lines') AS q(sku_id uuid,quantity bigint)
    GROUP BY q.sku_id),
   plan AS (SELECT l.sku_id,sum(l.quantity) AS quantity
    FROM jsonb_to_recordset(p_lines) AS l(warehouse_id uuid,sku_id uuid,quantity bigint)
    GROUP BY l.sku_id)
  SELECT 1 FROM demand d FULL JOIN plan p USING(sku_id)
   WHERE d.sku_id IS NULL OR p.sku_id IS NULL OR d.quantity IS DISTINCT FROM p.quantity) mismatch;
 IF v_mismatch<>0 THEN RAISE EXCEPTION 'stock plan differs from quote' USING ERRCODE='PT409'; END IF;
 FOR v_line IN SELECT l.warehouse_id,l.sku_id,l.quantity
  FROM jsonb_to_recordset(p_lines) AS l(warehouse_id uuid,sku_id uuid,quantity bigint)
  ORDER BY l.warehouse_id,l.sku_id LOOP
  IF v_line.warehouse_id IS NULL OR v_line.sku_id IS NULL
     OR v_line.quantity IS NULL OR v_line.quantity NOT BETWEEN 1 AND 1000000000
     OR NOT EXISTS(SELECT 1 FROM fulfillment.allocation_warehouses a WHERE a.tenant_id=v_tenant
      AND a.store_id=p_store AND a.market_id=v_quote.market_id AND a.country=v_quote.country
      AND a.code=v_service.code AND a.version=v_allocation.version
      AND a.warehouse_id=v_line.warehouse_id)
     OR NOT EXISTS(SELECT 1 FROM inventory.warehouses w WHERE w.tenant_id=v_tenant
      AND w.store_id=p_store AND w.id=v_line.warehouse_id AND w.active) THEN
   RAISE EXCEPTION 'invalid stock plan' USING ERRCODE='PT409'; END IF;
  IF NOT EXISTS(SELECT 1 FROM inventory.balances b WHERE b.tenant_id=v_tenant
    AND b.store_id=p_store AND b.warehouse_id=v_line.warehouse_id AND b.sku_id=v_line.sku_id
    AND b.on_hand-b.reserved-b.allocated-b.unavailable>=v_line.quantity) THEN
   RAISE EXCEPTION 'insufficient stock' USING ERRCODE='PT402'; END IF;
 END LOOP;
 FOR v_quote_line IN SELECT q.sku_id,q.product_id,q.quantity,q.unit_price_minor
  FROM jsonb_to_recordset(v_quote.snapshot->'lines')
   AS q(sku_id uuid,product_id uuid,quantity bigint,unit_price_minor bigint) LOOP
  IF v_quote_line.sku_id IS NULL OR v_quote_line.product_id IS NULL
     OR v_quote_line.quantity IS NULL OR v_quote_line.quantity NOT BETWEEN 1 AND 1000000000
     OR v_quote_line.unit_price_minor IS NULL OR v_quote_line.unit_price_minor NOT BETWEEN 0 AND 1000000000000 THEN
   RAISE EXCEPTION 'invalid quote lines' USING ERRCODE='PT400'; END IF;
 END LOOP;

 -- Final time follows every row and advisory wait. An expiry job is a
 -- scheduled housekeeping task, never a runnable payment attempt.
 v_now:=clock_timestamp();
 SELECT * INTO v_scope FROM buyer.resolve_scope(p_hash,p_store);
 IF NOT FOUND OR v_scope.tenant_id<>v_tenant OR v_scope.owner_id<>v_owner
    OR v_scope.session_id<>v_session THEN
  RAISE EXCEPTION 'buyer capability expired' USING ERRCODE='PT401'; END IF;
 IF v_quote.created_at>v_now OR v_quote.expires_at<=v_now
    OR v_dest.selected_at>v_now OR v_dest.expires_at<=v_now THEN
  RAISE EXCEPTION 'checkout source expired' USING ERRCODE='PT409'; END IF;
 IF v_dest.pickup_id IS NOT NULL THEN
  IF v_source.attested_at>v_dest.selected_at OR v_source.attested_at>v_now
     OR v_source.valid_until<=v_now OR v_dest.expires_at>v_source.valid_until THEN
   RAISE EXCEPTION 'pickup source expired' USING ERRCODE='PT409'; END IF;
 END IF;
 IF NOT EXISTS(SELECT 1 FROM river_expiry.river_job j WHERE j.id=p_job_id
  AND j.kind='checkout_expiry_v1' AND j.args->>'order_id'=p_order::text
  AND j.args->>'generation'='1' AND j.args->>'version'='1') THEN
  RAISE EXCEPTION 'expiry job missing' USING ERRCODE='PT409'; END IF;
 v_expires:=v_now+interval '15 minutes';
 v_result:=jsonb_build_object('order_id',p_order::text,'reservation_id',p_order::text,
  'generation',1,'expires_at',v_expires,'job_id',p_job_id,'payment_mode',p_payment_mode,
  'commercial_state',CASE WHEN v_pap THEN 'CONFIRMED' ELSE 'DRAFT' END);
 INSERT INTO checkout.orders(tenant_id,store_id,owner_id,id,creator_session_id,cart_id,cart_version,
  quote_id,destination_id,market_id,country,service_code,service_version,allocation_version,
  currency,total_minor,commercial_state,fulfillment_state,generation,expires_at,job_id,snapshot,created_at,updated_at,
  payment_mode,collection_state)
 VALUES(v_tenant,p_store,v_owner,p_order,v_session,v_cart.id,v_cart.version,v_quote.id,v_dest.id,
  v_quote.market_id,v_quote.country,v_service.code,v_service.version,v_allocation.version,
  v_quote.currency,(v_quote.snapshot->'amount'->>'total_minor')::bigint,
  CASE WHEN v_pap THEN 'CONFIRMED' ELSE 'DRAFT' END,'MANUAL_UNASSIGNED',1,v_expires,p_job_id,p_snapshot,v_now,v_now,
  p_payment_mode,CASE WHEN v_pap THEN 'PENDING' END);
 INSERT INTO inventory.reservations(tenant_id,store_id,id,state,expires_at,
  checkout_id,buyer_owner_id,buyer_session_id,generation)
 VALUES(v_tenant,p_store,p_order,'HELD',v_expires,p_order,v_owner,v_session,1);
 FOR v_line IN SELECT l.warehouse_id,l.sku_id,l.quantity
  FROM jsonb_to_recordset(p_lines) AS l(warehouse_id uuid,sku_id uuid,quantity bigint)
  ORDER BY l.warehouse_id,l.sku_id LOOP
  INSERT INTO inventory.reservation_lines(tenant_id,store_id,reservation_id,warehouse_id,sku_id,quantity)
   VALUES(v_tenant,p_store,p_order,v_line.warehouse_id,v_line.sku_id,v_line.quantity);
  INSERT INTO inventory.ledger(tenant_id,store_id,warehouse_id,sku_id,kind,delta_reserved,
   operation,command_key,reservation_id,principal_id,checkout_id,buyer_owner_id,buyer_session_id,actor_kind)
   VALUES(v_tenant,p_store,v_line.warehouse_id,v_line.sku_id,'RESERVE',v_line.quantity,
    'checkout.begin',p_order::text,p_order,NULL,p_order,v_owner,v_session,'BUYER');
 END LOOP;
 IF v_pap THEN
  -- §11.5 HELD -> COMMITTED at placement (stock follows the normal path, no Stripe session, no payment attempt): one ALLOCATE
  -- row per line (BUYER, checkout.pay_at_pickup.commit, command_key = order id), after inventory.lock_balance. Evidence: order id
  -- + collection_state PENDING (guard inventory.guard_pay_at_pickup_ledger).
  FOR v_line IN SELECT l.warehouse_id,l.sku_id,l.quantity
   FROM jsonb_to_recordset(p_lines) AS l(warehouse_id uuid,sku_id uuid,quantity bigint)
   ORDER BY l.warehouse_id,l.sku_id LOOP
   PERFORM 1 FROM inventory.lock_balance(v_line.warehouse_id,v_line.sku_id);
   INSERT INTO inventory.ledger(tenant_id,store_id,warehouse_id,sku_id,kind,delta_reserved,delta_allocated,
    operation,command_key,reservation_id,principal_id,checkout_id,buyer_owner_id,buyer_session_id,actor_kind)
    VALUES(v_tenant,p_store,v_line.warehouse_id,v_line.sku_id,'ALLOCATE',-v_line.quantity,v_line.quantity,
     'checkout.pay_at_pickup.commit',p_order::text,p_order,NULL,p_order,v_owner,v_session,'BUYER');
  END LOOP;
  UPDATE inventory.reservations SET state='COMMITTED' WHERE tenant_id=v_tenant AND store_id=p_store AND id=p_order AND state='HELD';
 END IF;
 INSERT INTO checkout.events(tenant_id,store_id,owner_id,order_id,session_id,generation,action,actor_kind)
 VALUES(v_tenant,p_store,v_owner,p_order,v_session,1,'checkout.held','BUYER');
 IF v_pap THEN
  INSERT INTO checkout.events(tenant_id,store_id,owner_id,order_id,session_id,generation,action,actor_kind)
  VALUES(v_tenant,p_store,v_owner,p_order,v_session,1,'checkout.pay_at_pickup_placed','BUYER');
 END IF;
 INSERT INTO checkout.command_results(tenant_id,store_id,owner_id,operation,idempotency_key,
  creator_session_id,request_hash,order_id,response)
 VALUES(v_tenant,p_store,v_owner,'checkout.begin',p_key,v_session,p_request_hash,p_order,v_result);
 RETURN v_result;
EXCEPTION WHEN invalid_text_representation OR numeric_value_out_of_range
 OR invalid_datetime_format THEN
 RAISE EXCEPTION 'invalid checkout input' USING ERRCODE='PT400';
END $$;
ALTER FUNCTION checkout.begin_hold(bytea,uuid,text,bytea,uuid,jsonb,jsonb,bigint,text,text) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION checkout.begin_hold(bytea,uuid,text,bytea,uuid,jsonb,jsonb,bigint,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION checkout.begin_hold(bytea,uuid,text,bytea,uuid,jsonb,jsonb,bigint,text,text) TO commerce_checkout_runtime;
COMMENT ON FUNCTION checkout.begin_hold(bytea,uuid,text,bytea,uuid,jsonb,jsonb,bigint,text,text) IS
 'internal/checkout Service.Begin only; EXECUTE commerce_checkout_runtime. Order + stock hold from a priced snapshot; CVS deltas of taiwan-cvs-logistics-v1 §4.3/§16.2: pickup kinds MANUAL_ATTESTED|PROVIDER_DIRECTORY_VERIFIED|BUYER_ENTERED (buyer-entered only with a MANUAL service and no qualified profile), API services bound to the enabled qualified profile, ECPay TWD x100 amount/recipient/environment guards (PT422 cvs_amount_exceeds|cvs_recipient_rejected|cvs_environment_mismatch|cvs_source_mismatch), and p_payment_mode pay_at_pickup (PT422 pay_at_pickup_unavailable|pay_at_pickup_amount_exceeds, PT429 pay_at_pickup_limit; HELD -> COMMITTED with BUYER ALLOCATE ledger rows, no payment attempt). Exactly one checkout.begin_hold exists after a full Apply.';
