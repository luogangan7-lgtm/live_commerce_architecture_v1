-- Stripe extends the existing payment River family. No second queue or stock writer.
GRANT USAGE ON SCHEMA river_payment TO commerce_stripe_ingress;
GRANT SELECT,INSERT ON river_payment.river_job TO commerce_stripe_ingress;
GRANT USAGE ON SEQUENCE river_payment.river_job_id_seq TO commerce_stripe_ingress;

CREATE OR REPLACE FUNCTION integration.payment_job_queue(p_job bigint)
RETURNS text LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT CASE a.execution_profile
  WHEN 'PROVIDER_MOCK' THEN 'payment_mock_v1'
  WHEN 'SANDBOX' THEN 'payment_sandbox_v1'
  WHEN 'LIVE' THEN 'payment_live_v1' END
 FROM river_payment.river_job j JOIN checkout.payment_attempts a
  ON a.id::text=j.args->>'operation_id'
 WHERE j.id=p_job AND (
  (j.kind='payment_query_v1' AND a.job_id=j.id
   AND j.args=jsonb_build_object('operation_id',a.id::text,'version',1))
  OR (j.kind='payment_reconcile_v1' AND EXISTS(
   SELECT 1 FROM payments.provider_observations o
   WHERE o.tenant_id=a.tenant_id AND o.store_id=a.store_id AND o.attempt_id=a.id
    AND o.source IN ('QUERY','LOCAL') AND o.execution_profile=a.execution_profile
    AND o.environment=a.environment
    AND j.args=jsonb_build_object('operation_id',a.id::text,
     'report_hash',encode(o.report_hash,'hex'),'version',1)))
  OR (j.kind='payment_signal_v1' AND EXISTS(
   SELECT 1 FROM payments.stripe_signals s WHERE s.job_id=j.id AND s.attempt_id=a.id
    AND s.tenant_id=a.tenant_id AND s.store_id=a.store_id
    AND j.args=jsonb_build_object('operation_id',a.id::text,'signal_id',s.id::text,'version',1))))
$$;
COMMENT ON FUNCTION integration.payment_job_queue(bigint) IS 'integration owner; links River payment query, reconcile or Stripe signal to a frozen attempt profile; no default-queue escape';

CREATE OR REPLACE FUNCTION integration.route_payment_queue_v1()
RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE j river_payment.river_job%ROWTYPE; expected text;
BEGIN
 SELECT x.* INTO j FROM river_payment.river_job x WHERE x.id=NEW.id FOR UPDATE;
 IF NEW.kind IN ('payment_query_v1','payment_reconcile_v1','payment_signal_v1')
  OR NEW.queue IN ('payment_mock_v1','payment_sandbox_v1','payment_live_v1')
  OR j.kind IN ('payment_query_v1','payment_reconcile_v1','payment_signal_v1')
  OR j.queue IN ('payment_mock_v1','payment_sandbox_v1','payment_live_v1') THEN
  IF j.id IS NULL OR j.kind IS DISTINCT FROM NEW.kind OR j.args IS DISTINCT FROM NEW.args
   OR j.kind NOT IN ('payment_query_v1','payment_reconcile_v1','payment_signal_v1')
   OR j.unique_key IS NOT NULL THEN
   RAISE EXCEPTION 'invalid payment queue job' USING ERRCODE='22023'; END IF;
  expected:=integration.payment_job_queue(j.id);
  IF expected IS NULL OR j.queue NOT IN ('default',expected) THEN
   RAISE EXCEPTION 'payment queue linkage mismatch' USING ERRCODE='22023'; END IF;
  IF j.queue<>expected THEN
   UPDATE river_payment.river_job SET queue=expected WHERE id=j.id;
  END IF;
 END IF;
 RETURN NULL;
END $$;
COMMENT ON FUNCTION integration.route_payment_queue_v1() IS 'integration owner; deferrable River router for linked payment and Stripe signal jobs; no unaudited queue selection';

CREATE OR REPLACE FUNCTION integration.guard_payment_job_family() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 IF NEW.kind NOT IN ('payment_query_v1','payment_reconcile_v1','payment_signal_v1')
  OR NEW.queue NOT IN ('default','payment_mock_v1','payment_sandbox_v1','payment_live_v1')
  OR NEW.unique_key IS NOT NULL OR NEW.args IS NULL OR jsonb_typeof(NEW.args)<>'object'
  OR NEW.args->>'operation_id' IS NULL
  OR NEW.args->>'operation_id' !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  OR NEW.args->>'version' IS DISTINCT FROM '1'
  OR (NEW.kind='payment_query_v1' AND NEW.args IS DISTINCT FROM
   jsonb_build_object('operation_id',NEW.args->>'operation_id','version',1))
  OR (NEW.kind='payment_reconcile_v1' AND
   (NEW.args->>'report_hash' IS NULL OR NEW.args->>'report_hash' !~ '^[0-9a-f]{64}$'
    OR NEW.args IS DISTINCT FROM jsonb_build_object('operation_id',NEW.args->>'operation_id',
     'report_hash',NEW.args->>'report_hash','version',1)))
  OR (NEW.kind='payment_signal_v1' AND
   (NEW.args->>'signal_id' IS NULL
    OR NEW.args->>'signal_id' !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
    OR NEW.args IS DISTINCT FROM jsonb_build_object('operation_id',NEW.args->>'operation_id',
     'signal_id',NEW.args->>'signal_id','version',1))) THEN
  RAISE EXCEPTION 'invalid payment family job' USING ERRCODE='22023'; END IF;
 IF TG_OP='UPDATE' THEN
  IF NEW.id IS DISTINCT FROM OLD.id OR NEW.kind IS DISTINCT FROM OLD.kind
   OR NEW.args IS DISTINCT FROM OLD.args OR NEW.unique_key IS DISTINCT FROM OLD.unique_key
   OR (NEW.queue IS DISTINCT FROM OLD.queue AND NOT
    (OLD.queue='default' AND NEW.queue IS NOT DISTINCT FROM integration.payment_job_queue(OLD.id))) THEN
   RAISE EXCEPTION 'immutable payment job identity' USING ERRCODE='22023'; END IF;
 END IF;
 RETURN NEW;
END $$;
COMMENT ON FUNCTION integration.guard_payment_job_family() IS 'integration owner; River payment family argument and queue guard, including Stripe signals; no job identity rewrite';

CREATE OR REPLACE FUNCTION integration.reject_legacy_family_job() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 IF NEW.kind IN ('payment_query_v1','payment_reconcile_v1','payment_signal_v1','checkout_expiry_v1')
  OR NEW.queue IN ('payment_mock_v1','payment_sandbox_v1','payment_live_v1','checkout_expiry_v1')
  OR (TG_OP='UPDATE' AND (OLD.kind IN
   ('payment_query_v1','payment_reconcile_v1','payment_signal_v1','checkout_expiry_v1')
   OR OLD.queue IN ('payment_mock_v1','payment_sandbox_v1','payment_live_v1','checkout_expiry_v1'))) THEN
  RAISE EXCEPTION 'legacy family lane disabled' USING ERRCODE='22023'; END IF;
 RETURN NEW;
END $$;
COMMENT ON FUNCTION integration.reject_legacy_family_job() IS 'integration owner; blocks payment, Stripe signal and expiry jobs from legacy River table';

CREATE OR REPLACE FUNCTION integration.payment_queue_ready()
RETURNS boolean LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE guards_ready boolean;
BEGIN
 SELECT count(*)=3 INTO guards_ready FROM (VALUES
  ('river_payment.river_job','payment_job_family','integration.guard_payment_job_family()',23,false,'commerce_integration_writer'),
  ('river_payment.river_job','payment_queue_route_v1','integration.route_payment_queue_v1()',5,true,'commerce_integration_writer'),
  ('river.river_job','legacy_family_exclusion','integration.reject_legacy_family_job()',23,false,'commerce_integration_writer')
 ) AS expected(relation_name,trigger_name,function_name,trigger_type,deferred,owner_name)
 JOIN pg_catalog.pg_trigger t ON t.tgname=expected.trigger_name
  AND t.tgfoid=to_regprocedure(expected.function_name)
 JOIN pg_catalog.pg_proc p ON p.oid=t.tgfoid
 JOIN pg_catalog.pg_roles r ON r.oid=p.proowner
 JOIN pg_catalog.pg_class c ON c.oid=t.tgrelid
 JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname||'.'||c.relname=expected.relation_name
  AND t.tgtype=expected.trigger_type AND t.tgenabled IN ('O','A') AND NOT t.tgisinternal
  AND t.tgdeferrable=expected.deferred AND t.tginitdeferred=expected.deferred
  AND t.tgqual IS NULL AND t.tgnargs=0 AND t.tgargs='\x'::bytea AND t.tgattr=''::int2vector
  AND p.prosecdef AND p.proconfig=ARRAY['search_path=pg_catalog']::text[]
  AND r.rolname=expected.owner_name AND NOT r.rolcanlogin AND NOT r.rolsuper
  AND NOT r.rolbypassrls AND NOT r.rolcreatedb AND NOT r.rolcreaterole AND NOT r.rolreplication
  AND p.proowner<>c.relowner AND p.proowner<>n.nspowner
  AND p.proowner<>(SELECT datdba FROM pg_catalog.pg_database WHERE datname=current_database());
 IF NOT guards_ready THEN RETURN false; END IF;
 RETURN NOT EXISTS(SELECT 1 FROM river_payment.river_job j WHERE j.kind NOT IN
   ('payment_query_v1','payment_reconcile_v1','payment_signal_v1')
   OR j.queue NOT IN ('default','payment_mock_v1','payment_sandbox_v1','payment_live_v1')
   OR (j.state NOT IN ('completed','cancelled','discarded') AND
    (j.unique_key IS NOT NULL OR j.args->>'version' IS DISTINCT FROM '1'
     OR j.queue IS DISTINCT FROM integration.payment_job_queue(j.id))));
END $$;
COMMENT ON FUNCTION integration.payment_queue_ready() IS 'integration owner; worker readiness audits River guards, linkage and profile queues for payment and Stripe signal jobs';

CREATE FUNCTION checkout.start_stripe_payment(p_hash bytea,p_store uuid,p_key text,p_request_hash bytea,
 p_order uuid,p_method text,p_version bigint,p_profile text,p_attempt uuid,p_job bigint,
 p_locale text,p_config_digest bytea,p_return_url text) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE s record; sf record; o checkout.orders%ROWTYPE; r inventory.reservations%ROWTYPE;
 m payments.method_versions%ROWTYPE; a integration.merchant_accounts%ROWTYPE;
 b integration.bindings%ROWTYPE; q payments.account_qualifications%ROWTYPE;
 v_version bigint; v_active boolean; v_now timestamptz; v_trade text;
 v_result jsonb; v_params jsonb; v_expiry timestamptz; v_locale text;
BEGIN
 IF current_setting('transaction_isolation')<>'read committed'
 OR p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR p_order IS NULL
 OR p_key IS NULL OR p_key !~ '^[A-Za-z0-9_.:-]{8,128}$'
 OR p_request_hash IS NULL OR octet_length(p_request_hash)<>32 OR p_attempt IS NULL
 OR p_job IS NULL OR p_job<1 OR p_method IS DISTINCT FROM 'stripe_checkout'
 OR p_version IS NULL OR p_version<1 OR p_profile NOT IN ('PROVIDER_MOCK','SANDBOX')
 OR p_locale NOT IN ('zh-CN','zh-TW','en')
 OR p_config_digest IS NULL OR octet_length(p_config_digest)<>32
 OR p_return_url IS NULL OR octet_length(p_return_url)>2048
 OR p_return_url !~ '^https://[^[:space:]]+$' THEN
  RAISE EXCEPTION 'invalid Stripe payment input' USING ERRCODE='PT400'; END IF;
 SELECT * INTO s FROM buyer.resolve_scope(p_hash,p_store);
 IF NOT FOUND THEN RAISE EXCEPTION 'invalid buyer capability' USING ERRCODE='PT401'; END IF;
 PERFORM set_config('app.tenant_id',s.tenant_id::text,true);
 PERFORM set_config('app.store_id',p_store::text,true);
 PERFORM set_config('app.buyer_id',s.owner_id::text,true);
 PERFORM set_config('app.buyer_session_id',s.session_id::text,true);
 PERFORM set_config('app.principal_id','',true);
 PERFORM pg_advisory_xact_lock(hashtextextended('checkout.payment.start|'||s.tenant_id||'|'||p_store||'|'||s.owner_id||'|'||p_key,0));
 IF EXISTS(SELECT 1 FROM checkout.command_results x WHERE x.tenant_id=s.tenant_id AND x.store_id=p_store
  AND x.owner_id=s.owner_id AND x.operation='checkout.payment.start' AND x.idempotency_key=p_key) THEN
  RAISE EXCEPTION 'payment already recorded' USING ERRCODE='PT409'; END IF;
 SELECT x.* INTO o FROM checkout.orders x WHERE x.tenant_id=s.tenant_id AND x.store_id=p_store
  AND x.owner_id=s.owner_id AND x.id=p_order FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'order unavailable' USING ERRCODE='PT409'; END IF;
 SELECT x.* INTO r FROM inventory.reservations x WHERE x.tenant_id=s.tenant_id
  AND x.store_id=p_store AND x.id=p_order FOR UPDATE;
 IF NOT FOUND OR o.commercial_state<>'DRAFT' OR r.state<>'HELD' OR r.checkout_id<>o.id
  OR r.buyer_owner_id<>o.owner_id OR r.buyer_session_id<>o.creator_session_id
  OR r.generation<>o.generation THEN
  RAISE EXCEPTION 'order hold unavailable' USING ERRCODE='PT409'; END IF;
 SELECT x.active INTO v_active FROM pricing.markets x WHERE x.tenant_id=s.tenant_id
  AND x.store_id=p_store AND x.id=o.market_id AND x.currency=o.currency FOR SHARE;
 IF NOT FOUND OR NOT v_active THEN RAISE EXCEPTION 'payment market unavailable' USING ERRCODE='PT409'; END IF;
 SELECT h.current_version INTO v_version FROM payments.method_heads h WHERE h.tenant_id=s.tenant_id
  AND h.store_id=p_store AND h.market_id=o.market_id AND h.country=o.country AND h.code=p_method FOR SHARE;
 IF NOT FOUND OR v_version<>p_version THEN RAISE EXCEPTION 'payment method changed' USING ERRCODE='PT409'; END IF;
 SELECT x.* INTO m FROM payments.method_versions x WHERE x.tenant_id=s.tenant_id AND x.store_id=p_store
  AND x.market_id=o.market_id AND x.country=o.country AND x.code=p_method AND x.version=p_version;
 IF NOT FOUND OR NOT m.enabled OR NOT m.visible OR m.connection_id IS NULL OR m.qualification_id IS NULL
  OR m.provider<>'stripe' OR m.currency<>o.currency OR NOT payments.stripe_amount_ok(o.currency,o.total_minor)
  OR o.total_minor<m.min_amount_minor OR o.total_minor>m.max_amount_minor THEN
  RAISE EXCEPTION 'payment method unavailable' USING ERRCODE='PT409'; END IF;
 SELECT x.* INTO a FROM integration.merchant_accounts x WHERE x.tenant_id=s.tenant_id
  AND x.store_id=p_store AND x.id=m.connection_id FOR SHARE;
 IF NOT FOUND OR a.provider<>'stripe' OR a.environment<>m.environment OR a.environment<>'SANDBOX' THEN
  RAISE EXCEPTION 'payment account unavailable' USING ERRCODE='PT409'; END IF;
 SELECT x.* INTO b FROM integration.bindings x WHERE x.tenant_id=s.tenant_id AND x.store_id=p_store
  AND x.id=a.binding_id FOR SHARE;
 IF NOT FOUND OR NOT b.enabled OR b.semantic_version<>m.binding_version OR b.provider<>a.provider
  OR b.external_asset_id<>a.binding_asset THEN
  RAISE EXCEPTION 'payment binding unavailable' USING ERRCODE='PT409'; END IF;
 SELECT x.* INTO q FROM payments.account_qualifications x WHERE x.tenant_id=s.tenant_id
  AND x.store_id=p_store AND x.id=m.qualification_id FOR SHARE;
 IF NOT FOUND OR q.connection_id<>a.id OR q.credential_version<>a.credential_version
  OR q.environment<>a.environment OR q.code<>p_method OR q.revoked_at IS NOT NULL
  OR q.proof_class<>(CASE WHEN p_profile='PROVIDER_MOCK' THEN 'PROVIDER_MOCK' ELSE 'REAL_SANDBOX' END) THEN
  RAISE EXCEPTION 'payment qualification unavailable' USING ERRCODE='PT409'; END IF;
 v_now:=clock_timestamp();
 IF o.expires_at<=v_now OR r.expires_at<=v_now OR q.observed_at>v_now OR q.expires_at<=v_now THEN
  RAISE EXCEPTION 'payment admission expired' USING ERRCODE='PT409'; END IF;
 IF NOT EXISTS(SELECT 1 FROM river_payment.river_job j WHERE j.id=p_job AND j.kind='payment_query_v1'
  AND j.args=jsonb_build_object('operation_id',p_attempt::text,'version',1)) THEN
  RAISE EXCEPTION 'payment query job missing' USING ERRCODE='PT409'; END IF;
 v_trade:='P'||rtrim(translate(encode(uuid_send(p_attempt),'base64'),'+/','-_'),'=');
 v_expiry:=date_trunc('second',v_now)+interval '40 minutes';
 v_locale:=CASE p_locale WHEN 'zh-CN' THEN 'zh' ELSE p_locale END;
 v_params:=jsonb_build_object(
  'adaptive_pricing[enabled]','false','managed_payments[enabled]','false',
  'automatic_tax[enabled]','false','metadata[lc_attempt]',p_attempt::text,
  'cancel_url',p_return_url,'metadata[lc_order]',p_order::text,
  'client_reference_id',p_attempt::text,'metadata[lc_profile]',p_profile,
  'expires_at',extract(epoch FROM v_expiry)::bigint::text,'metadata[lc_v]','1',
  'line_items[0][price_data][currency]',lower(o.currency),'mode','payment',
  'line_items[0][price_data][product_data][name]','Order '||upper(substr(replace(p_order::text,'-',''),1,8)),
  'line_items[0][price_data][unit_amount]',payments.stripe_unit_amount(o.currency,o.total_minor)::text,
  'line_items[0][quantity]','1','payment_intent_data[metadata][lc_attempt]',p_attempt::text,
  'locale',v_locale,'payment_intent_data[metadata][lc_order]',p_order::text,
  'payment_method_types[0]','card','submit_type','pay',
  'success_url',p_return_url,'ui_mode','hosted_page');
 INSERT INTO checkout.payment_attempts(tenant_id,store_id,owner_id,id,session_id,order_id,market_id,country,
  method_code,method_version,connection_id,credential_version,qualification_id,environment,execution_profile,
  binding_id,binding_version,currency,amount_minor,merchant_trade_no,state,generation,job_id,created_at)
 VALUES(s.tenant_id,p_store,s.owner_id,p_attempt,s.session_id,p_order,o.market_id,o.country,p_method,p_version,
  a.id,a.credential_version,q.id,a.environment,p_profile,b.id,b.semantic_version,o.currency,o.total_minor,
  v_trade,'PAYMENT_PENDING',o.generation+1,p_job,v_now);
 INSERT INTO integration.operations(id,tenant_id,store_id,principal_id,binding_id,binding_version,provider,
  external_asset_id,purpose,action,semantic_key,request_hash,request,job_id,state,generation,
  actor_kind,payment_attempt_id,buyer_owner_id,buyer_session_id)
 VALUES(p_attempt,s.tenant_id,p_store,NULL,b.id,b.semantic_version,'stripe',b.external_asset_id,
  'transactional','stripe.checkout_session','payment.stripe:'||p_attempt,p_request_hash,
  jsonb_build_object('attempt_id',p_attempt::text),p_job,'UNKNOWN',1,
  'BUYER_PAYMENT_QUERY',p_attempt,s.owner_id,s.session_id);
 INSERT INTO payments.stripe_sessions(tenant_id,store_id,owner_id,attempt_id,environment,account_id,
  locale,config_digest,unit_amount,create_params,attempt_created_at,expires_at,send_deadline,handoff_cutoff)
 VALUES(s.tenant_id,p_store,s.owner_id,p_attempt,a.environment,a.account_id,
  p_locale,p_config_digest,o.total_minor,v_params,v_now,v_expiry,v_now+interval '7 minutes',
  v_expiry-interval '5 minutes');
 INSERT INTO integration.operation_events(tenant_id,store_id,operation_id,generation,state,mode,reason_code)
 VALUES(s.tenant_id,p_store,p_attempt,1,'UNKNOWN','','buyer_payment_started');
 UPDATE checkout.orders SET commercial_state='AWAITING_PAYMENT',generation=o.generation+1,
  updated_at=v_now WHERE id=p_order;
 PERFORM set_config('app.buyer_session_id',o.creator_session_id::text,true);
 UPDATE inventory.reservations SET state='PAYMENT_PENDING',generation=o.generation+1
  WHERE tenant_id=s.tenant_id AND store_id=p_store AND id=p_order;
 PERFORM set_config('app.buyer_session_id',s.session_id::text,true);
 INSERT INTO checkout.events(tenant_id,store_id,owner_id,order_id,session_id,generation,action,actor_kind)
 VALUES(s.tenant_id,p_store,s.owner_id,p_order,s.session_id,o.generation+1,'checkout.payment_started','BUYER');
 v_result:=jsonb_build_object('order_id',p_order::text,'attempt_id',p_attempt::text,
  'operation_id',p_attempt::text,'job_id',p_job,'generation',o.generation+1,
  'merchant_trade_no',v_trade,'currency',o.currency,'amount_minor',o.total_minor,'state','PAYMENT_PENDING');
 INSERT INTO checkout.command_results(tenant_id,store_id,owner_id,operation,idempotency_key,
  creator_session_id,request_hash,order_id,response)
 VALUES(s.tenant_id,p_store,s.owner_id,'checkout.payment.start',p_key,s.session_id,p_request_hash,p_order,v_result);
 SELECT * INTO sf FROM buyer.resolve_scope(p_hash,p_store);
 IF NOT FOUND OR sf.tenant_id<>s.tenant_id OR sf.owner_id<>s.owner_id OR sf.session_id<>s.session_id THEN
  RAISE EXCEPTION 'buyer capability expired' USING ERRCODE='PT401'; END IF;
 v_now:=clock_timestamp();
 IF o.expires_at<=v_now OR r.expires_at<=v_now OR q.expires_at<=v_now THEN
  RAISE EXCEPTION 'payment admission expired' USING ERRCODE='PT409'; END IF;
 RETURN v_result;
END $$;
ALTER FUNCTION checkout.start_stripe_payment(bytea,uuid,text,bytea,uuid,text,bigint,text,uuid,bigint,text,bytea,text)
 OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION checkout.start_stripe_payment(bytea,uuid,text,bytea,uuid,text,bigint,text,uuid,bigint,text,bytea,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION checkout.start_stripe_payment(bytea,uuid,text,bytea,uuid,text,bigint,text,uuid,bigint,text,bytea,text) TO commerce_hosted_runtime;
COMMENT ON FUNCTION checkout.start_stripe_payment(bytea,uuid,text,bytea,uuid,text,bigint,text,uuid,bigint,text,bytea,text) IS 'checkout owner; hosted runtime atomically starts a scoped Stripe attempt and same-tx River query; no PSP send or card data';

CREATE FUNCTION checkout.request_stripe_signal(p_hash bytea,p_store uuid,p_order uuid,
 p_profile text,p_config_digest bytea,p_kind text,p_signal uuid,p_job bigint) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE scope record; sf record; ord checkout.orders%ROWTYPE;
 a checkout.payment_attempts%ROWTYPE; s payments.stripe_sessions%ROWTYPE;
 v_now timestamptz; v_source text;
BEGIN
 IF current_setting('transaction_isolation')<>'read committed'
  OR p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR p_order IS NULL
  OR p_profile NOT IN ('PROVIDER_MOCK','SANDBOX')
  OR p_config_digest IS NULL OR octet_length(p_config_digest)<>32
  OR p_kind NOT IN ('REFRESH','CANCEL') OR p_signal IS NULL OR p_job IS NULL OR p_job<1 THEN
  RAISE EXCEPTION 'invalid Stripe signal input' USING ERRCODE='PT400'; END IF;
 SELECT * INTO scope FROM buyer.resolve_scope(p_hash,p_store);
 IF NOT FOUND THEN RAISE EXCEPTION 'invalid buyer capability' USING ERRCODE='PT401'; END IF;
 PERFORM set_config('app.tenant_id',scope.tenant_id::text,true);
 PERFORM set_config('app.store_id',p_store::text,true);
 PERFORM set_config('app.buyer_id',scope.owner_id::text,true);
 PERFORM set_config('app.buyer_session_id',scope.session_id::text,true);
 PERFORM set_config('app.principal_id','',true);
 SELECT x.* INTO ord FROM checkout.orders x WHERE x.tenant_id=scope.tenant_id
  AND x.store_id=p_store AND x.owner_id=scope.owner_id AND x.id=p_order FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'order unavailable' USING ERRCODE='PT404'; END IF;
 SELECT x.* INTO a FROM checkout.payment_attempts x WHERE x.tenant_id=scope.tenant_id
  AND x.store_id=p_store AND x.owner_id=scope.owner_id AND x.order_id=p_order;
 IF NOT FOUND OR a.method_code<>'stripe_checkout' OR a.execution_profile<>p_profile THEN
  RAISE EXCEPTION 'Stripe attempt unavailable' USING ERRCODE='PT409'; END IF;
 SELECT x.* INTO s FROM payments.stripe_sessions x WHERE x.attempt_id=a.id FOR UPDATE;
 IF NOT FOUND OR s.config_digest<>p_config_digest THEN
  RAISE EXCEPTION 'Stripe session unavailable' USING ERRCODE='PT409'; END IF;
 IF EXISTS(SELECT 1 FROM payments.facts f WHERE f.tenant_id=a.tenant_id
  AND f.store_id=a.store_id AND f.attempt_id=a.id AND f.kind IN ('CAPTURED','CLOSED_UNPAID')) THEN
  RETURN jsonb_build_object('order_id',p_order,'scheduled',false); END IF;
 v_now:=clock_timestamp();
 IF p_kind='REFRESH' THEN
  IF s.refresh_count>=30 OR (s.last_refresh_at IS NOT NULL
   AND v_now<s.last_refresh_at+interval '10 seconds') THEN
   RETURN jsonb_build_object('order_id',p_order,'scheduled',false); END IF;
  v_source:='BUYER_REFRESH';
  UPDATE payments.stripe_sessions SET refresh_count=refresh_count+1,last_refresh_at=v_now
   WHERE attempt_id=a.id;
 ELSE
  IF s.cancel_requested_at IS NOT NULL THEN
   RETURN jsonb_build_object('order_id',p_order,'scheduled',false); END IF;
  v_source:='BUYER_CANCEL';
  UPDATE payments.stripe_sessions SET cancel_requested_at=v_now WHERE attempt_id=a.id;
 END IF;
 IF s.signal_count>=64 OR NOT EXISTS(SELECT 1 FROM river_payment.river_job j
  WHERE j.id=p_job AND j.kind='payment_signal_v1' AND j.state='available' AND j.attempt=0
   AND j.args=jsonb_build_object('operation_id',a.id::text,'signal_id',p_signal::text,'version',1)) THEN
  RAISE EXCEPTION 'Stripe signal job unavailable' USING ERRCODE='PT409'; END IF;
 INSERT INTO payments.stripe_signals(id,tenant_id,store_id,attempt_id,source,job_id)
 VALUES(p_signal,a.tenant_id,a.store_id,a.id,v_source,p_job);
 UPDATE payments.stripe_sessions SET signal_count=signal_count+1 WHERE attempt_id=a.id;
 SELECT * INTO sf FROM buyer.resolve_scope(p_hash,p_store);
 IF NOT FOUND OR sf.tenant_id<>scope.tenant_id OR sf.owner_id<>scope.owner_id
  OR sf.session_id<>scope.session_id THEN
  RAISE EXCEPTION 'buyer capability expired' USING ERRCODE='PT401'; END IF;
 RETURN jsonb_build_object('order_id',p_order,'scheduled',true);
END $$;
ALTER FUNCTION checkout.request_stripe_signal(bytea,uuid,uuid,text,bytea,text,uuid,bigint)
 OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION checkout.request_stripe_signal(bytea,uuid,uuid,text,bytea,text,uuid,bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION checkout.request_stripe_signal(bytea,uuid,uuid,text,bytea,text,uuid,bigint)
 TO commerce_hosted_runtime;
COMMENT ON FUNCTION checkout.request_stripe_signal(bytea,uuid,uuid,text,bytea,text,uuid,bigint) IS 'checkout owner; hosted runtime atomically records bounded buyer refresh/cancel with one linked River signal; no immediate stock release';

CREATE FUNCTION payments.stripe_webhook_commit(p_receipt uuid,p_signal uuid,p_job bigint)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE r payments.stripe_webhook_receipts%ROWTYPE; s payments.stripe_sessions%ROWTYPE;
BEGIN
 IF p_receipt IS NULL OR p_signal IS NULL OR p_job IS NULL OR p_job<1 THEN
  RAISE EXCEPTION 'invalid Stripe webhook commit' USING ERRCODE='22023'; END IF;
 SELECT x.* INTO r FROM payments.stripe_webhook_receipts x WHERE x.id=p_receipt FOR SHARE;
 IF NOT FOUND OR r.disposition<>'ACCEPTED' OR r.signal_id<>p_signal
  OR r.attempt_id IS NULL OR r.tenant_id IS NULL OR r.store_id IS NULL THEN
  RAISE EXCEPTION 'Stripe receipt unavailable' USING ERRCODE='PT409'; END IF;
 SELECT x.* INTO s FROM payments.stripe_sessions x WHERE x.attempt_id=r.attempt_id
  AND x.tenant_id=r.tenant_id AND x.store_id=r.store_id FOR UPDATE;
 IF NOT FOUND OR s.account_id<>r.account_id OR s.environment<>r.environment
  OR s.signal_count>=64 THEN
  RAISE EXCEPTION 'Stripe signal scope unavailable' USING ERRCODE='PT409'; END IF;
 IF NOT EXISTS(SELECT 1 FROM river_payment.river_job j WHERE j.id=p_job
  AND j.kind='payment_signal_v1' AND j.state='available' AND j.attempt=0
  AND j.unique_key IS NULL
  AND j.args=jsonb_build_object('operation_id',r.attempt_id::text,
   'signal_id',p_signal::text,'version',1)) THEN
  RAISE EXCEPTION 'Stripe signal job unavailable' USING ERRCODE='PT409'; END IF;
 INSERT INTO payments.stripe_signals(id,tenant_id,store_id,attempt_id,source,receipt_id,session_id,job_id)
 VALUES(p_signal,r.tenant_id,r.store_id,r.attempt_id,'STRIPE_WEBHOOK',r.id,r.session_id,p_job);
 UPDATE payments.stripe_sessions SET signal_count=signal_count+1 WHERE attempt_id=r.attempt_id;
END $$;
ALTER FUNCTION payments.stripe_webhook_commit(uuid,uuid,bigint) OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION payments.stripe_webhook_commit(uuid,uuid,bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION payments.stripe_webhook_commit(uuid,uuid,bigint) TO commerce_stripe_ingress;
COMMENT ON FUNCTION payments.stripe_webhook_commit(uuid,uuid,bigint) IS 'payments owner; signed ingress links preallocated receipt to exact River signal in same transaction; no financial authority';

CREATE FUNCTION payments.guard_stripe_receipt_link() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE r payments.stripe_webhook_receipts%ROWTYPE; s payments.stripe_signals%ROWTYPE;
 j river_payment.river_job%ROWTYPE; expected text;
BEGIN
 SELECT x.* INTO r FROM payments.stripe_webhook_receipts x WHERE x.id=NEW.id;
 IF NOT FOUND OR r.disposition<>'ACCEPTED' OR r.signal_id IS NULL
  OR r.attempt_id IS NULL OR r.tenant_id IS NULL OR r.store_id IS NULL THEN
  RAISE EXCEPTION 'Stripe receipt linkage missing' USING ERRCODE='23514'; END IF;
 SELECT x.* INTO s FROM payments.stripe_signals x WHERE x.id=r.signal_id
  AND x.receipt_id=r.id AND x.source='STRIPE_WEBHOOK' AND x.tenant_id=r.tenant_id
  AND x.store_id=r.store_id AND x.attempt_id=r.attempt_id
  AND x.session_id IS NOT DISTINCT FROM r.session_id;
 IF NOT FOUND THEN RAISE EXCEPTION 'Stripe signal linkage missing' USING ERRCODE='23514'; END IF;
 SELECT x.* INTO j FROM river_payment.river_job x WHERE x.id=s.job_id;
 expected:=integration.payment_job_queue(s.job_id);
 IF NOT FOUND OR j.kind<>'payment_signal_v1' OR j.state<>'available'
  OR j.attempt<>0 OR j.unique_key IS NOT NULL
  OR j.args IS DISTINCT FROM jsonb_build_object('operation_id',r.attempt_id::text,
   'signal_id',r.signal_id::text,'version',1)
  OR expected IS NULL OR j.queue NOT IN ('default',expected) THEN
  RAISE EXCEPTION 'Stripe River linkage missing' USING ERRCODE='23514'; END IF;
 RETURN NULL;
END $$;
ALTER FUNCTION payments.guard_stripe_receipt_link() OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION payments.guard_stripe_receipt_link() FROM PUBLIC;
CREATE CONSTRAINT TRIGGER stripe_receipt_link AFTER INSERT ON payments.stripe_webhook_receipts
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.disposition='ACCEPTED')
 EXECUTE FUNCTION payments.guard_stripe_receipt_link();
COMMENT ON FUNCTION payments.guard_stripe_receipt_link() IS 'payments owner; deferred commit gate rejects accepted receipt without exact signal and River job; no early webhook ACK';

CREATE FUNCTION integration.record_stripe_observation(p_id uuid,p_generation bigint,p_token bytea,
 p_profile text,p_report jsonb,p_reconcile_job bigint,p_session_url text) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE a checkout.payment_attempts%ROWTYPE; s payments.stripe_sessions%ROWTYPE;
 op integration.operations%ROWTYPE; v_hash bytea; v_lease timestamptz;
 v_keys text[]:=ARRAY['Provider','Version','Via','AccountID','KeyVersion','RequestID',
  'SendCount','SessionID','Status','PaymentStatus','Livemode','Currency','AmountTotal',
  'AmountSubtotal','AmountDiscount','AmountTax','AmountShipping','PresentmentCurrency',
  'PresentmentAmount','CurrencyConversion','ClientReferenceID','MetadataAttempt',
  'MetadataProfile','ExpiresAt','Created','Mode','PaymentMethodTypes','PaymentIntentID',
  'PaymentIntentStatus','PaymentIntentAmountReceived','PaymentIntentCurrency',
  'ErrorClass','ErrorCode','HTTPStatus','ListMatchCount','LocalReason'];
 v_key text; v_value jsonb; v_source text; v_session text;
BEGIN
 a:=integration.require_stripe_query(p_id,p_generation,p_token,p_profile);
 IF p_report IS NULL OR jsonb_typeof(p_report)<>'object' OR octet_length(p_report::text)>2048
  OR (SELECT count(*) FROM jsonb_object_keys(p_report))<>array_length(v_keys,1)
  OR NOT p_report ?& v_keys THEN
  RAISE EXCEPTION 'invalid Stripe observation shape' USING ERRCODE='22023'; END IF;
 FOR v_key,v_value IN SELECT e.key,e.value FROM jsonb_each(p_report) e LOOP
  IF v_key IN ('Version','KeyVersion','SendCount','AmountTotal','AmountSubtotal','AmountDiscount',
   'AmountTax','AmountShipping','PresentmentAmount','ExpiresAt','Created',
   'PaymentIntentAmountReceived','HTTPStatus','ListMatchCount') THEN
   IF v_value<>'null'::jsonb AND (jsonb_typeof(v_value)<>'number'
    OR v_value::text !~ '^(0|[1-9][0-9]{0,17})$') THEN
    RAISE EXCEPTION 'invalid Stripe observation number' USING ERRCODE='22023'; END IF;
  ELSIF v_key IN ('Livemode','CurrencyConversion') THEN
   IF jsonb_typeof(v_value)<>'boolean' THEN
    RAISE EXCEPTION 'invalid Stripe observation boolean' USING ERRCODE='22023'; END IF;
  ELSIF v_key='PaymentMethodTypes' THEN
   IF jsonb_typeof(v_value)<>'array' THEN
    RAISE EXCEPTION 'invalid Stripe observation method list' USING ERRCODE='22023'; END IF;
  ELSIF jsonb_typeof(v_value)<>'string' THEN
   RAISE EXCEPTION 'invalid Stripe observation string' USING ERRCODE='22023';
  END IF;
 END LOOP;
 IF p_report->>'Provider'<>'stripe' OR p_report->'Version'<>'1'::jsonb
  OR p_report->>'Via' NOT IN ('create','retrieve','expire','list','unsent','escalate')
  OR p_report->>'AccountID' !~ '^acct_[A-Za-z0-9]{1,59}$'
  OR p_report->>'RequestID' !~ '^[A-Za-z0-9_]{0,64}$'
  OR p_report->>'SessionID' !~ '^[A-Za-z0-9_]{0,255}$'
  OR p_report->>'PaymentIntentID' !~ '^[A-Za-z0-9_]{0,255}$'
  OR p_report->>'Currency' !~ '^([A-Z]{3})?$'
  OR p_report->>'ErrorClass' NOT IN ('','rejected') THEN
  RAISE EXCEPTION 'invalid Stripe observation value' USING ERRCODE='22023'; END IF;
 v_source:=CASE WHEN p_report->>'Via' IN ('unsent','escalate') THEN 'LOCAL' ELSE 'QUERY' END;
 IF (v_source='LOCAL' AND (p_report->'KeyVersion' IS DISTINCT FROM '0'::jsonb
   OR p_report->>'SessionID'<>'' OR p_report->>'RequestID'<>''))
  OR (v_source='QUERY' AND (p_report->'KeyVersion')::bigint IS DISTINCT FROM a.credential_version) THEN
  RAISE EXCEPTION 'Stripe observation source mismatch' USING ERRCODE='PT409'; END IF;
 SELECT x.* INTO s FROM payments.stripe_sessions x WHERE x.attempt_id=a.id FOR UPDATE;
 SELECT x.* INTO op FROM integration.operations x WHERE x.id=a.id;
 IF s.attempt_id IS NULL OR op.id IS NULL
  OR s.account_id<>p_report->>'AccountID' OR op.provider<>'stripe'
  OR s.environment<>a.environment THEN
  RAISE EXCEPTION 'Stripe observation account mismatch' USING ERRCODE='PT409'; END IF;
 v_session:=p_report->>'SessionID';
 IF v_session<>'' THEN
  IF p_report->>'ClientReferenceID' IS DISTINCT FROM a.id::text
   OR p_report->>'MetadataAttempt' IS DISTINCT FROM a.id::text
   OR p_report->>'MetadataProfile' IS DISTINCT FROM a.execution_profile
   OR p_report->'Livemode' IS DISTINCT FROM to_jsonb(a.environment='LIVE')
   OR (p_report->>'ExpiresAt')::bigint IS DISTINCT FROM extract(epoch FROM s.expires_at)::bigint
   OR p_report->>'Mode' IS DISTINCT FROM 'payment'
   OR p_report->'PaymentMethodTypes' IS DISTINCT FROM '["card"]'::jsonb THEN
   RAISE EXCEPTION 'Stripe session identity mismatch' USING ERRCODE='PT409'; END IF;
  IF s.session_id IS NULL THEN
   IF p_session_url IS NOT NULL AND (p_report->>'Via') NOT IN ('create','list','retrieve') THEN
    RAISE EXCEPTION 'Stripe URL pin mismatch' USING ERRCODE='PT409'; END IF;
   UPDATE payments.stripe_sessions SET session_id=v_session,pinned_at=clock_timestamp(),
    payment_intent_id=nullif(p_report->>'PaymentIntentID',''),session_url=p_session_url
    WHERE attempt_id=a.id;
   UPDATE integration.operations SET provider_reference=v_session WHERE id=a.id
    AND provider_reference='';
  ELSIF s.session_id=v_session THEN
   IF op.provider_reference IS DISTINCT FROM v_session THEN
    RAISE EXCEPTION 'Stripe operation reference mismatch' USING ERRCODE='PT409'; END IF;
   IF p_session_url IS NOT NULL THEN
    IF s.session_url IS NOT NULL AND s.session_url<>p_session_url OR s.url_purged_at IS NOT NULL THEN
     RAISE EXCEPTION 'Stripe URL changed' USING ERRCODE='PT409'; END IF;
    UPDATE payments.stripe_sessions SET session_url=p_session_url WHERE attempt_id=a.id
     AND session_url IS NULL;
   END IF;
   IF s.payment_intent_id IS NULL AND p_report->>'PaymentIntentID'<>'' THEN
    UPDATE payments.stripe_sessions SET payment_intent_id=p_report->>'PaymentIntentID'
     WHERE attempt_id=a.id;
   ELSIF s.payment_intent_id IS NOT NULL AND p_report->>'PaymentIntentID' NOT IN ('',s.payment_intent_id) THEN
    RAISE EXCEPTION 'Stripe payment intent changed' USING ERRCODE='PT409'; END IF;
  ELSIF p_session_url IS NOT NULL THEN
   RAISE EXCEPTION 'duplicate Stripe session URL cannot be handed out' USING ERRCODE='PT409';
  END IF;
 ELSE
  IF p_session_url IS NOT NULL OR p_report->>'ClientReferenceID'<>''
   OR p_report->>'MetadataAttempt'<>'' OR p_report->>'PaymentIntentID'<>'' THEN
   RAISE EXCEPTION 'Stripe empty identity mismatch' USING ERRCODE='PT409'; END IF;
  IF p_report->>'Via'='unsent' THEN
   IF s.session_id IS NOT NULL OR s.create_first_sent_at IS NOT NULL
    OR NOT (s.cancel_requested_at IS NOT NULL OR clock_timestamp()>=s.send_deadline) THEN
    RAISE EXCEPTION 'Stripe unsent closure unavailable' USING ERRCODE='PT409'; END IF;
   UPDATE payments.stripe_sessions SET create_suppressed_at=coalesce(create_suppressed_at,clock_timestamp())
    WHERE attempt_id=a.id;
  ELSIF p_report->>'Via'='list' AND (p_report->>'ListMatchCount')::integer=0 THEN
   IF s.session_id IS NOT NULL OR s.create_first_sent_at IS NULL
    OR clock_timestamp()<s.expires_at+interval '15 minutes' THEN
    RAISE EXCEPTION 'Stripe list closure unavailable' USING ERRCODE='PT409'; END IF;
  ELSIF p_report->>'Via'='create' AND p_report->>'ErrorClass'='rejected' THEN
   IF s.session_id IS NOT NULL OR s.create_send_count<>1 THEN
    RAISE EXCEPTION 'Stripe rejected create mismatch' USING ERRCODE='PT409'; END IF;
  ELSIF p_report->>'Via' NOT IN ('escalate') THEN
   RAISE EXCEPTION 'Stripe missing session identity' USING ERRCODE='PT409'; END IF;
 END IF;
 v_hash:=sha256(convert_to(p_report::text,'UTF8'));
 IF p_reconcile_job IS NULL OR p_reconcile_job<1 OR NOT EXISTS(
  SELECT 1 FROM river_payment.river_job j WHERE j.id=p_reconcile_job
   AND j.kind='payment_reconcile_v1' AND j.state='available' AND j.attempt=0
   AND j.args=jsonb_build_object('operation_id',a.id::text,'report_hash',encode(v_hash,'hex'),'version',1)) THEN
  RAISE EXCEPTION 'Stripe reconcile job missing' USING ERRCODE='PT409'; END IF;
 INSERT INTO payments.provider_observations(tenant_id,store_id,attempt_id,source,execution_profile,
  environment,first_generation,report,report_hash)
 VALUES(a.tenant_id,a.store_id,a.id,v_source,a.execution_profile,a.environment,p_generation,p_report,v_hash)
 ON CONFLICT(tenant_id,store_id,attempt_id,report_hash) DO NOTHING;
 SELECT x.lease_until INTO v_lease FROM integration.operations x WHERE x.id=a.id;
 PERFORM integration.complete_operation(p_id,p_generation,p_token,'UNKNOWN','stripe_report_observed',
  coalesce((SELECT x.provider_reference FROM integration.operations x WHERE x.id=a.id),''));
 IF clock_timestamp()>=v_lease THEN
  RAISE EXCEPTION 'Stripe query lease conflict' USING ERRCODE='40001'; END IF;
END $$;
ALTER FUNCTION integration.record_stripe_observation(uuid,bigint,bytea,text,jsonb,bigint,text)
 OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION integration.record_stripe_observation(uuid,bigint,bytea,text,jsonb,bigint,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION integration.record_stripe_observation(uuid,bigint,bytea,text,jsonb,bigint,text)
 TO commerce_worker;
COMMENT ON FUNCTION integration.record_stripe_observation(uuid,bigint,bytea,text,jsonb,bigint,text) IS 'integration owner; worker validates exact Stripe projection and pins one session under lease, then records immutable observation and same-tx reconcile; no direct money authority';

-- A signal job may not use load_stripe_session's latest-candidate poller hint:
-- another signal can arrive while this job waits. Resolve this job's exact row
-- using the same operation lease, without exposing the private signal table.
CREATE FUNCTION integration.load_stripe_signal(p_operation uuid,p_generation bigint,
 p_token bytea,p_profile text,p_job bigint,p_signal uuid)
RETURNS TABLE(source text,session_id text,created_at timestamptz,consumed_at timestamptz,db_now timestamptz)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE a checkout.payment_attempts%ROWTYPE; s payments.stripe_signals%ROWTYPE;
BEGIN
 IF p_job IS NULL OR p_job<1 OR p_signal IS NULL THEN
  RAISE EXCEPTION 'invalid Stripe signal identity' USING ERRCODE='22023'; END IF;
 a:=integration.require_stripe_query(p_operation,p_generation,p_token,p_profile);
 SELECT x.* INTO s FROM payments.stripe_signals x
  WHERE x.id=p_signal AND x.job_id=p_job AND x.attempt_id=a.id
   AND x.tenant_id=a.tenant_id AND x.store_id=a.store_id;
 IF NOT FOUND OR NOT EXISTS(SELECT 1 FROM river_payment.river_job j
  WHERE j.id=p_job AND j.kind='payment_signal_v1' AND j.unique_key IS NULL
   AND j.args=jsonb_build_object('operation_id',a.id::text,'signal_id',s.id::text,'version',1)
   AND j.queue=CASE a.execution_profile WHEN 'PROVIDER_MOCK' THEN 'payment_mock_v1'
    WHEN 'SANDBOX' THEN 'payment_sandbox_v1' WHEN 'LIVE' THEN 'payment_live_v1' END) THEN
  RAISE EXCEPTION 'Stripe signal unavailable' USING ERRCODE='PT409'; END IF;
 -- A repeated job may see consumed_at and finish without a second observation.
 -- Read/consume/record are serialized by require_stripe_query's operation lock.
 PERFORM integration.require_stripe_query(p_operation,p_generation,p_token,p_profile);
 RETURN QUERY SELECT s.source,s.session_id,s.created_at,s.consumed_at,clock_timestamp();
END $$;
ALTER FUNCTION integration.load_stripe_signal(uuid,bigint,bytea,text,bigint,uuid) OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION integration.load_stripe_signal(uuid,bigint,bytea,text,bigint,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION integration.load_stripe_signal(uuid,bigint,bytea,text,bigint,uuid) TO commerce_worker;
COMMENT ON FUNCTION integration.load_stripe_signal(uuid,bigint,bytea,text,bigint,uuid) IS 'integration owner; exact River signal metadata under matching operation lease and profile; no table access, secrets or financial authority';
