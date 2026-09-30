-- post_river/0016 Stripe LIVE admission in the post-River definers (contracts/stripe-live-enable-v1.md §3.4 "Re-created
-- post-River functions", LR-1 ruling S1: 0015 belongs to meta-ads). Same signatures, owners and ACLs as post_river/0012 and
-- 0013; only the marked lines change. Owner package: checkout (start/signal), integration (observations), payments (refund).
-- Non-goals: no approval/qualification/method dependency in the refund path (LD7); the kill switch stays the qualification's
-- revoked_at, which start already rejects with PT409.

-- delta: profile LIVE admitted; a.environment = m.environment = the profile's environment; proof_class follows the profile.
CREATE OR REPLACE FUNCTION checkout.start_stripe_payment(p_hash bytea,p_store uuid,p_key text,p_request_hash bytea,
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
 OR p_version IS NULL OR p_version<1 OR p_profile NOT IN ('PROVIDER_MOCK','SANDBOX','LIVE')
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
 IF NOT FOUND OR a.provider<>'stripe' OR a.environment<>m.environment
  OR a.environment<>(CASE WHEN p_profile='LIVE' THEN 'LIVE' ELSE 'SANDBOX' END) THEN
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
  OR q.proof_class<>(CASE p_profile WHEN 'PROVIDER_MOCK' THEN 'PROVIDER_MOCK' WHEN 'SANDBOX' THEN 'REAL_SANDBOX'
   ELSE 'REAL_LIVE' END) THEN
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

-- delta: profile LIVE admitted.
CREATE OR REPLACE FUNCTION checkout.request_stripe_signal(p_hash bytea,p_store uuid,p_order uuid,
 p_profile text,p_config_digest bytea,p_kind text,p_signal uuid,p_job bigint) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE scope record; sf record; ord checkout.orders%ROWTYPE;
 a checkout.payment_attempts%ROWTYPE; s payments.stripe_sessions%ROWTYPE;
 v_now timestamptz; v_source text;
BEGIN
 IF current_setting('transaction_isolation')<>'read committed'
  OR p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR p_order IS NULL
  OR p_profile NOT IN ('PROVIDER_MOCK','SANDBOX','LIVE')
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

-- delta: Livemode must equal to_jsonb(a.environment='LIVE') (rule 0012 already uses for checkout observations).
CREATE OR REPLACE FUNCTION integration.record_stripe_refund_observation(p_id uuid,p_generation bigint,p_token bytea,
 p_profile text,p_report jsonb,p_reconcile_job bigint) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE r payments.stripe_refunds%ROWTYPE; a checkout.payment_attempts%ROWTYPE;
 m integration.merchant_accounts%ROWTYPE; o integration.operations%ROWTYPE;
 v_keys text[]:=ARRAY['Provider','Version','Object','Via','AccountID','KeyVersion','RequestID',
  'SendCount','RefundRef','AttemptRef','RefundID','Status','FailureReason','PendingReason','Amount',
  'Currency','PaymentIntentID','Livemode','MetadataRefund','MetadataAttempt','ErrorClass','ErrorCode',
  'HTTPStatus','ListMatchCount','LocalReason'];
 v_key text; v_value jsonb; v_via text; v_source text; v_hash bytea; v_lease timestamptz; v_now timestamptz;
BEGIN
 IF p_id IS NULL OR p_generation IS NULL OR p_generation<2 OR p_token IS NULL OR octet_length(p_token)<>32
  OR p_profile IS NULL OR p_profile NOT IN ('PROVIDER_MOCK','SANDBOX','LIVE') THEN
  RAISE EXCEPTION 'invalid Stripe refund observation' USING ERRCODE='22023'; END IF;
 IF p_report IS NULL OR jsonb_typeof(p_report)<>'object' OR octet_length(p_report::text)>2048
  OR (SELECT count(*) FROM jsonb_object_keys(p_report))<>array_length(v_keys,1)
  OR NOT p_report ?& v_keys THEN
  RAISE EXCEPTION 'invalid Stripe refund observation shape' USING ERRCODE='22023'; END IF;
 FOR v_key,v_value IN SELECT e.key,e.value FROM jsonb_each(p_report) e LOOP
  IF v_key IN ('Version','KeyVersion','SendCount','Amount','HTTPStatus','ListMatchCount') THEN
   IF v_value<>'null'::jsonb AND (jsonb_typeof(v_value)<>'number'
    OR v_value::text !~ '^(0|[1-9][0-9]{0,17})$') THEN
    RAISE EXCEPTION 'invalid Stripe refund observation number' USING ERRCODE='22023'; END IF;
   IF v_value='null'::jsonb AND v_key NOT IN ('Amount','ListMatchCount') THEN
    RAISE EXCEPTION 'invalid Stripe refund observation number' USING ERRCODE='22023'; END IF;
  ELSIF v_key='Livemode' THEN
   IF jsonb_typeof(v_value)<>'boolean' THEN
    RAISE EXCEPTION 'invalid Stripe refund observation boolean' USING ERRCODE='22023'; END IF;
  ELSIF jsonb_typeof(v_value)<>'string' THEN
   RAISE EXCEPTION 'invalid Stripe refund observation string' USING ERRCODE='22023';
  END IF;
 END LOOP;
 v_via:=p_report->>'Via';
 IF p_report->>'Provider'<>'stripe' OR p_report->'Version'<>'1'::jsonb OR p_report->>'Object'<>'refund'
  OR v_via NOT IN ('create','retrieve','list','unsent','escalate')
  OR p_report->>'AccountID' !~ '^acct_[A-Za-z0-9]{1,59}$'
  OR p_report->>'RequestID' !~ '^[A-Za-z0-9_]{0,64}$'
  OR p_report->>'RefundRef' !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  OR p_report->>'AttemptRef' !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  OR p_report->>'RefundID' !~ '^[A-Za-z0-9_]{0,255}$'
  OR p_report->>'Status' NOT IN ('','pending','requires_action','succeeded','failed','canceled')
  OR p_report->>'FailureReason' NOT IN ('','lost_or_stolen_card','expired_or_canceled_card',
   'charge_for_pending_refund_disputed','insufficient_funds','declined','merchant_request','unknown')
  OR p_report->>'PendingReason' NOT IN ('','processing','insufficient_funds','charge_pending')
  OR p_report->>'Currency' !~ '^([A-Z]{3})?$'
  OR p_report->>'PaymentIntentID' !~ '^[A-Za-z0-9_]{0,255}$'
  OR p_report->>'MetadataRefund' !~ '^[A-Za-z0-9_-]{0,64}$'
  OR p_report->>'MetadataAttempt' !~ '^[A-Za-z0-9_-]{0,64}$'
  OR p_report->>'ErrorClass' NOT IN ('','rejected')
  OR p_report->>'ErrorCode' !~ '^[a-z_]{0,64}$'
  OR p_report->>'LocalReason' !~ '^[A-Za-z_]{0,64}$' THEN
  RAISE EXCEPTION 'invalid Stripe refund observation value' USING ERRCODE='22023'; END IF;
 v_source:=CASE WHEN v_via IN ('unsent','escalate') THEN 'LOCAL' ELSE 'QUERY' END;
 v_now:=clock_timestamp();
 IF v_via='escalate' THEN
  -- A binding change leaves no lease (claim_operation returned blocked_binding): the operation itself
  -- must prove result_code='binding_changed' with no lease, and the report must say so.
  SELECT x.* INTO o FROM integration.operations x WHERE x.id=p_id AND x.actor_kind='PAYMENT_REFUND'
   AND x.provider='stripe' FOR UPDATE;
  IF NOT FOUND OR o.state<>'UNKNOWN' OR o.lease_until IS NOT NULL OR o.result_code<>'binding_changed'
   OR o.generation<>p_generation OR p_report->>'LocalReason'<>'binding_changed' THEN
   RAISE EXCEPTION 'Stripe refund escalation unavailable' USING ERRCODE='PT409'; END IF;
  SELECT x.* INTO r FROM payments.stripe_refunds x WHERE x.id=p_id AND x.tenant_id=o.tenant_id
   AND x.store_id=o.store_id;
  SELECT x.* INTO a FROM checkout.payment_attempts x WHERE x.id=r.attempt_id AND x.tenant_id=r.tenant_id
   AND x.store_id=r.store_id;
  IF r.id IS NULL OR a.id IS NULL OR a.execution_profile<>p_profile THEN
   RAISE EXCEPTION 'Stripe refund escalation unavailable' USING ERRCODE='PT409'; END IF;
 ELSE
  r:=integration.require_stripe_refund(p_id,p_generation,p_token,p_profile);
  SELECT x.* INTO a FROM checkout.payment_attempts x WHERE x.id=r.attempt_id AND x.tenant_id=r.tenant_id
   AND x.store_id=r.store_id;
  SELECT x.* INTO o FROM integration.operations x WHERE x.id=r.id;
 END IF;
 SELECT x.* INTO m FROM integration.merchant_accounts x WHERE x.id=a.connection_id
  AND x.tenant_id=a.tenant_id AND x.store_id=a.store_id AND x.provider='stripe';
 IF m.id IS NULL OR m.account_id<>r.account_id OR p_report->>'AccountID'<>r.account_id
  OR p_report->>'RefundRef'<>r.id::text OR p_report->>'AttemptRef'<>r.attempt_id::text
  OR (v_source='LOCAL' AND (p_report->'KeyVersion'<>'0'::jsonb OR p_report->>'RequestID'<>''))
  OR (v_source='QUERY' AND ((p_report->'KeyVersion')::bigint<1
   OR (p_report->'KeyVersion')::bigint>m.credential_version)) THEN
  RAISE EXCEPTION 'Stripe refund observation account mismatch' USING ERRCODE='PT409'; END IF;
 IF p_report->>'RefundID'<>'' THEN
  -- RD8/§3.1: the refund's identity is proved by our own metadata, the PaymentIntent, the account and
  -- test mode; any mismatch raises and the worker finishes UNKNOWN stripe_refund_mismatch.
  -- Ruling 23: a non-empty Currency that differs from the request is the one exception: it is recorded
  -- (apply_stripe_refund step 4 opens REFUND_AMOUNT_MISMATCH, no fact, capacity stays held) and the op
  -- completes stripe_refund_mismatch below instead of stripe_refund_observed.
  IF v_source<>'QUERY' OR p_report->>'MetadataRefund'<>r.id::text
   OR p_report->>'MetadataAttempt'<>r.attempt_id::text OR p_report->>'PaymentIntentID'<>r.payment_intent_id
   OR p_report->'Livemode' IS DISTINCT FROM to_jsonb(a.environment='LIVE') OR p_report->>'Currency'=''
   OR p_report->>'Status'='' OR p_report->'Amount'='null'::jsonb THEN
   RAISE EXCEPTION 'Stripe refund identity mismatch' USING ERRCODE='PT409'; END IF;
  IF r.stripe_refund_id IS NULL THEN
   UPDATE payments.stripe_refunds SET stripe_refund_id=p_report->>'RefundID',pinned_at=v_now WHERE id=r.id;
   UPDATE integration.operations SET provider_reference=p_report->>'RefundID' WHERE id=r.id
    AND provider_reference='';
  ELSIF r.stripe_refund_id<>p_report->>'RefundID' THEN
   RAISE EXCEPTION 'Stripe refund pin changed' USING ERRCODE='PT409'; END IF;
  IF v_via='list' AND (p_report->>'ListMatchCount' IS DISTINCT FROM '1') THEN
   RAISE EXCEPTION 'Stripe refund list match mismatch' USING ERRCODE='PT409'; END IF;
 ELSE
  IF p_report->>'Status'<>'' OR p_report->'Amount'<>'null'::jsonb OR p_report->>'Currency'<>''
   OR p_report->>'PaymentIntentID'<>'' OR p_report->>'MetadataRefund'<>'' OR p_report->>'MetadataAttempt'<>''
   OR p_report->>'FailureReason'<>'' OR p_report->>'PendingReason'<>'' THEN
   RAISE EXCEPTION 'Stripe refund empty identity mismatch' USING ERRCODE='PT409'; END IF;
  IF v_via='unsent' THEN
   IF r.first_sent_at IS NOT NULL OR r.stripe_refund_id IS NOT NULL
    OR p_report->>'LocalReason' NOT IN ('external_refund','send_window_closed')
    OR (p_report->>'LocalReason'='external_refund' AND r.suppressed_at IS NULL)
    OR (p_report->>'LocalReason'='send_window_closed' AND v_now<r.resend_until-interval '1 hour') THEN
    RAISE EXCEPTION 'Stripe refund unsent closure unavailable' USING ERRCODE='PT409'; END IF;
   UPDATE payments.stripe_refunds SET suppressed_at=coalesce(suppressed_at,v_now) WHERE id=r.id;
  ELSIF v_via='list' THEN
   IF r.first_sent_at IS NULL OR r.stripe_refund_id IS NOT NULL
    OR p_report->'ListMatchCount'<>'0'::jsonb THEN
    RAISE EXCEPTION 'Stripe refund list closure unavailable' USING ERRCODE='PT409'; END IF;
  ELSIF v_via='create' THEN
   IF p_report->>'ErrorClass'<>'rejected' OR r.first_sent_at IS NULL OR r.send_count<>1
    OR r.stripe_refund_id IS NOT NULL OR p_report->'SendCount'<>'1'::jsonb THEN
    RAISE EXCEPTION 'Stripe rejected refund create mismatch' USING ERRCODE='PT409'; END IF;
  ELSIF v_via<>'escalate' THEN
   RAISE EXCEPTION 'Stripe refund missing identity' USING ERRCODE='PT409';
  END IF;
 END IF;
 v_hash:=sha256(convert_to(p_report::text,'UTF8'));
 IF p_reconcile_job IS NULL OR p_reconcile_job<1 OR NOT EXISTS(
  SELECT 1 FROM river_payment.river_job j WHERE j.id=p_reconcile_job
   AND j.kind='payment_reconcile_v1' AND j.state='available' AND j.attempt=0
   AND j.args=jsonb_build_object('operation_id',r.attempt_id::text,'report_hash',encode(v_hash,'hex'),'version',1)) THEN
  RAISE EXCEPTION 'Stripe refund reconcile job missing' USING ERRCODE='PT409'; END IF;
 INSERT INTO payments.provider_observations(tenant_id,store_id,attempt_id,source,execution_profile,
  environment,first_generation,report,report_hash)
 VALUES(a.tenant_id,a.store_id,a.id,v_source,a.execution_profile,a.environment,p_generation,p_report,v_hash)
 ON CONFLICT(tenant_id,store_id,attempt_id,report_hash) DO NOTHING;
 IF v_via='escalate' THEN RETURN; END IF;
 SELECT x.lease_until INTO v_lease FROM integration.operations x WHERE x.id=r.id;
 PERFORM integration.complete_operation(p_id,p_generation,p_token,'UNKNOWN',
  CASE WHEN p_report->>'Currency'<>'' AND p_report->>'Currency'<>r.currency
   THEN 'stripe_refund_mismatch' ELSE 'stripe_refund_observed' END,
  coalesce((SELECT x.provider_reference FROM integration.operations x WHERE x.id=r.id),''));
 IF clock_timestamp()>=v_lease THEN
  RAISE EXCEPTION 'Stripe refund lease conflict' USING ERRCODE='40001'; END IF;
END $$;

-- delta: the shape check no longer pins Livemode to false; the attempt is loaded on both paths, so the environment rule
-- runs right after them (same 22023 value error as before, before any write).
CREATE OR REPLACE FUNCTION integration.record_stripe_charge_observation(p_id uuid,p_generation bigint,p_token bytea,
 p_profile text,p_report jsonb,p_reconcile_job bigint) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE r payments.stripe_refunds%ROWTYPE; a checkout.payment_attempts%ROWTYPE; s payments.stripe_sessions%ROWTYPE;
 m integration.merchant_accounts%ROWTYPE; v_kind text; v_via text; v_hash bytea; v_lease timestamptz;
 v_pi text; v_account text; v_sent bigint; v_captured bigint; v_suppressed boolean:=false; v_head bigint;
 v_keys text[]:=ARRAY['Provider','Version','Object','Via','AccountID','KeyVersion','RequestID','RefundRef',
  'PaymentIntentID','ChargeID','Currency','AmountCaptured','AmountRefunded','Refunded','Disputed',
  'Livemode','LocalReason'];
 v_key text; v_value jsonb;
BEGIN
 IF p_id IS NULL OR p_generation IS NULL OR p_generation<2 OR p_token IS NULL OR octet_length(p_token)<>32
  OR p_profile IS NULL OR p_profile NOT IN ('PROVIDER_MOCK','SANDBOX','LIVE') THEN
  RAISE EXCEPTION 'invalid Stripe charge observation' USING ERRCODE='22023'; END IF;
 IF p_report IS NULL OR jsonb_typeof(p_report)<>'object' OR octet_length(p_report::text)>2048
  OR (SELECT count(*) FROM jsonb_object_keys(p_report))<>array_length(v_keys,1)
  OR NOT p_report ?& v_keys THEN
  RAISE EXCEPTION 'invalid Stripe charge observation shape' USING ERRCODE='22023'; END IF;
 FOR v_key,v_value IN SELECT e.key,e.value FROM jsonb_each(p_report) e LOOP
  IF v_key IN ('Version','KeyVersion','AmountCaptured','AmountRefunded') THEN
   IF v_value<>'null'::jsonb AND (jsonb_typeof(v_value)<>'number'
    OR v_value::text !~ '^(0|[1-9][0-9]{0,17})$') THEN
    RAISE EXCEPTION 'invalid Stripe charge observation number' USING ERRCODE='22023'; END IF;
   IF v_value='null'::jsonb AND v_key IN ('Version','KeyVersion') THEN
    RAISE EXCEPTION 'invalid Stripe charge observation number' USING ERRCODE='22023'; END IF;
  ELSIF v_key IN ('Refunded','Disputed','Livemode') THEN
   IF jsonb_typeof(v_value)<>'boolean' THEN
    RAISE EXCEPTION 'invalid Stripe charge observation boolean' USING ERRCODE='22023'; END IF;
  ELSIF jsonb_typeof(v_value)<>'string' THEN
   RAISE EXCEPTION 'invalid Stripe charge observation string' USING ERRCODE='22023';
  END IF;
 END LOOP;
 v_via:=p_report->>'Via';
 IF p_report->>'Provider'<>'stripe' OR p_report->'Version'<>'1'::jsonb OR p_report->>'Object'<>'charge'
  OR v_via NOT IN ('presend','retrieve')
  OR p_report->>'AccountID' !~ '^acct_[A-Za-z0-9]{1,59}$'
  OR p_report->>'RequestID' !~ '^[A-Za-z0-9_]{0,64}$'
  OR p_report->>'RefundRef' !~ '^([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})?$'
  OR p_report->>'PaymentIntentID' !~ '^[A-Za-z0-9_]{1,255}$'
  OR p_report->>'ChargeID' !~ '^[A-Za-z0-9_]{1,255}$'
  OR p_report->>'Currency' !~ '^[A-Z]{3}$'
  OR p_report->>'LocalReason'<>'' THEN
  RAISE EXCEPTION 'invalid Stripe charge observation value' USING ERRCODE='22023'; END IF;
 SELECT x.actor_kind INTO v_kind FROM integration.operations x WHERE x.id=p_id;
 IF v_kind='PAYMENT_REFUND' THEN
  -- Presend: fenced by the refund lease; the refund must be unsent and the report must name it.
  IF v_via<>'presend' THEN
   RAISE EXCEPTION 'Stripe charge observation via mismatch' USING ERRCODE='PT409'; END IF;
  r:=integration.require_stripe_refund(p_id,p_generation,p_token,p_profile);
  SELECT x.* INTO a FROM checkout.payment_attempts x WHERE x.id=r.attempt_id AND x.tenant_id=r.tenant_id
   AND x.store_id=r.store_id;
  v_pi:=r.payment_intent_id; v_account:=r.account_id;
  IF p_report->>'RefundRef'<>r.id::text OR r.first_sent_at IS NOT NULL OR r.suppressed_at IS NOT NULL
   OR p_report->'AmountCaptured'='null'::jsonb OR p_report->'AmountRefunded'='null'::jsonb THEN
   RAISE EXCEPTION 'Stripe presend observation unavailable' USING ERRCODE='PT409'; END IF;
  SELECT x.* INTO m FROM integration.merchant_accounts x WHERE x.id=a.connection_id
   AND x.tenant_id=a.tenant_id AND x.store_id=a.store_id AND x.provider='stripe';
  v_head:=m.credential_version;
  IF m.id IS NULL OR m.account_id<>v_account OR (p_report->'KeyVersion')::bigint<1
   OR (p_report->'KeyVersion')::bigint>v_head THEN
   RAISE EXCEPTION 'Stripe charge observation account mismatch' USING ERRCODE='PT409'; END IF;
 ELSE
  -- Retrieve: fenced by the checkout-operation lease with that attempt's frozen key version (D7).
  IF v_via<>'retrieve' OR p_report->>'RefundRef'<>'' THEN
   RAISE EXCEPTION 'Stripe charge observation via mismatch' USING ERRCODE='PT409'; END IF;
  a:=integration.require_stripe_query(p_id,p_generation,p_token,p_profile);
  SELECT x.* INTO s FROM payments.stripe_sessions x WHERE x.attempt_id=a.id AND x.tenant_id=a.tenant_id
   AND x.store_id=a.store_id;
  v_pi:=s.payment_intent_id; v_account:=s.account_id;
  IF s.attempt_id IS NULL OR (p_report->'KeyVersion')::bigint IS DISTINCT FROM a.credential_version THEN
   RAISE EXCEPTION 'Stripe charge observation account mismatch' USING ERRCODE='PT409'; END IF;
 END IF;
 -- LD1: Livemode must equal the attempt's environment
 IF p_report->'Livemode' IS DISTINCT FROM to_jsonb(a.environment='LIVE') THEN
  RAISE EXCEPTION 'invalid Stripe charge observation value' USING ERRCODE='22023'; END IF;
 SELECT f.amount_minor INTO v_captured FROM payments.facts f WHERE f.tenant_id=a.tenant_id
  AND f.store_id=a.store_id AND f.attempt_id=a.id AND f.kind='CAPTURED';
 IF NOT FOUND OR v_pi IS NULL OR p_report->>'PaymentIntentID'<>v_pi OR p_report->>'AccountID'<>v_account
  OR p_report->>'Currency'<>a.currency THEN
  RAISE EXCEPTION 'Stripe charge observation identity mismatch' USING ERRCODE='PT409'; END IF;
 v_hash:=sha256(convert_to(p_report::text,'UTF8'));
 IF p_reconcile_job IS NULL OR p_reconcile_job<1 OR NOT EXISTS(
  SELECT 1 FROM river_payment.river_job j WHERE j.id=p_reconcile_job
   AND j.kind='payment_reconcile_v1' AND j.state='available' AND j.attempt=0
   AND j.args=jsonb_build_object('operation_id',a.id::text,'report_hash',encode(v_hash,'hex'),'version',1)) THEN
  RAISE EXCEPTION 'Stripe charge reconcile job missing' USING ERRCODE='PT409'; END IF;
 INSERT INTO payments.provider_observations(tenant_id,store_id,attempt_id,source,execution_profile,
  environment,first_generation,report,report_hash)
 VALUES(a.tenant_id,a.store_id,a.id,'QUERY',a.execution_profile,a.environment,p_generation,p_report,v_hash)
 ON CONFLICT(tenant_id,store_id,attempt_id,report_hash) DO NOTHING;
 IF v_via='presend' THEN
  -- RD9: an external refund is detected in the SAME transaction as the send decision. Compare what
  -- Stripe says was refunded with the refunds this system already sent and still holds.
  SELECT coalesce(sum(x.amount_minor),0) INTO v_sent FROM payments.stripe_refunds x
   WHERE x.tenant_id=a.tenant_id AND x.store_id=a.store_id AND x.attempt_id=a.id
    AND x.first_sent_at IS NOT NULL
    AND NOT EXISTS(SELECT 1 FROM payments.refund_facts f WHERE f.tenant_id=x.tenant_id
     AND f.store_id=x.store_id AND f.refund_id=x.id AND f.kind IN ('FAILED','CANCELED','REJECTED'));
  IF (p_report->>'AmountRefunded')::bigint>v_sent THEN
   UPDATE payments.stripe_refunds SET suppressed_at=clock_timestamp() WHERE id=r.id;
   INSERT INTO payments.review_cases(tenant_id,store_id,attempt_id,reason,source_report_hash)
    VALUES(a.tenant_id,a.store_id,a.id,'REFUND_HISTORY',v_hash) ON CONFLICT DO NOTHING;
   v_suppressed:=true;
  END IF;
  PERFORM integration.require_stripe_refund(p_id,p_generation,p_token,p_profile);
  RETURN v_suppressed;
 END IF;
 SELECT x.lease_until INTO v_lease FROM integration.operations x WHERE x.id=a.id;
 PERFORM integration.complete_operation(p_id,p_generation,p_token,'UNKNOWN','stripe_charge_observed',
  coalesce((SELECT x.provider_reference FROM integration.operations x WHERE x.id=a.id),''));
 IF clock_timestamp()>=v_lease THEN
  RAISE EXCEPTION 'Stripe query lease conflict' USING ERRCODE='40001'; END IF;
 RETURN false;
END $$;

-- delta: LD7 - a captured attempt of either environment is refundable; the deployment-environment guard is in Go
-- (merchantorders.RequestRefundIn, S5) because this definer has no profile input. No approval/qualification/method read.
CREATE OR REPLACE FUNCTION payments.request_stripe_refund(p_hash bytea,p_store uuid,p_order uuid,p_key text,
 p_request_hash bytea,p_amount bigint,p_reason text,p_expected_refundable bigint,p_refund uuid,p_job bigint)
RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_access record; v_final record; v_expiry timestamptz; v_tenant uuid; v_principal uuid; v_revision bigint;
 ord checkout.orders%ROWTYPE; a checkout.payment_attempts%ROWTYPE; ao integration.operations%ROWTYPE;
 s payments.stripe_sessions%ROWTYPE; m integration.merchant_accounts%ROWTYPE; v_prev record;
 v_captured bigint; v_held bigint; v_count integer; v_reasons text[]; v_now timestamptz;
 v_request jsonb; v_params jsonb; v_result jsonb; v_full_only boolean;
BEGIN
 IF current_setting('transaction_isolation')<>'read committed'
  OR p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR p_order IS NULL
  OR p_key IS NULL OR p_key !~ '^[A-Za-z0-9_.:-]{8,128}$'
  OR p_request_hash IS NULL OR octet_length(p_request_hash)<>32
  OR p_amount IS NULL OR p_amount<1 OR p_amount>999999999999
  OR p_reason IS NULL OR p_reason NOT IN ('requested_by_customer','duplicate')
  OR p_expected_refundable IS NULL OR p_expected_refundable<0
  OR p_refund IS NULL OR p_job IS NULL OR p_job<1 THEN
  RAISE EXCEPTION 'invalid Stripe refund request' USING ERRCODE='PT400'; END IF;
 -- Authorize before any lock; the caller's GUCs must be exactly the resolved scope.
 SELECT * INTO v_access FROM identity.resolve_access(p_hash,p_store,'payments:refund');
 IF v_access.access_status='unauthorized' THEN RAISE EXCEPTION 'refund access unavailable' USING ERRCODE='PT401'; END IF;
 IF v_access.access_status='not_found' THEN RAISE EXCEPTION 'refund access unavailable' USING ERRCODE='PT404'; END IF;
 IF v_access.access_status<>'ok' THEN RAISE EXCEPTION 'refund access unavailable' USING ERRCODE='PT403'; END IF;
 v_tenant:=v_access.tenant_id; v_principal:=v_access.principal_id; v_revision:=v_access.authz_revision;
 IF v_tenant IS DISTINCT FROM nullif(current_setting('app.tenant_id',true),'')::uuid
  OR p_store IS DISTINCT FROM nullif(current_setting('app.store_id',true),'')::uuid
  OR v_principal IS DISTINCT FROM nullif(current_setting('app.principal_id',true),'')::uuid THEN
  RAISE EXCEPTION 'refund access unavailable' USING ERRCODE='PT403'; END IF;
 -- Serialize duplicate keys with the same lock key command.Run uses, then replay (I02).
 PERFORM pg_advisory_xact_lock(hashtextextended('command|'||v_tenant||'|'||p_store||'|payments.refund.request|'||p_key,0));
 SELECT x.request_hash,x.principal_id,x.response INTO v_prev FROM ops.command_results x
  WHERE x.tenant_id=v_tenant AND x.store_id=p_store AND x.operation='payments.refund.request'
   AND x.idempotency_key=p_key;
 IF FOUND THEN
  IF v_prev.request_hash<>p_request_hash OR v_prev.principal_id<>v_principal THEN
   RAISE EXCEPTION 'refund key conflict' USING ERRCODE='PT409'; END IF;
  v_result:=v_prev.response;
 ELSE
  -- RD3: the order is the fund bucket and the first lock (same as capture and apply).
  SELECT x.* INTO ord FROM checkout.orders x WHERE x.tenant_id=v_tenant AND x.store_id=p_store
   AND x.id=p_order FOR UPDATE;
  IF NOT FOUND THEN RAISE EXCEPTION 'order not found' USING ERRCODE='PT404'; END IF;
  -- The merchant definer adopts the order's buyer scope so the existing owner-scoped 0016/0061
  -- policies apply to the attempt, session and payment-operation reads (A1).
  PERFORM set_config('app.buyer_id',ord.owner_id::text,true);
  PERFORM set_config('app.buyer_session_id',ord.creator_session_id::text,true);
  SELECT x.* INTO a FROM checkout.payment_attempts x WHERE x.tenant_id=v_tenant AND x.store_id=p_store
   AND x.owner_id=ord.owner_id AND x.order_id=ord.id;
  SELECT x.* INTO s FROM payments.stripe_sessions x WHERE x.attempt_id=a.id AND x.tenant_id=a.tenant_id
   AND x.store_id=a.store_id;
  SELECT x.* INTO ao FROM integration.operations x WHERE x.id=a.id AND x.tenant_id=a.tenant_id
   AND x.store_id=a.store_id AND x.actor_kind='BUYER_PAYMENT_QUERY';
  SELECT x.* INTO m FROM integration.merchant_accounts x WHERE x.id=a.connection_id
   AND x.tenant_id=a.tenant_id AND x.store_id=a.store_id AND x.provider='stripe';
  SELECT f.amount_minor INTO v_captured FROM payments.facts f WHERE f.tenant_id=a.tenant_id
   AND f.store_id=a.store_id AND f.attempt_id=a.id AND f.kind='CAPTURED' AND f.currency=a.currency;
  -- §4.3: Stripe, SANDBOX or LIVE, a CAPTURED fact and a pinned PaymentIntent, on the attempt's own account
  IF a.id IS NULL OR a.method_code<>'stripe_checkout' OR a.environment NOT IN ('SANDBOX','LIVE') OR v_captured IS NULL
   OR v_captured<>a.amount_minor OR s.attempt_id IS NULL OR s.payment_intent_id IS NULL
   OR s.account_id IS DISTINCT FROM m.account_id OR ao.id IS NULL OR m.id IS NULL THEN
   RAISE EXCEPTION 'not_refundable' USING ERRCODE='PT422'; END IF;
  SELECT array_agg(c.reason) INTO v_reasons FROM payments.review_cases c WHERE c.tenant_id=a.tenant_id
   AND c.store_id=a.store_id AND c.attempt_id=a.id;
  v_reasons:=coalesce(v_reasons,ARRAY[]::text[]);
  -- (A1) combination rule: only these reasons allow a refund; the late-payment two allow full-remaining only
  IF EXISTS(SELECT 1 FROM unnest(v_reasons) r WHERE r NOT IN
   ('PROVIDER_PRESENTMENT_DRIFT','CLOSURE_CONTRADICTED','PAID_ALLOCATION_FAILED'))
   OR EXISTS(SELECT 1 FROM payments.stripe_refunds x WHERE x.tenant_id=a.tenant_id AND x.store_id=a.store_id
    AND x.attempt_id=a.id AND x.suppressed_at IS NOT NULL AND NOT EXISTS(SELECT 1 FROM payments.refund_facts f
     WHERE f.tenant_id=x.tenant_id AND f.store_id=x.store_id AND f.refund_id=x.id AND f.kind='REJECTED')) THEN
   RAISE EXCEPTION 'refund_blocked_review' USING ERRCODE='PT422'; END IF;
  v_full_only:='CLOSURE_CONTRADICTED'=ANY(v_reasons) OR 'PAID_ALLOCATION_FAILED'=ANY(v_reasons);
  IF NOT payments.stripe_refund_amount_ok(a.currency,p_amount) THEN
   RAISE EXCEPTION 'amount_step' USING ERRCODE='PT422'; END IF;
  -- RD3: held = every refund without a FAILED, CANCELED or REJECTED fact (REQUESTED..UNKNOWN, SUCCEEDED)
  SELECT count(*),coalesce(sum(x.amount_minor) FILTER (WHERE NOT EXISTS(SELECT 1 FROM payments.refund_facts f
    WHERE f.tenant_id=x.tenant_id AND f.store_id=x.store_id AND f.refund_id=x.id
     AND f.kind IN ('FAILED','CANCELED','REJECTED'))),0) INTO v_count,v_held
   FROM payments.stripe_refunds x WHERE x.tenant_id=a.tenant_id AND x.store_id=a.store_id AND x.attempt_id=a.id;
  IF v_count>=20 THEN RAISE EXCEPTION 'refund_limit' USING ERRCODE='PT422'; END IF;
  IF p_expected_refundable<>v_captured-v_held THEN
   RAISE EXCEPTION 'refundable_changed' USING ERRCODE='PT409'; END IF;
  IF v_held+p_amount>v_captured THEN RAISE EXCEPTION 'exceeds_refundable' USING ERRCODE='PT422'; END IF;
  IF v_full_only AND p_amount<>v_captured-v_held THEN
   RAISE EXCEPTION 'refund_blocked_review' USING ERRCODE='PT422'; END IF;
  IF NOT EXISTS(SELECT 1 FROM river_payment.river_job j WHERE j.id=p_job AND j.kind='payment_refund_v1'
   AND j.state='available' AND j.attempt=0 AND j.unique_key IS NULL
   AND j.args=jsonb_build_object('operation_id',p_refund::text,'version',1)) THEN
   RAISE EXCEPTION 'refund job unavailable' USING ERRCODE='PT409'; END IF;
  v_now:=clock_timestamp();
  v_request:=jsonb_build_object('refund_id',p_refund::text,'attempt_id',a.id::text);
  INSERT INTO integration.operations(id,tenant_id,store_id,principal_id,binding_id,binding_version,provider,
   external_asset_id,purpose,action,semantic_key,request_hash,request,job_id,state,generation,
   actor_kind,payment_attempt_id,buyer_owner_id,buyer_session_id)
  VALUES(p_refund,a.tenant_id,a.store_id,v_principal,ao.binding_id,ao.binding_version,'stripe',
   ao.external_asset_id,'transactional','stripe.refund','payment.stripe.refund:'||p_refund,
   sha256(convert_to(v_request::text,'UTF8')),v_request,p_job,'UNKNOWN',1,
   'PAYMENT_REFUND',a.id,a.owner_id,a.session_id);
  v_params:=jsonb_build_object('payment_intent',s.payment_intent_id,'amount',p_amount::text,
   'reason',p_reason,'metadata[lc_refund]',p_refund::text,'metadata[lc_attempt]',a.id::text);
  INSERT INTO payments.stripe_refunds(tenant_id,store_id,id,attempt_id,order_id,owner_id,principal_id,
   environment,account_id,credential_version,payment_intent_id,currency,amount_minor,reason,create_params,
   requested_at,resend_until)
  VALUES(a.tenant_id,a.store_id,p_refund,a.id,ord.id,a.owner_id,v_principal,a.environment,s.account_id,
   m.credential_version,s.payment_intent_id,a.currency,p_amount,p_reason,v_params,
   v_now,v_now+interval '20 hours');
  INSERT INTO integration.operation_events(tenant_id,store_id,operation_id,generation,state,mode,reason_code)
  VALUES(a.tenant_id,a.store_id,p_refund,1,'UNKNOWN','','merchant_refund_requested');
  INSERT INTO ops.audit_events(tenant_id,store_id,principal_id,action)
  VALUES(v_tenant,p_store,v_principal,'payments.refund_requested');
  v_result:=jsonb_build_object('refund_id',p_refund::text,'state','REQUESTED','amount_minor',p_amount,
   'currency',a.currency,'refundable_minor',v_captured-v_held-p_amount);
  INSERT INTO ops.command_results(tenant_id,store_id,operation,idempotency_key,request_hash,response,principal_id)
  VALUES(v_tenant,p_store,'payments.refund.request',p_key,p_request_hash,v_result,v_principal);
 END IF;
 -- Final authority after every lock wait and write.
 SELECT * INTO v_final FROM identity.resolve_access(p_hash,p_store,'payments:refund');
 SELECT x.expires_at INTO v_expiry FROM identity.sessions x
  WHERE x.token_hash=p_hash AND x.audience='merchant' AND x.revoked_at IS NULL;
 IF v_final.access_status='unauthorized' OR v_expiry IS NULL OR v_expiry<=clock_timestamp() THEN
  RAISE EXCEPTION 'refund access unavailable' USING ERRCODE='PT401'; END IF;
 IF v_final.access_status<>'ok' OR v_final.tenant_id IS DISTINCT FROM v_tenant
  OR v_final.principal_id IS DISTINCT FROM v_principal
  OR v_final.authz_revision IS DISTINCT FROM v_revision THEN
  RAISE EXCEPTION 'refund access unavailable' USING ERRCODE='PT403'; END IF;
 RETURN v_result;
END $$;

COMMENT ON FUNCTION checkout.start_stripe_payment(bytea,uuid,text,bytea,uuid,text,bigint,text,uuid,bigint,text,bytea,text) IS 'checkout owner; hosted runtime atomically starts a scoped Stripe attempt (SANDBOX or LIVE profile; a revoked qualification is PT409) and same-tx River query; no PSP send or card data';
COMMENT ON FUNCTION checkout.request_stripe_signal(bytea,uuid,uuid,text,bytea,text,uuid,bigint) IS 'checkout owner; hosted runtime records one buyer refresh or cancel signal (SANDBOX, mock or LIVE profile) with its same-tx River job; no PSP send';
COMMENT ON FUNCTION integration.record_stripe_refund_observation(uuid,bigint,bytea,text,jsonb,bigint) IS 'integration owner; worker records one Stripe refund observation under the refund lease; Livemode must equal the attempt environment; no direct money authority';
COMMENT ON FUNCTION integration.record_stripe_charge_observation(uuid,bigint,bytea,text,jsonb,bigint) IS 'integration owner; worker records a Stripe charge snapshot under the refund lease (presend, may suppress an unsent refund on an external refund) or the checkout lease (retrieve, evidence only) with a same-tx reconcile job; Livemode must equal the attempt environment; no refund send, no direct money authority';
COMMENT ON FUNCTION payments.request_stripe_refund(bytea,uuid,uuid,text,bytea,bigint,text,bigint,uuid,bigint) IS 'payments owner; merchant runtime atomically records one Stripe refund request (SANDBOX or LIVE attempt, independent of approval/method state) with its same-tx River job under payments:refund and the order-lock capacity rule; no provider I/O, no stock, order or fulfilment write';
