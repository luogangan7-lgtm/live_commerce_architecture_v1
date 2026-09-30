-- Stripe refund v1, post-River part (contracts/stripe-refund-v1.md §4.4-§4.6, unit refund-core).
-- Runs after every River schema exists. Owner packages: internal/payments (worker, ingress),
-- internal/merchantorders (request and refresh callers). Non-goals: no stock, order or fulfilment
-- write, no provider I/O, no River state mutation beyond the INSERT the runtime pool needs.
-- Frozen B1 functions replaced here (RF12 records old/new body hashes): payment_job_queue,
-- route_payment_queue_v1, guard_payment_job_family, reject_legacy_family_job, payment_queue_ready,
-- stripe_webhook_commit, guard_stripe_receipt_link, load_stripe_signal (DROP + CREATE) and
-- consume_stripe_signal.

-- §4.5 commerce_runtime: the merchant API inserts payment_refund_v1 and the refresh payment_signal_v1
-- through platform.OpenPool (the commerce_checkout_runtime precedent). No DELETE, no state UPDATE:
-- guard_payment_job_family refuses any kind/args/identity rewrite and internal/platform rejects the rest.
GRANT USAGE ON SCHEMA river_payment TO commerce_runtime;
GRANT SELECT,INSERT,UPDATE(kind) ON river_payment.river_job TO commerce_runtime;
GRANT USAGE ON SEQUENCE river_payment.river_job_id_seq TO commerce_runtime;

-- Queue routing, guard, readiness: 0012 bodies plus the payment_refund_v1 family and the refund-id
-- branch of payment_job_queue.
CREATE OR REPLACE FUNCTION integration.payment_job_queue(p_job bigint)
RETURNS text LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT q.queue FROM (
 SELECT CASE a.execution_profile
  WHEN 'PROVIDER_MOCK' THEN 'payment_mock_v1'
  WHEN 'SANDBOX' THEN 'payment_sandbox_v1'
  WHEN 'LIVE' THEN 'payment_live_v1' END AS queue
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
 -- stripe-refund-v1 §4.4 (A1): a refund id in args.operation_id resolves the attempt through
 -- payments.stripe_refunds; the attempt-id branches above are unchanged.
 UNION ALL
 SELECT CASE a.execution_profile
  WHEN 'PROVIDER_MOCK' THEN 'payment_mock_v1'
  WHEN 'SANDBOX' THEN 'payment_sandbox_v1'
  WHEN 'LIVE' THEN 'payment_live_v1' END
 FROM river_payment.river_job j
 JOIN payments.stripe_refunds r ON r.id::text=j.args->>'operation_id'
 JOIN checkout.payment_attempts a ON a.tenant_id=r.tenant_id AND a.store_id=r.store_id AND a.id=r.attempt_id
 WHERE j.id=p_job AND (
  (j.kind='payment_refund_v1' AND EXISTS(SELECT 1 FROM integration.operations o WHERE o.id=r.id
    AND o.tenant_id=r.tenant_id AND o.store_id=r.store_id AND o.job_id=j.id)
   AND j.args=jsonb_build_object('operation_id',r.id::text,'version',1))
  OR (j.kind='payment_signal_v1' AND EXISTS(
   SELECT 1 FROM payments.stripe_signals s WHERE s.job_id=j.id AND s.refund_id=r.id AND s.attempt_id=a.id
    AND s.tenant_id=a.tenant_id AND s.store_id=a.store_id
    AND j.args=jsonb_build_object('operation_id',r.id::text,'signal_id',s.id::text,'version',1)))))
 q LIMIT 1
$$;
COMMENT ON FUNCTION integration.payment_job_queue(bigint) IS 'integration owner; links River payment query, reconcile, refund or Stripe signal to a frozen attempt profile (refund ids resolve through payments.stripe_refunds); no default-queue escape';

CREATE OR REPLACE FUNCTION integration.route_payment_queue_v1()
RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE j river_payment.river_job%ROWTYPE; expected text;
BEGIN
 SELECT x.* INTO j FROM river_payment.river_job x WHERE x.id=NEW.id FOR UPDATE;
 IF NEW.kind IN ('payment_query_v1','payment_reconcile_v1','payment_signal_v1','payment_refund_v1')
  OR NEW.queue IN ('payment_mock_v1','payment_sandbox_v1','payment_live_v1')
  OR j.kind IN ('payment_query_v1','payment_reconcile_v1','payment_signal_v1','payment_refund_v1')
  OR j.queue IN ('payment_mock_v1','payment_sandbox_v1','payment_live_v1') THEN
  IF j.id IS NULL OR j.kind IS DISTINCT FROM NEW.kind OR j.args IS DISTINCT FROM NEW.args
   OR j.kind NOT IN ('payment_query_v1','payment_reconcile_v1','payment_signal_v1','payment_refund_v1')
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
COMMENT ON FUNCTION integration.route_payment_queue_v1() IS 'integration owner; deferrable River router for linked payment, refund and Stripe signal jobs; no unaudited queue selection';

CREATE OR REPLACE FUNCTION integration.guard_payment_job_family() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 IF NEW.kind NOT IN ('payment_query_v1','payment_reconcile_v1','payment_signal_v1','payment_refund_v1')
  OR NEW.queue NOT IN ('default','payment_mock_v1','payment_sandbox_v1','payment_live_v1')
  OR NEW.unique_key IS NOT NULL OR NEW.args IS NULL OR jsonb_typeof(NEW.args)<>'object'
  OR NEW.args->>'operation_id' IS NULL
  OR NEW.args->>'operation_id' !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  OR NEW.args->>'version' IS DISTINCT FROM '1'
  OR (NEW.kind IN ('payment_query_v1','payment_refund_v1') AND NEW.args IS DISTINCT FROM
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
COMMENT ON FUNCTION integration.guard_payment_job_family() IS 'integration owner; River payment family argument and queue guard, including refund jobs and Stripe signals; no job identity rewrite';

CREATE OR REPLACE FUNCTION integration.reject_legacy_family_job() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 IF NEW.kind IN ('payment_query_v1','payment_reconcile_v1','payment_signal_v1','payment_refund_v1','checkout_expiry_v1')
  OR NEW.queue IN ('payment_mock_v1','payment_sandbox_v1','payment_live_v1','checkout_expiry_v1')
  OR (TG_OP='UPDATE' AND (OLD.kind IN
   ('payment_query_v1','payment_reconcile_v1','payment_signal_v1','payment_refund_v1','checkout_expiry_v1')
   OR OLD.queue IN ('payment_mock_v1','payment_sandbox_v1','payment_live_v1','checkout_expiry_v1'))) THEN
  RAISE EXCEPTION 'legacy family lane disabled' USING ERRCODE='22023'; END IF;
 RETURN NEW;
END $$;
COMMENT ON FUNCTION integration.reject_legacy_family_job() IS 'integration owner; blocks payment, refund, Stripe signal and expiry jobs from legacy River table';

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
   ('payment_query_v1','payment_reconcile_v1','payment_signal_v1','payment_refund_v1')
   OR j.queue NOT IN ('default','payment_mock_v1','payment_sandbox_v1','payment_live_v1')
   OR (j.state NOT IN ('completed','cancelled','discarded') AND
    (j.unique_key IS NOT NULL OR j.args->>'version' IS DISTINCT FROM '1'
     OR j.queue IS DISTINCT FROM integration.payment_job_queue(j.id))));
END $$;
COMMENT ON FUNCTION integration.payment_queue_ready() IS 'integration owner; worker readiness audits River guards, linkage and profile queues for payment, refund and Stripe signal jobs';


-- ------------------------------------------------------------------------------------------------
-- §4.6 webhook commit: a refund receipt links the refund operation id and counts against the refund's
-- own signal_count; a charge receipt counts against charge_signal_count; checkout receipts keep the
-- attempt id and signal_count.
-- ------------------------------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION payments.stripe_webhook_commit(p_receipt uuid,p_signal uuid,p_job bigint)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE r payments.stripe_webhook_receipts%ROWTYPE; s payments.stripe_sessions%ROWTYPE;
 rf payments.stripe_refunds%ROWTYPE; v_operation uuid;
BEGIN
 IF p_receipt IS NULL OR p_signal IS NULL OR p_job IS NULL OR p_job<1 THEN
  RAISE EXCEPTION 'invalid Stripe webhook commit' USING ERRCODE='22023'; END IF;
 SELECT x.* INTO r FROM payments.stripe_webhook_receipts x WHERE x.id=p_receipt FOR SHARE;
 IF NOT FOUND OR r.disposition<>'ACCEPTED' OR r.signal_id<>p_signal
  OR r.attempt_id IS NULL OR r.tenant_id IS NULL OR r.store_id IS NULL THEN
  RAISE EXCEPTION 'Stripe receipt unavailable' USING ERRCODE='PT409'; END IF;
 IF r.refund_id IS NOT NULL THEN
  SELECT x.* INTO rf FROM payments.stripe_refunds x WHERE x.id=r.refund_id AND x.attempt_id=r.attempt_id
   AND x.tenant_id=r.tenant_id AND x.store_id=r.store_id AND x.account_id=r.account_id
   AND x.environment=r.environment FOR UPDATE;
  -- prepare checked the cap; a race with another accepted delivery re-checks here (PT409 only on a race)
  IF NOT FOUND OR rf.signal_count>=64 THEN
   RAISE EXCEPTION 'Stripe signal scope unavailable' USING ERRCODE='PT409'; END IF;
  v_operation:=r.refund_id;
 ELSE
  SELECT x.* INTO s FROM payments.stripe_sessions x WHERE x.attempt_id=r.attempt_id
   AND x.tenant_id=r.tenant_id AND x.store_id=r.store_id FOR UPDATE;
  IF NOT FOUND OR s.account_id<>r.account_id OR s.environment<>r.environment
   OR (r.object_type='charge' AND s.charge_signal_count>=64)
   OR (r.object_type IS DISTINCT FROM 'charge' AND s.signal_count>=64) THEN
   RAISE EXCEPTION 'Stripe signal scope unavailable' USING ERRCODE='PT409'; END IF;
  v_operation:=r.attempt_id;
 END IF;
 IF NOT EXISTS(SELECT 1 FROM river_payment.river_job j WHERE j.id=p_job
  AND j.kind='payment_signal_v1' AND j.state='available' AND j.attempt=0
  AND j.unique_key IS NULL
  AND j.args=jsonb_build_object('operation_id',v_operation::text,
   'signal_id',p_signal::text,'version',1)) THEN
  RAISE EXCEPTION 'Stripe signal job unavailable' USING ERRCODE='PT409'; END IF;
 INSERT INTO payments.stripe_signals(id,tenant_id,store_id,attempt_id,source,receipt_id,session_id,job_id,refund_id)
 VALUES(p_signal,r.tenant_id,r.store_id,r.attempt_id,'STRIPE_WEBHOOK',r.id,r.session_id,p_job,r.refund_id);
 IF r.refund_id IS NOT NULL THEN
  UPDATE payments.stripe_refunds SET signal_count=signal_count+1 WHERE id=r.refund_id;
 ELSIF r.object_type='charge' THEN
  UPDATE payments.stripe_sessions SET charge_signal_count=charge_signal_count+1 WHERE attempt_id=r.attempt_id;
 ELSE
  UPDATE payments.stripe_sessions SET signal_count=signal_count+1 WHERE attempt_id=r.attempt_id;
 END IF;
END $$;
COMMENT ON FUNCTION payments.stripe_webhook_commit(uuid,uuid,bigint) IS 'payments owner; signed ingress links preallocated receipt to exact River signal in same transaction (refund receipts use the refund operation id and their own cap); no financial authority';


CREATE OR REPLACE FUNCTION payments.guard_stripe_receipt_link() RETURNS trigger
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
  AND x.session_id IS NOT DISTINCT FROM r.session_id
  AND x.refund_id IS NOT DISTINCT FROM r.refund_id;
 IF NOT FOUND THEN RAISE EXCEPTION 'Stripe signal linkage missing' USING ERRCODE='23514'; END IF;
 SELECT x.* INTO j FROM river_payment.river_job x WHERE x.id=s.job_id;
 expected:=integration.payment_job_queue(s.job_id);
 IF NOT FOUND OR j.kind<>'payment_signal_v1' OR j.state<>'available'
  OR j.attempt<>0 OR j.unique_key IS NOT NULL
  OR j.args IS DISTINCT FROM jsonb_build_object('operation_id',coalesce(r.refund_id,r.attempt_id)::text,
   'signal_id',r.signal_id::text,'version',1)
  OR expected IS NULL OR j.queue NOT IN ('default',expected) THEN
  RAISE EXCEPTION 'Stripe River linkage missing' USING ERRCODE='23514'; END IF;
 RETURN NULL;
END $$;
COMMENT ON FUNCTION payments.guard_stripe_receipt_link() IS 'payments owner; deferred commit gate rejects accepted receipt without exact signal and River job (refund receipts link the refund operation id); no early webhook ACK';


-- ------------------------------------------------------------------------------------------------
-- §4.6 load/consume: the fence is chosen from the operation's actor_kind. Buyer checkout ops keep
-- require_stripe_query and refund-less signals; PAYMENT_REFUND ops use require_stripe_refund and
-- signals whose refund_id is the operation. The job args shape is unchanged.
-- ------------------------------------------------------------------------------------------------
DROP FUNCTION integration.load_stripe_signal(uuid,bigint,bytea,text,bigint,uuid);
CREATE FUNCTION integration.load_stripe_signal(p_operation uuid,p_generation bigint,
 p_token bytea,p_profile text,p_job bigint,p_signal uuid)
RETURNS TABLE(source text,session_id text,created_at timestamptz,consumed_at timestamptz,db_now timestamptz,
 object_type text,refund_id uuid)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE a checkout.payment_attempts%ROWTYPE; rf payments.stripe_refunds%ROWTYPE; s payments.stripe_signals%ROWTYPE;
 v_kind text; v_object text; v_queue text;
BEGIN
 IF p_job IS NULL OR p_job<1 OR p_signal IS NULL THEN
  RAISE EXCEPTION 'invalid Stripe signal identity' USING ERRCODE='22023'; END IF;
 SELECT x.actor_kind INTO v_kind FROM integration.operations x WHERE x.id=p_operation;
 v_queue:=CASE p_profile WHEN 'PROVIDER_MOCK' THEN 'payment_mock_v1'
  WHEN 'SANDBOX' THEN 'payment_sandbox_v1' WHEN 'LIVE' THEN 'payment_live_v1' END;
 IF v_kind='PAYMENT_REFUND' THEN
  rf:=integration.require_stripe_refund(p_operation,p_generation,p_token,p_profile);
  SELECT x.* INTO s FROM payments.stripe_signals x
   WHERE x.id=p_signal AND x.job_id=p_job AND x.refund_id=p_operation AND x.attempt_id=rf.attempt_id
    AND x.tenant_id=rf.tenant_id AND x.store_id=rf.store_id;
  IF NOT FOUND OR NOT EXISTS(SELECT 1 FROM river_payment.river_job j
   WHERE j.id=p_job AND j.kind='payment_signal_v1' AND j.unique_key IS NULL
    AND j.args=jsonb_build_object('operation_id',p_operation::text,'signal_id',s.id::text,'version',1)
    AND j.queue=v_queue) THEN
   RAISE EXCEPTION 'Stripe signal unavailable' USING ERRCODE='PT409'; END IF;
  PERFORM integration.require_stripe_refund(p_operation,p_generation,p_token,p_profile);
 ELSE
  a:=integration.require_stripe_query(p_operation,p_generation,p_token,p_profile);
  SELECT x.* INTO s FROM payments.stripe_signals x
   WHERE x.id=p_signal AND x.job_id=p_job AND x.attempt_id=a.id AND x.refund_id IS NULL
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
 END IF;
 -- The receipt's object type tells a charge.refunded wake-up from a checkout one; NULL for
 -- buyer and merchant refresh signals, which have no receipt.
 SELECT x.object_type INTO v_object FROM payments.stripe_webhook_receipts x WHERE x.id=s.receipt_id;
 RETURN QUERY SELECT s.source,s.session_id,s.created_at,s.consumed_at,clock_timestamp(),v_object,s.refund_id;
END $$;
ALTER FUNCTION integration.load_stripe_signal(uuid,bigint,bytea,text,bigint,uuid) OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION integration.load_stripe_signal(uuid,bigint,bytea,text,bigint,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION integration.load_stripe_signal(uuid,bigint,bytea,text,bigint,uuid) TO commerce_worker;
COMMENT ON FUNCTION integration.load_stripe_signal(uuid,bigint,bytea,text,bigint,uuid) IS 'integration owner; exact River signal metadata under the matching checkout or refund operation lease and profile, with the receipt object type; no table access, secrets or financial authority';

CREATE OR REPLACE FUNCTION integration.consume_stripe_signal(p_signal uuid,p_operation uuid,p_generation bigint,
 p_token bytea,p_profile text,p_outcome text) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE a checkout.payment_attempts%ROWTYPE; rf payments.stripe_refunds%ROWTYPE; v_lease timestamptz; v_kind text;
BEGIN
 SELECT x.actor_kind INTO v_kind FROM integration.operations x WHERE x.id=p_operation;
 IF v_kind='PAYMENT_REFUND' THEN
  rf:=integration.require_stripe_refund(p_operation,p_generation,p_token,p_profile);
 ELSE
  a:=integration.require_stripe_query(p_operation,p_generation,p_token,p_profile);
 END IF;
 IF p_signal IS NULL OR p_outcome NOT IN
  ('OBSERVED','EXPIRE_REQUESTED','NOOP_TERMINAL','STALE_DROPPED') THEN
  RAISE EXCEPTION 'invalid Stripe signal outcome' USING ERRCODE='22023'; END IF;
 SELECT x.lease_until INTO v_lease FROM integration.operations x WHERE x.id=p_operation;
 IF v_kind='PAYMENT_REFUND' THEN
  UPDATE payments.stripe_signals SET consumed_at=clock_timestamp(),outcome=p_outcome
   WHERE id=p_signal AND tenant_id=rf.tenant_id AND store_id=rf.store_id AND attempt_id=rf.attempt_id
    AND refund_id=p_operation AND consumed_at IS NULL;
 ELSE
  UPDATE payments.stripe_signals SET consumed_at=clock_timestamp(),outcome=p_outcome
   WHERE id=p_signal AND tenant_id=a.tenant_id AND store_id=a.store_id AND attempt_id=a.id
    AND refund_id IS NULL AND consumed_at IS NULL;
 END IF;
 IF NOT FOUND THEN RAISE EXCEPTION 'Stripe signal unavailable' USING ERRCODE='PT409'; END IF;
 IF clock_timestamp()>=v_lease THEN
  RAISE EXCEPTION 'Stripe signal lease conflict' USING ERRCODE='40001'; END IF;
END $$;
COMMENT ON FUNCTION integration.consume_stripe_signal(uuid,uuid,bigint,bytea,text,text) IS 'integration owner; worker marks one durable Stripe signal consumed under the checkout or refund operation lease; no financial authority';

-- ------------------------------------------------------------------------------------------------
-- §4.4 record_stripe_refund_observation: exact §3.1 refund keys, identity or raise, set-once pin,
-- hash dedupe, verified same-tx payment_reconcile_v1 job. Escalate is the only lease-less code.
-- ------------------------------------------------------------------------------------------------
CREATE FUNCTION integration.record_stripe_refund_observation(p_id uuid,p_generation bigint,p_token bytea,
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
   OR p_report->'Livemode'<>'false'::jsonb OR p_report->>'Currency'=''
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
ALTER FUNCTION integration.record_stripe_refund_observation(uuid,bigint,bytea,text,jsonb,bigint)
 OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION integration.record_stripe_refund_observation(uuid,bigint,bytea,text,jsonb,bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION integration.record_stripe_refund_observation(uuid,bigint,bytea,text,jsonb,bigint)
 TO commerce_worker;
COMMENT ON FUNCTION integration.record_stripe_refund_observation(uuid,bigint,bytea,text,jsonb,bigint) IS 'integration owner; worker validates the exact Stripe refund projection, pins the refund once under the operation lease and records an immutable observation with its same-tx reconcile job; binding_changed escalation is the only lease-less code; no direct money authority';

-- ------------------------------------------------------------------------------------------------
-- §4.4 record_stripe_charge_observation: Via='presend' under the refund-op lease (RD9, synchronous
-- suppression) or Via='retrieve' under the checkout-op lease (review evidence only).
-- ------------------------------------------------------------------------------------------------
CREATE FUNCTION integration.record_stripe_charge_observation(p_id uuid,p_generation bigint,p_token bytea,
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
  OR p_report->'Livemode'<>'false'::jsonb OR p_report->>'LocalReason'<>'' THEN
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
ALTER FUNCTION integration.record_stripe_charge_observation(uuid,bigint,bytea,text,jsonb,bigint)
 OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION integration.record_stripe_charge_observation(uuid,bigint,bytea,text,jsonb,bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION integration.record_stripe_charge_observation(uuid,bigint,bytea,text,jsonb,bigint)
 TO commerce_worker;
COMMENT ON FUNCTION integration.record_stripe_charge_observation(uuid,bigint,bytea,text,jsonb,bigint) IS 'integration owner; worker records a Stripe charge snapshot under the refund lease (presend, may suppress an unsent refund on an external refund) or the checkout lease (retrieve, evidence only) with a same-tx reconcile job; no refund send, no direct money authority';

-- ------------------------------------------------------------------------------------------------
-- §4.4 merchant request and refresh (checkout_writer definers, EXECUTE commerce_runtime). Authority is
-- decided in the database (0060 issue_link pattern): resolve_access before any lock, GUC equality with
-- the scope the API pool opened, a fresh resolve_access plus session expiry after the last write.
-- No provider I/O; the caller inserted the River job in the same transaction.
-- ------------------------------------------------------------------------------------------------
CREATE FUNCTION payments.request_stripe_refund(p_hash bytea,p_store uuid,p_order uuid,p_key text,
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
  -- §4.3: Stripe, SANDBOX, a CAPTURED fact and a pinned PaymentIntent, on the attempt's own account
  IF a.id IS NULL OR a.method_code<>'stripe_checkout' OR a.environment<>'SANDBOX' OR v_captured IS NULL
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
ALTER FUNCTION payments.request_stripe_refund(bytea,uuid,uuid,text,bytea,bigint,text,bigint,uuid,bigint)
 OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION payments.request_stripe_refund(bytea,uuid,uuid,text,bytea,bigint,text,bigint,uuid,bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION payments.request_stripe_refund(bytea,uuid,uuid,text,bytea,bigint,text,bigint,uuid,bigint)
 TO commerce_runtime;
COMMENT ON FUNCTION payments.request_stripe_refund(bytea,uuid,uuid,text,bytea,bigint,text,bigint,uuid,bigint) IS 'payments owner; merchant runtime atomically records one Stripe refund request (op, refund, event, audit, command result) with its same-tx River job under payments:refund and the order-lock capacity rule; no provider I/O, no stock, order or fulfilment write';

CREATE FUNCTION payments.request_stripe_refund_refresh(p_hash bytea,p_store uuid,p_order uuid,p_refund uuid,
 p_signal uuid,p_job bigint) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_access record; v_final record; v_expiry timestamptz; v_tenant uuid; v_principal uuid; v_revision bigint;
 ord checkout.orders%ROWTYPE; rf payments.stripe_refunds%ROWTYPE; v_now timestamptz; v_scheduled boolean:=false;
BEGIN
 IF current_setting('transaction_isolation')<>'read committed'
  OR p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR p_order IS NULL
  OR p_refund IS NULL OR p_signal IS NULL OR p_job IS NULL OR p_job<1 THEN
  RAISE EXCEPTION 'invalid Stripe refund refresh' USING ERRCODE='PT400'; END IF;
 SELECT * INTO v_access FROM identity.resolve_access(p_hash,p_store,'payments:refund');
 IF v_access.access_status='unauthorized' THEN RAISE EXCEPTION 'refund access unavailable' USING ERRCODE='PT401'; END IF;
 IF v_access.access_status='not_found' THEN RAISE EXCEPTION 'refund access unavailable' USING ERRCODE='PT404'; END IF;
 IF v_access.access_status<>'ok' THEN RAISE EXCEPTION 'refund access unavailable' USING ERRCODE='PT403'; END IF;
 v_tenant:=v_access.tenant_id; v_principal:=v_access.principal_id; v_revision:=v_access.authz_revision;
 IF v_tenant IS DISTINCT FROM nullif(current_setting('app.tenant_id',true),'')::uuid
  OR p_store IS DISTINCT FROM nullif(current_setting('app.store_id',true),'')::uuid
  OR v_principal IS DISTINCT FROM nullif(current_setting('app.principal_id',true),'')::uuid THEN
  RAISE EXCEPTION 'refund access unavailable' USING ERRCODE='PT403'; END IF;
 SELECT x.* INTO ord FROM checkout.orders x WHERE x.tenant_id=v_tenant AND x.store_id=p_store
  AND x.id=p_order FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'order not found' USING ERRCODE='PT404'; END IF;
 PERFORM set_config('app.buyer_id',ord.owner_id::text,true);
 PERFORM set_config('app.buyer_session_id',ord.creator_session_id::text,true);
 SELECT x.* INTO rf FROM payments.stripe_refunds x WHERE x.id=p_refund AND x.tenant_id=v_tenant
  AND x.store_id=p_store AND x.order_id=ord.id FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'refund not found' USING ERRCODE='PT404'; END IF;
 v_now:=clock_timestamp();
 -- Throttled and bounded in SQL; a late-failure check after SUCCEEDED is allowed, other terminals are not.
 IF NOT EXISTS(SELECT 1 FROM payments.refund_facts f WHERE f.tenant_id=rf.tenant_id AND f.store_id=rf.store_id
   AND f.refund_id=rf.id AND f.kind IN ('FAILED','CANCELED','REJECTED'))
  AND rf.refresh_count<30 AND rf.signal_count<64
  AND (rf.last_refresh_at IS NULL OR v_now>=rf.last_refresh_at+interval '10 seconds') THEN
  IF NOT EXISTS(SELECT 1 FROM river_payment.river_job j WHERE j.id=p_job AND j.kind='payment_signal_v1'
   AND j.state='available' AND j.attempt=0 AND j.unique_key IS NULL
   AND j.args=jsonb_build_object('operation_id',rf.id::text,'signal_id',p_signal::text,'version',1)) THEN
   RAISE EXCEPTION 'refund signal job unavailable' USING ERRCODE='PT409'; END IF;
  INSERT INTO payments.stripe_signals(id,tenant_id,store_id,attempt_id,source,job_id,refund_id)
  VALUES(p_signal,rf.tenant_id,rf.store_id,rf.attempt_id,'MERCHANT_REFRESH',p_job,rf.id);
  UPDATE payments.stripe_refunds SET refresh_count=refresh_count+1,last_refresh_at=v_now,
   signal_count=signal_count+1 WHERE id=rf.id;
  v_scheduled:=true;
 END IF;
 SELECT * INTO v_final FROM identity.resolve_access(p_hash,p_store,'payments:refund');
 SELECT x.expires_at INTO v_expiry FROM identity.sessions x
  WHERE x.token_hash=p_hash AND x.audience='merchant' AND x.revoked_at IS NULL;
 IF v_final.access_status='unauthorized' OR v_expiry IS NULL OR v_expiry<=clock_timestamp() THEN
  RAISE EXCEPTION 'refund access unavailable' USING ERRCODE='PT401'; END IF;
 IF v_final.access_status<>'ok' OR v_final.tenant_id IS DISTINCT FROM v_tenant
  OR v_final.principal_id IS DISTINCT FROM v_principal
  OR v_final.authz_revision IS DISTINCT FROM v_revision THEN
  RAISE EXCEPTION 'refund access unavailable' USING ERRCODE='PT403'; END IF;
 RETURN jsonb_build_object('refund_id',p_refund::text,'scheduled',v_scheduled);
END $$;
ALTER FUNCTION payments.request_stripe_refund_refresh(bytea,uuid,uuid,uuid,uuid,bigint)
 OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION payments.request_stripe_refund_refresh(bytea,uuid,uuid,uuid,uuid,bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION payments.request_stripe_refund_refresh(bytea,uuid,uuid,uuid,uuid,bigint)
 TO commerce_runtime;
COMMENT ON FUNCTION payments.request_stripe_refund_refresh(bytea,uuid,uuid,uuid,uuid,bigint) IS 'payments owner; merchant runtime records one throttled MERCHANT_REFRESH signal for a refund with its same-tx River job under payments:refund; scheduled=false means the caller must roll back its job insert; no provider I/O';
