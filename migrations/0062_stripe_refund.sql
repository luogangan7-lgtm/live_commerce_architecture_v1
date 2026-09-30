-- Stripe refund v1 (contracts/stripe-refund-v1.md §4, integrator rulings 1-14 in
-- docs/delivery/units/refund-fulfilment-rulings.md, unit refund-core).
--
-- Owner package: internal/payments (worker), internal/merchantorders (merchant service),
-- internal/checkout (buyer view). Non-goals: no stock, order, fulfilment or work-item write
-- (RD6), no grant row (0065 owns onboarding), no replacement of identity.read_merchant_orders
-- (ruling 1: 0063 owns the merchant projection), no PAYUNi refund, no LIVE.
-- Roles: checkout_writer owns request/apply definers and the refund tables' policies;
-- integration_writer owns every operation-lease fenced function; commerce_auth owns the merchant
-- refund read. Refund and charge webhooks reuse the frozen B1 receipt/signal/observation tables.
-- Post-River objects (River job families, request/record definers) live in post_river/0013.

-- ------------------------------------------------------------------------------------------------
-- Amount rule twin of internal/integrations/psp/stripe.RefundAmountOK (RF01 parity).
-- ------------------------------------------------------------------------------------------------
CREATE FUNCTION payments.stripe_refund_amount_ok(p_currency text,p_minor bigint)
RETURNS boolean LANGUAGE sql IMMUTABLE SET search_path=pg_catalog AS $$
 SELECT coalesce(CASE p_currency
  WHEN 'HKD' THEN p_minor BETWEEN 1 AND 99999999
  WHEN 'USD' THEN p_minor BETWEEN 1 AND 99999999
  WHEN 'SGD' THEN p_minor BETWEEN 1 AND 99999999
  WHEN 'MYR' THEN p_minor BETWEEN 1 AND 99999999
  WHEN 'TWD' THEN p_minor BETWEEN 100 AND 99999900 AND p_minor%100=0
  ELSE false END,false)
$$;
REVOKE ALL ON FUNCTION payments.stripe_refund_amount_ok(text,bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION payments.stripe_refund_amount_ok(text,bigint)
 TO commerce_checkout_writer,commerce_integration_writer;
COMMENT ON FUNCTION payments.stripe_refund_amount_ok(text,bigint) IS 'payments owner; checkout and integration writers validate the closed refund currency/step table (TWD whole NT$, no provider minimum); no conversion, no provider guarantee';

-- ------------------------------------------------------------------------------------------------
-- §4.1 widened constraints. Every list is re-derived from the live definition so that a later or
-- earlier migration cannot be silently dropped from it.
-- ------------------------------------------------------------------------------------------------
DO $$
DECLARE v_def text;
BEGIN
 SELECT pg_get_constraintdef(c.oid) INTO v_def FROM pg_constraint c
  WHERE c.conrelid='identity.store_grants'::regclass AND c.conname='store_grants_permission_check';
 IF v_def IS NULL OR v_def NOT LIKE '%''orders:read''%' OR v_def LIKE '%payments:refund%'
  OR v_def !~ '::text\]\)' THEN
  RAISE EXCEPTION 'store_grants_permission_check has an unexpected shape: %',v_def; END IF;
 v_def:=regexp_replace(v_def,'::text\]\)','::text, ''payments:refund''::text])');
 ALTER TABLE identity.store_grants DROP CONSTRAINT store_grants_permission_check;
 EXECUTE format('ALTER TABLE identity.store_grants ADD CONSTRAINT store_grants_permission_check %s',v_def);
END $$;

DO $$
DECLARE v_def text;
BEGIN
 SELECT pg_get_constraintdef(c.oid) INTO v_def FROM pg_constraint c
  WHERE c.conrelid='integration.operations'::regclass AND c.conname='operation_actor_family';
 IF v_def IS NULL OR v_def NOT LIKE '%BUYER_PAYMENT_QUERY%' OR v_def NOT LIKE '%stripe.checkout_session%'
  OR v_def NOT LIKE '%MEDIA_ATTEMPT%' OR v_def LIKE '%PAYMENT_REFUND%' OR right(v_def,1)<>')' THEN
  RAISE EXCEPTION 'operation_actor_family has an unexpected shape: %',v_def; END IF;
 v_def:=left(v_def,length(v_def)-1)||
  ' OR (actor_kind=''PAYMENT_REFUND'' AND principal_id IS NOT NULL AND media_attempt_id IS NULL'||
  ' AND payment_attempt_id IS NOT NULL AND payment_attempt_id<>id AND buyer_owner_id IS NOT NULL'||
  ' AND buyer_session_id IS NOT NULL AND provider=''stripe'' AND action=''stripe.refund'''||
  ' AND purpose=''transactional'' AND state NOT IN (''READY'',''DISPATCHING'',''BLOCKED_POLICY'',''STALE_BINDING'')'||
  ' AND lease_mode<>''dispatch''))';
 ALTER TABLE integration.operations DROP CONSTRAINT operation_actor_family;
 EXECUTE format('ALTER TABLE integration.operations ADD CONSTRAINT operation_actor_family %s',v_def);
END $$;

DO $$
DECLARE v_def text;
BEGIN
 SELECT pg_get_constraintdef(c.oid) INTO v_def FROM pg_constraint c
  WHERE c.conrelid='payments.review_cases'::regclass AND c.conname='review_cases_reason_check';
 IF v_def IS NULL OR v_def NOT LIKE '%REFUND_HISTORY%' OR v_def LIKE '%REFUND_UNRESOLVED%'
  OR v_def !~ '::text\]\)' THEN
  RAISE EXCEPTION 'review_cases_reason_check has an unexpected shape: %',v_def; END IF;
 v_def:=regexp_replace(v_def,'::text\]\)',
  '::text, ''REFUND_UNRESOLVED''::text, ''REFUND_AMOUNT_MISMATCH''::text, ''REFUND_CONFLICTING''::text])');
 ALTER TABLE payments.review_cases DROP CONSTRAINT review_cases_reason_check;
 EXECUTE format('ALTER TABLE payments.review_cases ADD CONSTRAINT review_cases_reason_check %s',v_def);
END $$;

-- target of the stripe_refunds order/attempt composite FK (superset of the primary key)
ALTER TABLE checkout.payment_attempts ADD CONSTRAINT payment_attempts_order_attempt_key
 UNIQUE(tenant_id,store_id,owner_id,order_id,id);

-- ------------------------------------------------------------------------------------------------
-- §4.2 tables
-- ------------------------------------------------------------------------------------------------
CREATE TABLE payments.stripe_refunds (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, id uuid PRIMARY KEY,           -- = operations.id
 attempt_id uuid NOT NULL, order_id uuid NOT NULL, owner_id uuid NOT NULL,
 principal_id uuid NOT NULL,                                                     -- requester = approver (RD11)
 environment text NOT NULL CHECK(environment='SANDBOX'),                          -- LIVE refused in v1
 account_id text NOT NULL CHECK(account_id ~ '^acct_[A-Za-z0-9]{1,59}$'),
 credential_version bigint NOT NULL CHECK(credential_version>0),                  -- audit only (RD13)
 payment_intent_id text NOT NULL CHECK(payment_intent_id ~ '^[A-Za-z0-9_]{1,255}$'),
 currency text NOT NULL CHECK(currency ~ '^[A-Z]{3}$'),
 amount_minor bigint NOT NULL CHECK(amount_minor BETWEEN 1 AND 999999999999),
 reason text NOT NULL CHECK(reason IN ('requested_by_customer','duplicate')),
 create_params jsonb NOT NULL CHECK(jsonb_typeof(create_params)='object' AND octet_length(create_params::text)<=2048),
 requested_at timestamptz NOT NULL, resend_until timestamptz NOT NULL,
 first_sent_at timestamptz, last_sent_at timestamptz,
 send_count integer NOT NULL DEFAULT 0 CHECK(send_count BETWEEN 0 AND 200),
 body_sha256 bytea CHECK(body_sha256 IS NULL OR octet_length(body_sha256)=32),
 suppressed_at timestamptz,
 stripe_refund_id text UNIQUE CHECK(stripe_refund_id ~ '^[A-Za-z0-9_]{1,255}$'), pinned_at timestamptz,
 refresh_count integer NOT NULL DEFAULT 0 CHECK(refresh_count BETWEEN 0 AND 30), last_refresh_at timestamptz,
 signal_count integer NOT NULL DEFAULT 0 CHECK(signal_count BETWEEN 0 AND 64),
 CHECK(resend_until=requested_at+interval '20 hours'),
 CHECK((stripe_refund_id IS NULL)=(pinned_at IS NULL)),
 CHECK((first_sent_at IS NULL)=(send_count=0) AND (first_sent_at IS NULL)=(body_sha256 IS NULL)
   AND (first_sent_at IS NULL)=(last_sent_at IS NULL)),
 CHECK(suppressed_at IS NULL OR first_sent_at IS NULL),
 CHECK(first_sent_at IS NULL OR first_sent_at < resend_until - interval '1 hour'),
 CHECK(payments.stripe_refund_amount_ok(currency,amount_minor)),
 UNIQUE(tenant_id,store_id,attempt_id,id),
 FOREIGN KEY(tenant_id,store_id,owner_id,order_id,attempt_id)
  REFERENCES checkout.payment_attempts(tenant_id,store_id,owner_id,order_id,id),
 FOREIGN KEY(tenant_id,store_id,id) REFERENCES integration.operations(tenant_id,store_id,id),
 FOREIGN KEY(tenant_id,principal_id) REFERENCES identity.memberships(tenant_id,principal_id)
);
CREATE INDEX stripe_refunds_by_attempt ON payments.stripe_refunds(tenant_id,store_id,attempt_id,requested_at DESC,id DESC);
CREATE TABLE payments.refund_facts (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, refund_id uuid NOT NULL,
 attempt_id uuid NOT NULL,
 FOREIGN KEY(tenant_id,store_id,attempt_id,refund_id)
  REFERENCES payments.stripe_refunds(tenant_id,store_id,attempt_id,id),
 kind text NOT NULL CHECK(kind IN ('SUCCEEDED','FAILED','CANCELED','REJECTED')),
 amount_minor bigint NOT NULL CHECK(amount_minor>=0), currency text NOT NULL CHECK(currency ~ '^[A-Z]{3}$'),
 stripe_refund_id text CHECK(stripe_refund_id ~ '^[A-Za-z0-9_]{1,255}$'),
 failure_reason text CHECK(failure_reason IN ('lost_or_stolen_card','expired_or_canceled_card',
   'charge_for_pending_refund_disputed','insufficient_funds','declined','merchant_request','unknown',
   'first_send_rejected','external_refund_detected','send_window_closed')),
 source_report_hash bytea NOT NULL CHECK(octet_length(source_report_hash)=32),
 received_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,store_id,refund_id,kind),
 CHECK((kind='REJECTED')=(stripe_refund_id IS NULL)),
 FOREIGN KEY(tenant_id,store_id,attempt_id,source_report_hash)
  REFERENCES payments.provider_observations(tenant_id,store_id,attempt_id,report_hash)
);

-- ------------------------------------------------------------------------------------------------
-- Existing B1 tables: refund columns
-- ------------------------------------------------------------------------------------------------
ALTER TABLE payments.stripe_signals DROP CONSTRAINT stripe_signals_source_check;
ALTER TABLE payments.stripe_signals ADD CONSTRAINT stripe_signals_source_check
 CHECK(source IN ('STRIPE_WEBHOOK','BUYER_REFRESH','BUYER_CANCEL','MERCHANT_REFRESH'));
ALTER TABLE payments.stripe_signals ADD COLUMN refund_id uuid;
ALTER TABLE payments.stripe_signals ADD CONSTRAINT stripe_signals_refund_fk
 FOREIGN KEY(tenant_id,store_id,attempt_id,refund_id)
 REFERENCES payments.stripe_refunds(tenant_id,store_id,attempt_id,id);
ALTER TABLE payments.stripe_signals ADD CONSTRAINT stripe_signals_merchant_refresh_check
 CHECK((source='MERCHANT_REFRESH')<=(refund_id IS NOT NULL));

ALTER TABLE payments.stripe_webhook_receipts DROP CONSTRAINT stripe_webhook_receipts_reason_check;
ALTER TABLE payments.stripe_webhook_receipts ADD CONSTRAINT stripe_webhook_receipts_reason_check
 CHECK(reason IN ('accepted','unsubscribed_type','probe_session','connect_event',
  'livemode_mismatch','account_unregistered','object_mismatch','unknown_session','reference_mismatch',
  'profile_mismatch','signal_cap','malformed_json','unknown_refund','unknown_charge'));
ALTER TABLE payments.stripe_webhook_receipts ADD COLUMN refund_id uuid;
ALTER TABLE payments.stripe_webhook_receipts ADD CONSTRAINT stripe_receipt_refund_fk
 FOREIGN KEY(refund_id) REFERENCES payments.stripe_refunds(id);
ALTER TABLE payments.stripe_webhook_receipts ADD CONSTRAINT stripe_receipt_refund_object_check
 CHECK(refund_id IS NULL OR object_type='refund');

ALTER TABLE payments.stripe_sessions ADD COLUMN charge_signal_count integer NOT NULL DEFAULT 0
 CHECK(charge_signal_count BETWEEN 0 AND 64);
GRANT UPDATE(charge_signal_count) ON payments.stripe_sessions TO commerce_integration_writer;

DO $$ DECLARE v_table text; v_col record; BEGIN
 FOREACH v_table IN ARRAY ARRAY['stripe_refunds','refund_facts'] LOOP
  EXECUTE format('ALTER TABLE payments.%I ENABLE ROW LEVEL SECURITY',v_table);
  EXECUTE format('ALTER TABLE payments.%I FORCE ROW LEVEL SECURITY',v_table);
  EXECUTE format('REVOKE ALL ON payments.%I FROM PUBLIC',v_table);
  EXECUTE format('COMMENT ON TABLE payments.%I IS %L',v_table,
   'payments owner (internal/payments, stripe-refund-v1); refund state for checkout and integration definers; no direct runtime login access, no PSP authority, never stock or order state');
  FOR v_col IN SELECT column_name FROM information_schema.columns
   WHERE table_schema='payments' AND table_name=v_table LOOP
   EXECUTE format('COMMENT ON COLUMN payments.%I.%I IS %L',v_table,v_col.column_name,
    'payments owner; checkout/integration writer bounded refund metadata; auth reads a fixed column subset; no card data, ARN or provider message');
  END LOOP;
 END LOOP;
END $$;
COMMENT ON COLUMN payments.stripe_signals.refund_id IS 'payments owner; set for refund-op signals (job operation_id = refund id) and MERCHANT_REFRESH; NULL keeps the checkout meaning';
COMMENT ON COLUMN payments.stripe_webhook_receipts.refund_id IS 'payments owner; immutable refund mapped by stripe_webhook_prepare for object_type refund; never a client value';
COMMENT ON COLUMN payments.stripe_sessions.charge_signal_count IS 'payments owner; charge.refunded signal cap (64) so refund traffic never crowds the checkout signal_count';
COMMENT ON CONSTRAINT payment_attempts_order_attempt_key ON checkout.payment_attempts IS 'checkout owner; target of payments.stripe_refunds order/attempt composite FK';

-- Triggers: set-once markers, monotone counters and append-only fact rules.
CREATE FUNCTION payments.guard_stripe_refund() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 IF (to_jsonb(OLD)-ARRAY['first_sent_at','last_sent_at','send_count','body_sha256','suppressed_at',
   'stripe_refund_id','pinned_at','refresh_count','last_refresh_at','signal_count'])
  IS DISTINCT FROM
  (to_jsonb(NEW)-ARRAY['first_sent_at','last_sent_at','send_count','body_sha256','suppressed_at',
   'stripe_refund_id','pinned_at','refresh_count','last_refresh_at','signal_count'])
  OR (OLD.first_sent_at IS NOT NULL AND NEW.first_sent_at IS DISTINCT FROM OLD.first_sent_at)
  OR (OLD.body_sha256 IS NOT NULL AND NEW.body_sha256 IS DISTINCT FROM OLD.body_sha256)
  OR (OLD.suppressed_at IS NOT NULL AND NEW.suppressed_at IS DISTINCT FROM OLD.suppressed_at)
  OR (OLD.stripe_refund_id IS NOT NULL AND NEW.stripe_refund_id IS DISTINCT FROM OLD.stripe_refund_id)
  OR (OLD.pinned_at IS NOT NULL AND NEW.pinned_at IS DISTINCT FROM OLD.pinned_at)
  OR (OLD.last_sent_at IS NOT NULL AND (NEW.last_sent_at IS NULL OR NEW.last_sent_at<OLD.last_sent_at))
  OR (OLD.last_refresh_at IS NOT NULL AND (NEW.last_refresh_at IS NULL OR NEW.last_refresh_at<OLD.last_refresh_at))
  OR NEW.send_count<OLD.send_count OR NEW.refresh_count<OLD.refresh_count
  OR NEW.signal_count<OLD.signal_count THEN
  RAISE EXCEPTION 'Stripe refund immutable field changed' USING ERRCODE='42501';
 END IF;
 RETURN NEW;
END $$;
ALTER FUNCTION payments.guard_stripe_refund() OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION payments.guard_stripe_refund() FROM PUBLIC;
CREATE TRIGGER guard_stripe_refund BEFORE UPDATE ON payments.stripe_refunds
 FOR EACH ROW EXECUTE FUNCTION payments.guard_stripe_refund();
COMMENT ON FUNCTION payments.guard_stripe_refund() IS 'payments owner; checkout/integration writers enforce frozen refund columns, set-once markers and monotone counters; no PSP network authority';

CREATE FUNCTION payments.guard_refund_fact() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE rf payments.stripe_refunds%ROWTYPE;
BEGIN
 SELECT x.* INTO rf FROM payments.stripe_refunds x WHERE x.tenant_id=NEW.tenant_id
  AND x.store_id=NEW.store_id AND x.attempt_id=NEW.attempt_id AND x.id=NEW.refund_id;
 IF NOT FOUND OR NEW.amount_minor<>rf.amount_minor OR NEW.currency<>rf.currency THEN
  -- I05: a fact always states exactly the requested money; a mismatch goes to review, never a fact.
  RAISE EXCEPTION 'refund fact money mismatch' USING ERRCODE='23514'; END IF;
 IF NEW.kind='REJECTED' THEN
  IF rf.stripe_refund_id IS NOT NULL OR NEW.stripe_refund_id IS NOT NULL
   OR NEW.failure_reason IS NULL
   OR NEW.failure_reason NOT IN ('first_send_rejected','external_refund_detected','send_window_closed')
   OR EXISTS(SELECT 1 FROM payments.refund_facts f WHERE f.tenant_id=NEW.tenant_id AND f.store_id=NEW.store_id
    AND f.refund_id=NEW.refund_id) THEN
   RAISE EXCEPTION 'refund rejection conflicts' USING ERRCODE='23514'; END IF;
  RETURN NEW;
 END IF;
 IF rf.stripe_refund_id IS NULL OR NEW.stripe_refund_id IS DISTINCT FROM rf.stripe_refund_id
  OR EXISTS(SELECT 1 FROM payments.refund_facts f WHERE f.tenant_id=NEW.tenant_id AND f.store_id=NEW.store_id
   AND f.refund_id=NEW.refund_id AND f.kind='REJECTED') THEN
  RAISE EXCEPTION 'refund fact requires the pinned refund' USING ERRCODE='23514'; END IF;
 IF NEW.kind='SUCCEEDED' THEN
  -- Stripe only moves succeeded to failed (F-R3): SUCCEEDED after FAILED/CANCELED is a conflict.
  IF NEW.failure_reason IS NOT NULL OR EXISTS(SELECT 1 FROM payments.refund_facts f
   WHERE f.tenant_id=NEW.tenant_id AND f.store_id=NEW.store_id AND f.refund_id=NEW.refund_id
    AND f.kind IN ('FAILED','CANCELED')) THEN
   RAISE EXCEPTION 'refund success after failure' USING ERRCODE='23514'; END IF;
 ELSIF NEW.failure_reason IS NULL OR NEW.failure_reason IN ('first_send_rejected','external_refund_detected','send_window_closed') THEN
  RAISE EXCEPTION 'refund failure reason invalid' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
ALTER FUNCTION payments.guard_refund_fact() OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION payments.guard_refund_fact() FROM PUBLIC;
CREATE TRIGGER guard_refund_fact BEFORE INSERT ON payments.refund_facts
 FOR EACH ROW EXECUTE FUNCTION payments.guard_refund_fact();
COMMENT ON FUNCTION payments.guard_refund_fact() IS 'payments owner; checkout writer allows append-only refund facts that state the requested money and the pinned refund; REJECTED excludes every other kind';

-- ------------------------------------------------------------------------------------------------
-- §4.5 grants and policies
-- ------------------------------------------------------------------------------------------------
-- checkout_writer: merchant auth and command receipts (the 0060 claims_writer set, ops rows)
GRANT USAGE ON SCHEMA identity,ops TO commerce_checkout_writer;
GRANT SELECT(token_hash,principal_id,audience,revoked_at,expires_at) ON identity.sessions TO commerce_checkout_writer;
GRANT EXECUTE ON FUNCTION identity.resolve_access(bytea,uuid,text) TO commerce_checkout_writer;
GRANT INSERT ON ops.command_results,ops.audit_events TO commerce_checkout_writer;
GRANT SELECT ON ops.command_results TO commerce_checkout_writer;
CREATE POLICY checkout_command_result_insert ON ops.command_results FOR INSERT TO commerce_checkout_writer
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND principal_id=nullif(current_setting('app.principal_id',true),'')::uuid);
CREATE POLICY checkout_command_result_insert_guard ON ops.command_results AS RESTRICTIVE
 FOR INSERT TO commerce_checkout_writer
 WITH CHECK(principal_id=nullif(current_setting('app.principal_id',true),'')::uuid);
CREATE POLICY checkout_command_result_select ON ops.command_results FOR SELECT TO commerce_checkout_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
CREATE POLICY checkout_audit_insert ON ops.audit_events FOR INSERT TO commerce_checkout_writer
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND principal_id=nullif(current_setting('app.principal_id',true),'')::uuid);
CREATE POLICY checkout_audit_insert_guard ON ops.audit_events AS RESTRICTIVE
 FOR INSERT TO commerce_checkout_writer
 WITH CHECK(principal_id=nullif(current_setting('app.principal_id',true),'')::uuid);

CREATE POLICY checkout_refund_operation ON integration.operations TO commerce_checkout_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND actor_kind='PAYMENT_REFUND'
  AND principal_id=nullif(current_setting('app.principal_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND actor_kind='PAYMENT_REFUND'
  AND principal_id=nullif(current_setting('app.principal_id',true),'')::uuid);
-- The attempt's BUYER_PAYMENT_QUERY operation (binding copy) stays readable through the existing
-- checkout_payment_operation policy because the merchant definers set app.buyer_id from the locked order.
CREATE POLICY checkout_refund_event ON integration.operation_events FOR INSERT TO commerce_checkout_writer
 WITH CHECK(EXISTS(SELECT 1 FROM integration.operations o WHERE o.id=operation_id
  AND o.tenant_id=operation_events.tenant_id AND o.store_id=operation_events.store_id
  AND o.actor_kind='PAYMENT_REFUND'));

GRANT SELECT,INSERT ON payments.stripe_refunds,payments.refund_facts TO commerce_checkout_writer;
GRANT UPDATE(refresh_count,last_refresh_at,signal_count) ON payments.stripe_refunds TO commerce_checkout_writer;
CREATE POLICY stripe_refund_checkout ON payments.stripe_refunds TO commerce_checkout_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
CREATE POLICY refund_fact_checkout ON payments.refund_facts TO commerce_checkout_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);

-- integration_writer: RD9 pre-send comparison, review read/insert, webhook cap accounting
GRANT SELECT ON payments.stripe_refunds,payments.refund_facts,payments.review_cases TO commerce_integration_writer;
GRANT UPDATE(first_sent_at,last_sent_at,send_count,body_sha256,suppressed_at,stripe_refund_id,
 pinned_at,signal_count) ON payments.stripe_refunds TO commerce_integration_writer;
GRANT INSERT ON payments.review_cases TO commerce_integration_writer;
CREATE POLICY stripe_refund_integration ON payments.stripe_refunds TO commerce_integration_writer
 USING(true) WITH CHECK(true);
CREATE POLICY refund_fact_integration ON payments.refund_facts FOR SELECT TO commerce_integration_writer
 USING(true);
CREATE POLICY review_integration_read ON payments.review_cases FOR SELECT TO commerce_integration_writer
 USING(true);
CREATE POLICY review_integration_refund_history ON payments.review_cases FOR INSERT TO commerce_integration_writer
 WITH CHECK(reason='REFUND_HISTORY');

-- commerce_auth: merchant refund read (0027 merchant_order_projection pattern)
GRANT SELECT(tenant_id,store_id,attempt_id,id,amount_minor,currency,reason,requested_at,resend_until,
 first_sent_at,last_sent_at,stripe_refund_id,pinned_at,suppressed_at) ON payments.stripe_refunds TO commerce_auth;
GRANT SELECT(tenant_id,store_id,refund_id,kind,received_at,failure_reason) ON payments.refund_facts TO commerce_auth;
CREATE POLICY auth_refund_read ON payments.stripe_refunds FOR SELECT TO commerce_auth USING(true);
CREATE POLICY auth_refund_fact_read ON payments.refund_facts FOR SELECT TO commerce_auth USING(true);
-- Not in the §4.5 list (flagged for integrator ruling): the merchant refund read must tell a Stripe attempt
-- from a PAYUNi one. checkout.payment_attempts keeps its exact merchant_orders column set (its gate asserts
-- it), so the read uses the presence of a payments.stripe_sessions row: three key columns, no session data.
GRANT SELECT(tenant_id,store_id,attempt_id) ON payments.stripe_sessions TO commerce_auth;
CREATE POLICY auth_refund_session_read ON payments.stripe_sessions FOR SELECT TO commerce_auth USING(true);

-- ------------------------------------------------------------------------------------------------
-- §4.4 refund-operation fence and worker definers (integration_writer; EXECUTE worker unless noted)
-- ------------------------------------------------------------------------------------------------
CREATE FUNCTION integration.require_stripe_refund(p_id uuid,p_generation bigint,p_token bytea,p_profile text)
RETURNS payments.stripe_refunds LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE o integration.operations%ROWTYPE; b integration.bindings%ROWTYPE;
 a checkout.payment_attempts%ROWTYPE; r payments.stripe_refunds%ROWTYPE; v_binding uuid;
BEGIN
 IF p_id IS NULL OR p_generation IS NULL OR p_generation<2 OR p_token IS NULL OR octet_length(p_token)<>32
  OR p_profile IS NULL OR p_profile NOT IN ('PROVIDER_MOCK','SANDBOX','LIVE') THEN
  RAISE EXCEPTION 'invalid Stripe refund' USING ERRCODE='22023'; END IF;
 SELECT x.binding_id INTO v_binding FROM integration.operations x
  WHERE x.id=p_id AND x.actor_kind='PAYMENT_REFUND' AND x.provider='stripe';
 IF NOT FOUND THEN RAISE EXCEPTION 'Stripe refund unavailable' USING ERRCODE='P0002'; END IF;
 -- Lock order = Claim/Complete: binding FOR SHARE, then the operation FOR UPDATE.
 SELECT x.* INTO b FROM integration.bindings x WHERE x.id=v_binding FOR SHARE;
 SELECT x.* INTO o FROM integration.operations x WHERE x.id=p_id FOR UPDATE;
 SELECT x.* INTO r FROM payments.stripe_refunds x WHERE x.id=p_id AND x.tenant_id=o.tenant_id
  AND x.store_id=o.store_id;
 SELECT x.* INTO a FROM checkout.payment_attempts x WHERE x.id=r.attempt_id AND x.tenant_id=r.tenant_id
  AND x.store_id=r.store_id AND x.owner_id=r.owner_id AND x.order_id=r.order_id;
 IF r.id IS NULL OR a.id IS NULL OR o.actor_kind<>'PAYMENT_REFUND' OR o.action<>'stripe.refund'
  OR o.provider<>'stripe' OR o.purpose<>'transactional' OR o.payment_attempt_id<>a.id
  OR o.buyer_owner_id<>a.owner_id OR o.buyer_session_id<>a.session_id
  OR o.binding_id<>a.binding_id OR o.binding_version<>a.binding_version
  OR a.method_code<>'stripe_checkout' OR b.id IS NULL OR b.tenant_id<>a.tenant_id
  OR b.store_id<>a.store_id OR b.provider<>o.provider OR b.external_asset_id<>o.external_asset_id
  OR a.execution_profile<>p_profile OR r.environment<>a.environment
  OR (p_profile='PROVIDER_MOCK' AND a.environment<>'SANDBOX')
  OR (p_profile<>'PROVIDER_MOCK' AND a.environment<>p_profile) THEN
  RAISE EXCEPTION 'Stripe refund unavailable' USING ERRCODE='PT409'; END IF;
 IF o.state<>'UNKNOWN' OR o.lease_mode<>'reconcile' OR o.generation<>p_generation
  OR o.lease_until IS NULL OR o.lease_until<=clock_timestamp()
  OR o.lease_token_hash IS DISTINCT FROM sha256(p_token) THEN
  RAISE EXCEPTION 'Stripe refund lease conflict' USING ERRCODE='40001'; END IF;
 RETURN r;
END $$;
ALTER FUNCTION integration.require_stripe_refund(uuid,bigint,bytea,text) OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION integration.require_stripe_refund(uuid,bigint,bytea,text) FROM PUBLIC;
COMMENT ON FUNCTION integration.require_stripe_refund(uuid,bigint,bytea,text) IS 'integration owner; private refund-operation lease and profile fence, the refund analogue of require_stripe_query; never reveals credentials, no EXECUTE for any login';

CREATE FUNCTION integration.load_stripe_refund(p_id uuid,p_generation bigint,p_token bytea,p_profile text)
RETURNS TABLE(tenant_id uuid,store_id uuid,attempt_id uuid,order_id uuid,connection_id uuid,environment text,
 account_id text,credential_version bigint,key_id text,nonce bytea,ciphertext bytea,
 payment_intent_id text,currency text,amount_minor bigint,reason text,requested_at timestamptz,
 resend_until timestamptz,first_sent_at timestamptz,last_sent_at timestamptz,send_count integer,
 body_sha256 bytea,suppressed_at timestamptz,stripe_refund_id text,pinned_at timestamptz,
 has_succeeded boolean,has_terminal boolean,has_rejected boolean,latest_status text,db_now timestamptz)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE r payments.stripe_refunds%ROWTYPE; a checkout.payment_attempts%ROWTYPE;
 m integration.merchant_accounts%ROWTYPE; c integration.account_credentials%ROWTYPE;
 o integration.operations%ROWTYPE; v_ok boolean; v_term boolean; v_rej boolean; v_status text;
BEGIN
 r:=integration.require_stripe_refund(p_id,p_generation,p_token,p_profile);
 SELECT x.* INTO a FROM checkout.payment_attempts x WHERE x.id=r.attempt_id AND x.tenant_id=r.tenant_id
  AND x.store_id=r.store_id;
 -- RD13: the account's CURRENT head credential, and only for the frozen account.
 SELECT x.* INTO m FROM integration.merchant_accounts x WHERE x.id=a.connection_id
  AND x.tenant_id=a.tenant_id AND x.store_id=a.store_id AND x.binding_id=a.binding_id
  AND x.provider='stripe' AND x.environment=a.environment;
 IF NOT FOUND OR m.account_id<>r.account_id THEN
  RAISE EXCEPTION 'Stripe refund account unavailable' USING ERRCODE='PT409'; END IF;
 SELECT x.* INTO c FROM integration.account_credentials x WHERE x.tenant_id=m.tenant_id
  AND x.store_id=m.store_id AND x.connection_id=m.id AND x.version=m.credential_version;
 IF NOT FOUND THEN RAISE EXCEPTION 'Stripe refund credential unavailable' USING ERRCODE='PT409'; END IF;
 SELECT x.* INTO o FROM integration.operations x WHERE x.id=r.id;
 IF o.external_asset_id<>m.environment||':'||m.account_id THEN
  RAISE EXCEPTION 'Stripe refund account mismatch' USING ERRCODE='PT409'; END IF;
 SELECT EXISTS(SELECT 1 FROM payments.refund_facts f WHERE f.tenant_id=r.tenant_id AND f.store_id=r.store_id
   AND f.refund_id=r.id AND f.kind='SUCCEEDED'),
  EXISTS(SELECT 1 FROM payments.refund_facts f WHERE f.tenant_id=r.tenant_id AND f.store_id=r.store_id
   AND f.refund_id=r.id AND f.kind IN ('FAILED','CANCELED')),
  EXISTS(SELECT 1 FROM payments.refund_facts f WHERE f.tenant_id=r.tenant_id AND f.store_id=r.store_id
   AND f.refund_id=r.id AND f.kind='REJECTED') INTO v_ok,v_term,v_rej;
 SELECT x.report->>'Status' INTO v_status FROM payments.provider_observations x
  WHERE x.tenant_id=r.tenant_id AND x.store_id=r.store_id AND x.attempt_id=r.attempt_id
   AND x.source='QUERY' AND x.report->>'Object'='refund' AND x.report->>'RefundRef'=r.id::text
   AND x.report->>'RefundID'<>'' ORDER BY x.received_at DESC LIMIT 1;
 -- Recheck after waits so an expired claim cannot release secret material.
 PERFORM integration.require_stripe_refund(p_id,p_generation,p_token,p_profile);
 RETURN QUERY SELECT r.tenant_id,r.store_id,r.attempt_id,r.order_id,a.connection_id,a.environment,
  m.account_id,m.credential_version,c.key_id,c.nonce,c.ciphertext,r.payment_intent_id,r.currency,
  r.amount_minor,r.reason,r.requested_at,r.resend_until,r.first_sent_at,r.last_sent_at,r.send_count,
  r.body_sha256,r.suppressed_at,r.stripe_refund_id,r.pinned_at,v_ok,v_term,v_rej,coalesce(v_status,''),
  clock_timestamp();
END $$;
ALTER FUNCTION integration.load_stripe_refund(uuid,bigint,bytea,text) OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION integration.load_stripe_refund(uuid,bigint,bytea,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION integration.load_stripe_refund(uuid,bigint,bytea,text) TO commerce_worker;
COMMENT ON FUNCTION integration.load_stripe_refund(uuid,bigint,bytea,text) IS 'integration owner; worker reads the frozen refund and the account current-head API credential under the refund-operation lease (RD13); never another account key';

CREATE FUNCTION integration.mark_stripe_refund_sent(p_id uuid,p_generation bigint,p_token bytea,
 p_profile text,p_body_sha256 bytea) RETURNS text
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE r payments.stripe_refunds%ROWTYPE; v_now timestamptz;
BEGIN
 r:=integration.require_stripe_refund(p_id,p_generation,p_token,p_profile);
 IF p_body_sha256 IS NULL OR octet_length(p_body_sha256)<>32 THEN
  RAISE EXCEPTION 'invalid Stripe refund body hash' USING ERRCODE='22023'; END IF;
 SELECT x.* INTO r FROM payments.stripe_refunds x WHERE x.id=p_id FOR UPDATE;
 v_now:=clock_timestamp();
 IF r.stripe_refund_id IS NOT NULL OR r.suppressed_at IS NOT NULL OR EXISTS(
  SELECT 1 FROM payments.refund_facts f WHERE f.tenant_id=r.tenant_id AND f.store_id=r.store_id
   AND f.refund_id=r.id AND f.kind IN ('REJECTED','FAILED','CANCELED'))
  -- RD4: a recorded first-send rejection is final even before payment_reconcile_v1 applies its
  -- REJECTED fact. A resend would reuse a key Stripe may not have cached (400 / pre-idempotency 401)
  -- and could create the refund after capacity is released.
  OR EXISTS(SELECT 1 FROM payments.provider_observations o WHERE o.tenant_id=r.tenant_id
   AND o.store_id=r.store_id AND o.attempt_id=r.attempt_id AND o.source='QUERY'
   AND o.report->>'Object'='refund' AND o.report->>'RefundRef'=r.id::text AND o.report->>'Via'='create'
   AND o.report->>'ErrorClass'='rejected' AND o.report->>'SendCount'='1' AND o.report->>'RefundID'='') THEN
  RETURN 'CLOSED'; END IF;
 IF r.first_sent_at IS NULL THEN
  -- §11.5 / RD9: the first send needs a fresh same-generation charge read that did not suppress,
  -- and no sticky review; the window leaves one hour for the list closure (A1 CHECK).
  IF v_now>=r.resend_until-interval '1 hour' OR NOT EXISTS(
   SELECT 1 FROM payments.provider_observations o WHERE o.tenant_id=r.tenant_id AND o.store_id=r.store_id
    AND o.attempt_id=r.attempt_id AND o.source='QUERY' AND o.first_generation=p_generation
    AND o.report->>'Object'='charge' AND o.report->>'Via'='presend'
    AND o.report->>'RefundRef'=r.id::text AND o.received_at>=v_now-interval '60 seconds')
   OR EXISTS(SELECT 1 FROM payments.review_cases c WHERE c.tenant_id=r.tenant_id AND c.store_id=r.store_id
    AND c.attempt_id=r.attempt_id AND c.reason IN ('REFUND_HISTORY','CONFLICTING_REPORT')) THEN
   RETURN 'CLOSED'; END IF;
  UPDATE payments.stripe_refunds SET first_sent_at=v_now,last_sent_at=v_now,send_count=1,
   body_sha256=p_body_sha256 WHERE id=r.id;
  PERFORM integration.require_stripe_refund(p_id,p_generation,p_token,p_profile);
  RETURN 'SEND';
 END IF;
 IF v_now>=r.resend_until OR r.body_sha256<>p_body_sha256 OR r.send_count>=200 THEN RETURN 'CLOSED'; END IF;
 UPDATE payments.stripe_refunds SET last_sent_at=v_now,send_count=send_count+1 WHERE id=r.id;
 PERFORM integration.require_stripe_refund(p_id,p_generation,p_token,p_profile);
 RETURN 'RESEND';
END $$;
ALTER FUNCTION integration.mark_stripe_refund_sent(uuid,bigint,bytea,text,bytea) OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION integration.mark_stripe_refund_sent(uuid,bigint,bytea,text,bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION integration.mark_stripe_refund_sent(uuid,bigint,bytea,text,bytea) TO commerce_worker;
COMMENT ON FUNCTION integration.mark_stripe_refund_sent(uuid,bigint,bytea,text,bytea) IS 'integration owner; worker commits SEND/RESEND for the exact create body before any POST; SEND needs a fresh presend charge read and no sticky review; same key only until resend_until; CLOSED once a first-send rejection is recorded (never resend after it)';

CREATE FUNCTION integration.finish_stripe_refund(p_id uuid,p_generation bigint,p_token bytea,
 p_profile text,p_code text) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE r payments.stripe_refunds%ROWTYPE; v_lease timestamptz; v_reference text;
BEGIN
 r:=integration.require_stripe_refund(p_id,p_generation,p_token,p_profile);
 IF p_code NOT IN ('stripe_refund_uncertain','stripe_retrieve_failed','stripe_rate_limited',
  'stripe_timeout','stripe_panic','stripe_record_failed','stripe_refund_mismatch',
  'stripe_idempotency_alarm','stripe_unavailable','stripe_budget_exhausted','stripe_refund_terminal') THEN
  RAISE EXCEPTION 'invalid Stripe refund completion' USING ERRCODE='22023'; END IF;
 IF p_code='stripe_refund_terminal' AND NOT EXISTS(SELECT 1 FROM payments.refund_facts f
  WHERE f.tenant_id=r.tenant_id AND f.store_id=r.store_id AND f.refund_id=r.id) THEN
  RAISE EXCEPTION 'Stripe refund terminal fact missing' USING ERRCODE='PT409'; END IF;
 SELECT x.lease_until,x.provider_reference INTO v_lease,v_reference
  FROM integration.operations x WHERE x.id=r.id;
 PERFORM integration.complete_operation(p_id,p_generation,p_token,'UNKNOWN',p_code,v_reference);
 IF clock_timestamp()>=v_lease THEN
  RAISE EXCEPTION 'Stripe refund lease conflict' USING ERRCODE='40001'; END IF;
END $$;
ALTER FUNCTION integration.finish_stripe_refund(uuid,bigint,bytea,text,text) OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION integration.finish_stripe_refund(uuid,bigint,bytea,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION integration.finish_stripe_refund(uuid,bigint,bytea,text,text) TO commerce_worker;
COMMENT ON FUNCTION integration.finish_stripe_refund(uuid,bigint,bytea,text,text) IS 'integration owner; worker completes UNKNOWN with a fixed reason under the refund lease; never SUCCEEDED, never a blind retry';

-- ------------------------------------------------------------------------------------------------
-- §4.6 payments.stripe_webhook_prepare: DROP + CREATE (return type adds refund_id and object_type).
-- The two new parameters default to NULL so an older 16-argument caller resolves unchanged; the
-- checkout branch below is the 0061 body byte for byte. Frozen B1 body replaced, see RF12.
-- ------------------------------------------------------------------------------------------------
DROP FUNCTION payments.stripe_webhook_prepare(uuid,bigint,text,text,bigint,text,text,text,text,text,boolean,boolean,boolean,boolean,bytea,bigint);
CREATE FUNCTION payments.stripe_webhook_prepare(p_endpoint uuid,p_key_version bigint,
 p_event_id text,p_event_type text,p_event_created bigint,p_api_version text,p_object_type text,
 p_session_id text,p_client_reference text,p_metadata_attempt text,p_account_present boolean,
 p_livemode boolean,p_probe boolean,p_malformed boolean,p_body_sha256 bytea,p_signed_at bigint,
 p_payment_intent text DEFAULT NULL,p_metadata_refund text DEFAULT NULL)
RETURNS TABLE(disposition text,receipt_id uuid,attempt_id uuid,session_id text,signal_id uuid,refund_id uuid,object_type text)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE ep payments.stripe_webhook_endpoints%ROWTYPE; a checkout.payment_attempts%ROWTYPE;
 sess payments.stripe_sessions%ROWTYPE; existing payments.stripe_webhook_receipts%ROWTYPE;
 v_receipt uuid; v_signal uuid; v_disposition text; v_reason text; v_attempt uuid;
 v_tenant uuid; v_store uuid; v_session text; rf payments.stripe_refunds%ROWTYPE; v_refund uuid;
BEGIN
 IF p_endpoint IS NULL OR p_key_version IS NULL OR p_key_version<1
  OR p_body_sha256 IS NULL OR octet_length(p_body_sha256)<>32
  OR p_signed_at IS NULL OR p_signed_at<=0 OR p_account_present IS NULL
  OR p_livemode IS NULL OR p_probe IS NULL OR p_malformed IS NULL THEN
  RAISE EXCEPTION 'invalid Stripe webhook metadata' USING ERRCODE='22023'; END IF;
 SELECT e.* INTO ep FROM payments.stripe_webhook_endpoints e WHERE e.endpoint_id=p_endpoint FOR SHARE;
 IF NOT FOUND OR NOT ep.enabled OR ep.key_version<>p_key_version THEN
  RAISE EXCEPTION 'Stripe webhook endpoint unavailable' USING ERRCODE='40001'; END IF;
 IF p_malformed THEN
  PERFORM pg_advisory_xact_lock(hashtextextended('stripe.webhook.malformed|'
   ||ep.endpoint_id||'|'||encode(p_body_sha256,'hex'),0));
  SELECT x.* INTO existing FROM payments.stripe_webhook_receipts x
   WHERE x.endpoint_id=ep.endpoint_id
    AND x.event_id IS NULL AND x.body_sha256=p_body_sha256 FOR UPDATE;
  IF FOUND THEN
   UPDATE payments.stripe_webhook_receipts SET redelivery_count=redelivery_count+1,
    last_redelivered_at=clock_timestamp() WHERE id=existing.id;
   RETURN QUERY SELECT 'DUPLICATE'::text,existing.id,existing.attempt_id,existing.session_id,existing.signal_id,
    existing.refund_id,existing.object_type;
   RETURN;
  END IF;
  v_receipt:=gen_random_uuid();
  INSERT INTO payments.stripe_webhook_receipts(id,endpoint_id,environment,account_id,event_id,body_sha256,
   signed_at,disposition,reason)
   VALUES(v_receipt,ep.endpoint_id,ep.environment,ep.account_id,NULL,p_body_sha256,p_signed_at,'MALFORMED','malformed_json');
  RETURN QUERY SELECT 'MALFORMED'::text,v_receipt,NULL::uuid,NULL::text,NULL::uuid,NULL::uuid,NULL::text;
  RETURN;
 END IF;
 IF p_event_id IS NULL OR p_event_id !~ '^[A-Za-z0-9_]{1,255}$'
  OR p_event_type IS NULL OR p_event_type !~ '^[a-z0-9_.]{1,100}$'
  OR p_session_id IS NULL OR (p_session_id<>'' AND p_session_id !~ '^[A-Za-z0-9_]{1,255}$')
  OR p_event_created IS NULL OR p_event_created<=0 THEN
  RAISE EXCEPTION 'invalid Stripe webhook projection' USING ERRCODE='22023'; END IF;
 IF (p_payment_intent IS NOT NULL AND p_payment_intent<>'' AND p_payment_intent !~ '^[A-Za-z0-9_]{1,255}$')
  OR (p_metadata_refund IS NOT NULL AND octet_length(p_metadata_refund)>64) THEN
  RAISE EXCEPTION 'invalid Stripe webhook refund projection' USING ERRCODE='22023'; END IF;
 PERFORM pg_advisory_xact_lock(hashtextextended('stripe.webhook.event|'
  ||ep.endpoint_id||'|'||p_event_id,0));
 SELECT x.* INTO existing FROM payments.stripe_webhook_receipts x
  WHERE x.endpoint_id=ep.endpoint_id AND x.event_id=p_event_id FOR UPDATE;
 IF FOUND THEN
  UPDATE payments.stripe_webhook_receipts SET redelivery_count=redelivery_count+1,
   last_redelivered_at=clock_timestamp() WHERE id=existing.id;
  RETURN QUERY SELECT 'DUPLICATE'::text,existing.id,existing.attempt_id,existing.session_id,existing.signal_id,
    existing.refund_id,existing.object_type;
  RETURN;
 END IF;
 v_disposition:='QUARANTINED'; v_reason:='unknown_session';
 IF p_account_present THEN v_reason:='connect_event';
 ELSIF p_livemode<>(ep.environment='LIVE') THEN v_reason:='livemode_mismatch';
 ELSIF p_event_type NOT IN ('checkout.session.completed','checkout.session.async_payment_succeeded',
  'checkout.session.async_payment_failed','checkout.session.expired',
  'refund.created','refund.updated','refund.failed','charge.refunded') THEN
  v_disposition:='IGNORED'; v_reason:='unsubscribed_type';
 ELSIF p_probe THEN v_disposition:='IGNORED'; v_reason:='probe_session';
 ELSIF (p_event_type LIKE 'checkout.session.%' AND p_object_type<>'checkout.session')
  OR (p_event_type LIKE 'refund.%' AND p_object_type<>'refund')
  OR (p_event_type='charge.refunded' AND p_object_type<>'charge') THEN v_reason:='object_mismatch';
 ELSIF p_event_type LIKE 'refund.%' THEN
  -- stripe-refund-v1 §7.3: a refund object maps by its pinned id, else by metadata.lc_refund of an
  -- unpinned refund whose lc_attempt also matches, always scoped to this endpoint's account.
  v_disposition:='IGNORED'; v_reason:='unknown_refund';
  SELECT x.* INTO rf FROM payments.stripe_refunds x WHERE x.stripe_refund_id=p_session_id
   AND x.account_id=ep.account_id AND x.environment=ep.environment;
  IF rf.id IS NULL
   AND p_metadata_refund ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
   AND p_metadata_attempt ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' THEN
   SELECT x.* INTO rf FROM payments.stripe_refunds x WHERE x.id=p_metadata_refund::uuid
    AND x.attempt_id=p_metadata_attempt::uuid AND x.stripe_refund_id IS NULL
    AND x.account_id=ep.account_id AND x.environment=ep.environment;
  END IF;
  IF rf.id IS NOT NULL THEN
   SELECT x.* INTO a FROM checkout.payment_attempts x WHERE x.id=rf.attempt_id
    AND x.tenant_id=rf.tenant_id AND x.store_id=rf.store_id;
   IF a.id IS NULL OR a.execution_profile<>ep.execution_profile
    OR a.connection_id<>ep.connection_id OR a.method_code<>'stripe_checkout' THEN
    v_disposition:='QUARANTINED'; v_reason:='profile_mismatch';
   ELSE
    SELECT x.* INTO rf FROM payments.stripe_refunds x WHERE x.id=rf.id FOR UPDATE;
    IF rf.signal_count>=64 THEN v_disposition:='IGNORED'; v_reason:='signal_cap';
    ELSE
     v_disposition:='ACCEPTED'; v_reason:='accepted'; v_attempt:=rf.attempt_id; v_refund:=rf.id;
     v_tenant:=rf.tenant_id; v_store:=rf.store_id; v_session:=p_session_id;
     v_signal:=gen_random_uuid();
    END IF;
   END IF;
  END IF;
 ELSIF p_event_type='charge.refunded' THEN
  -- A charge object maps by payment_intent to a pinned, captured Stripe attempt of this account.
  v_disposition:='IGNORED'; v_reason:='unknown_charge';
  IF p_payment_intent IS NOT NULL AND p_payment_intent<>'' THEN
   SELECT x.* INTO sess FROM payments.stripe_sessions x WHERE x.payment_intent_id=p_payment_intent
    AND x.account_id=ep.account_id AND x.environment=ep.environment
    AND EXISTS(SELECT 1 FROM payments.facts f WHERE f.tenant_id=x.tenant_id AND f.store_id=x.store_id
     AND f.attempt_id=x.attempt_id AND f.kind='CAPTURED');
  END IF;
  IF sess.attempt_id IS NOT NULL THEN
   SELECT x.* INTO a FROM checkout.payment_attempts x WHERE x.id=sess.attempt_id
    AND x.tenant_id=sess.tenant_id AND x.store_id=sess.store_id;
   IF a.id IS NULL OR a.execution_profile<>ep.execution_profile
    OR a.connection_id<>ep.connection_id OR a.method_code<>'stripe_checkout' THEN
    v_disposition:='QUARANTINED'; v_reason:='profile_mismatch';
   ELSE
    SELECT x.* INTO sess FROM payments.stripe_sessions x WHERE x.attempt_id=a.id FOR UPDATE;
    IF sess.charge_signal_count>=64 THEN v_disposition:='IGNORED'; v_reason:='signal_cap';
    ELSE
     v_disposition:='ACCEPTED'; v_reason:='accepted'; v_attempt:=a.id;
     v_tenant:=a.tenant_id; v_store:=a.store_id; v_session:=p_session_id;
     v_signal:=gen_random_uuid();
    END IF;
   END IF;
  END IF;
 ELSE
  SELECT x.* INTO sess FROM payments.stripe_sessions x
   WHERE x.session_id=p_session_id AND x.account_id=ep.account_id AND x.environment=ep.environment;
  IF NOT FOUND AND p_client_reference=p_metadata_attempt
   AND p_client_reference ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' THEN
   SELECT x.* INTO sess FROM payments.stripe_sessions x
    WHERE x.attempt_id=p_client_reference::uuid AND x.account_id=ep.account_id
     AND x.environment=ep.environment;
  END IF;
  IF sess.attempt_id IS NOT NULL THEN
   SELECT x.* INTO a FROM checkout.payment_attempts x WHERE x.id=sess.attempt_id
    AND x.tenant_id=sess.tenant_id AND x.store_id=sess.store_id;
   IF a.id IS NULL OR a.execution_profile<>ep.execution_profile
    OR a.connection_id<>ep.connection_id OR a.method_code<>'stripe_checkout' THEN
    v_reason:='profile_mismatch';
   ELSIF p_client_reference IS DISTINCT FROM a.id::text
    OR p_metadata_attempt IS DISTINCT FROM a.id::text THEN
    v_reason:='reference_mismatch';
   ELSE
    SELECT x.* INTO sess FROM payments.stripe_sessions x WHERE x.attempt_id=a.id FOR UPDATE;
    IF sess.signal_count>=64 THEN v_disposition:='IGNORED'; v_reason:='signal_cap';
    ELSE
     v_disposition:='ACCEPTED'; v_reason:='accepted'; v_attempt:=a.id;
     v_tenant:=a.tenant_id; v_store:=a.store_id; v_session:=p_session_id;
     v_signal:=gen_random_uuid();
    END IF;
   END IF;
  ELSIF p_client_reference IS DISTINCT FROM p_metadata_attempt THEN
   v_reason:='reference_mismatch';
  END IF;
 END IF;
 v_receipt:=gen_random_uuid();
 INSERT INTO payments.stripe_webhook_receipts(id,endpoint_id,environment,account_id,event_id,event_type,
  event_created,api_version,object_type,session_id,body_sha256,signed_at,disposition,reason,
  attempt_id,tenant_id,store_id,signal_id,refund_id)
 VALUES(v_receipt,ep.endpoint_id,ep.environment,ep.account_id,p_event_id,p_event_type,p_event_created,
  p_api_version,p_object_type,nullif(p_session_id,''),p_body_sha256,p_signed_at,
  v_disposition,v_reason,v_attempt,v_tenant,v_store,v_signal,v_refund);
 RETURN QUERY SELECT CASE WHEN v_disposition='ACCEPTED' THEN 'ACCEPT_PENDING' ELSE v_disposition END,
  v_receipt,v_attempt,v_session,v_signal,v_refund,p_object_type;
END $$;
ALTER FUNCTION payments.stripe_webhook_prepare(uuid,bigint,text,text,bigint,text,text,text,text,text,boolean,boolean,boolean,boolean,bytea,bigint,text,text)
 OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION payments.stripe_webhook_prepare(uuid,bigint,text,text,bigint,text,text,text,text,text,boolean,boolean,boolean,boolean,bytea,bigint,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION payments.stripe_webhook_prepare(uuid,bigint,text,text,bigint,text,text,text,text,text,boolean,boolean,boolean,boolean,bytea,bigint,text,text)
 TO commerce_stripe_ingress;
COMMENT ON FUNCTION payments.stripe_webhook_prepare(uuid,bigint,text,text,bigint,text,text,text,text,text,boolean,boolean,boolean,boolean,bytea,bigint,text,text) IS 'payments owner; signed ingress metadata dedupe and scoped signal preallocation for checkout, refund and charge objects (stripe-refund-v1 §7.3); no raw body or financial claim';

-- ------------------------------------------------------------------------------------------------
-- §4.4 payments.apply_stripe_observation: CREATE OR REPLACE of the 0061 body with ONE guard change
-- (marked below). Owner and ACL are retained. RF12 records the old/new body hashes and the diff.
-- ------------------------------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION payments.apply_stripe_observation(p_attempt uuid,p_report_hash bytea)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE a checkout.payment_attempts%ROWTYPE; obs payments.provider_observations%ROWTYPE;
 op integration.operations%ROWTYPE; acct integration.merchant_accounts%ROWTYPE;
 sess payments.stripe_sessions%ROWTYPE; ord checkout.orders%ROWTYPE;
 res inventory.reservations%ROWTYPE; ln record; r jsonb;
 v_now timestamptz; v_money_check boolean; v_paid boolean; v_closed boolean;
 v_review boolean; v_captured boolean; v_new_capture boolean:=false;
 v_closed_before boolean; v_reference text; v_valid_close boolean;
BEGIN
 IF p_attempt IS NULL OR p_report_hash IS NULL OR octet_length(p_report_hash)<>32 THEN
  RAISE EXCEPTION 'invalid capture input' USING ERRCODE='22023'; END IF;
 SELECT x.* INTO a FROM checkout.payment_attempts x WHERE x.id=p_attempt;
 IF NOT FOUND THEN RAISE EXCEPTION 'payment observation unavailable' USING ERRCODE='PT409'; END IF;
 PERFORM set_config('app.tenant_id',a.tenant_id::text,true);
 PERFORM set_config('app.store_id',a.store_id::text,true);
 PERFORM set_config('app.buyer_id',a.owner_id::text,true);
 PERFORM set_config('app.principal_id','',true);
 SELECT x.* INTO obs FROM payments.provider_observations x
  WHERE x.tenant_id=a.tenant_id AND x.store_id=a.store_id AND x.attempt_id=a.id
   AND x.report_hash=p_report_hash AND x.source IN ('QUERY','LOCAL');
 IF NOT FOUND OR obs.execution_profile<>a.execution_profile OR obs.environment<>a.environment
  OR obs.report_hash<>sha256(convert_to(obs.report::text,'UTF8')) THEN
  RAISE EXCEPTION 'payment observation mismatch' USING ERRCODE='PT409'; END IF;
 r:=obs.report;
 SELECT x.* INTO op FROM integration.operations x WHERE x.id=a.id AND x.tenant_id=a.tenant_id AND x.store_id=a.store_id;
 SELECT x.* INTO acct FROM integration.merchant_accounts x WHERE x.id=a.connection_id
  AND x.tenant_id=a.tenant_id AND x.store_id=a.store_id;
 SELECT x.* INTO sess FROM payments.stripe_sessions x WHERE x.attempt_id=a.id
  AND x.tenant_id=a.tenant_id AND x.store_id=a.store_id;
 IF op.id IS NULL OR acct.id IS NULL OR sess.attempt_id IS NULL
  OR op.actor_kind<>'BUYER_PAYMENT_QUERY' OR op.payment_attempt_id<>a.id
  OR op.buyer_owner_id<>a.owner_id OR op.buyer_session_id<>a.session_id
  OR op.binding_id<>a.binding_id OR op.binding_version<>a.binding_version
  OR op.provider<>'stripe' OR op.action<>'stripe.checkout_session' OR op.purpose<>'transactional'
  OR op.external_asset_id<>acct.environment||':'||acct.account_id
  OR acct.provider<>'stripe' OR acct.environment<>a.environment OR acct.binding_id<>a.binding_id
  OR sess.environment<>a.environment OR sess.account_id<>acct.account_id
  OR (a.execution_profile='PROVIDER_MOCK' AND a.environment<>'SANDBOX')
  OR (a.execution_profile<>'PROVIDER_MOCK' AND a.execution_profile<>a.environment)
  OR a.method_code<>'stripe_checkout' OR r->>'Provider'<>'stripe' OR r->'Version'<>'1'::jsonb
  OR r->>'AccountID'<>sess.account_id THEN
  RAISE EXCEPTION 'Stripe payment provenance mismatch' USING ERRCODE='PT409'; END IF;
 -- §11.5: the order is the aggregate lock, followed by reservation and sorted balances.
 SELECT x.* INTO ord FROM checkout.orders x WHERE x.tenant_id=a.tenant_id AND x.store_id=a.store_id
  AND x.owner_id=a.owner_id AND x.id=a.order_id FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'payment order unavailable' USING ERRCODE='PT409'; END IF;
 PERFORM set_config('app.buyer_session_id',ord.creator_session_id::text,true);
 SELECT x.* INTO res FROM inventory.reservations x WHERE x.tenant_id=a.tenant_id
  AND x.store_id=a.store_id AND x.id=ord.id FOR UPDATE;
 IF NOT FOUND OR res.checkout_id<>ord.id OR res.buyer_owner_id<>ord.owner_id
  OR res.buyer_session_id<>ord.creator_session_id OR a.owner_id<>ord.owner_id
  OR a.currency<>ord.currency OR a.amount_minor<>ord.total_minor THEN
  RAISE EXCEPTION 'payment aggregate mismatch' USING ERRCODE='PT409'; END IF;
 v_now:=clock_timestamp();
 -- I05: a provider identity/money conflict creates review evidence before any terminal early return.
 IF (r->>'SessionID'<>'' AND (r->>'ClientReferenceID' IS DISTINCT FROM a.id::text
  OR r->>'MetadataAttempt' IS DISTINCT FROM a.id::text
  OR r->>'MetadataProfile' IS DISTINCT FROM a.execution_profile
  OR r->>'Mode' IS DISTINCT FROM 'payment'
  OR r->'PaymentMethodTypes' IS DISTINCT FROM '["card"]'::jsonb
  OR (r->>'Livemode')::boolean IS DISTINCT FROM (a.environment='LIVE')
  OR (r->>'ExpiresAt')::bigint IS DISTINCT FROM extract(epoch FROM sess.expires_at)::bigint)) THEN
  INSERT INTO payments.review_cases(tenant_id,store_id,attempt_id,reason,source_report_hash)
   VALUES(a.tenant_id,a.store_id,a.id,'PROVIDER_IDENTITY_MISMATCH',obs.report_hash)
   ON CONFLICT DO NOTHING;
  RETURN;
 END IF;
 IF r->>'SessionID'<>'' AND r->>'SessionID' IS DISTINCT FROM sess.session_id THEN
  INSERT INTO payments.review_cases(tenant_id,store_id,attempt_id,reason,source_report_hash)
   VALUES(a.tenant_id,a.store_id,a.id,'PROVIDER_SESSION_DUPLICATE',obs.report_hash)
   ON CONFLICT DO NOTHING;
  RETURN;
 END IF;
 v_money_check:=r->>'Status' IN ('complete','expired') OR r->>'PaymentStatus'='paid';
 IF v_money_check AND
  (r->>'Currency' IS DISTINCT FROM a.currency
   OR (r->>'AmountTotal')::bigint IS DISTINCT FROM sess.unit_amount
   OR (r->>'AmountSubtotal')::bigint IS DISTINCT FROM (r->>'AmountTotal')::bigint
   OR (r->>'AmountDiscount')::bigint IS DISTINCT FROM 0
   OR (r->>'AmountTax')::bigint IS DISTINCT FROM 0
   OR (r->>'AmountShipping')::bigint IS DISTINCT FROM 0
   OR (r->>'PaymentStatus'='paid' AND
    ((r->>'PaymentIntentAmountReceived')::bigint IS DISTINCT FROM (r->>'AmountTotal')::bigint
     OR r->>'PaymentIntentCurrency' IS DISTINCT FROM r->>'Currency'))) THEN
  INSERT INTO payments.review_cases(tenant_id,store_id,attempt_id,reason,source_report_hash)
   VALUES(a.tenant_id,a.store_id,a.id,'PROVIDER_AMOUNT_MISMATCH',obs.report_hash)
   ON CONFLICT DO NOTHING;
  RETURN;
 END IF;
 IF r->>'PresentmentCurrency' NOT IN ('',r->>'Currency')
  OR r->'CurrencyConversion'='true'::jsonb
  OR (r->>'PresentmentAmount' IS NOT NULL
   AND (r->>'PresentmentAmount')::bigint<>(r->>'AmountTotal')::bigint) THEN
  INSERT INTO payments.review_cases(tenant_id,store_id,attempt_id,reason,source_report_hash)
   VALUES(a.tenant_id,a.store_id,a.id,'PROVIDER_PRESENTMENT_DRIFT',obs.report_hash)
   ON CONFLICT DO NOTHING;
 END IF;
 IF obs.source='LOCAL' AND r->>'Via'='escalate' THEN
  IF r->>'LocalReason'='EXPIRY_UNCONFIRMED' AND v_now>=sess.expires_at+interval '60 minutes'
   AND EXISTS(SELECT 1 FROM payments.provider_observations x WHERE x.attempt_id=a.id
    AND x.tenant_id=a.tenant_id AND x.store_id=a.store_id AND x.source='QUERY'
    AND x.report->>'Status'='open'
    AND NOT EXISTS(SELECT 1 FROM payments.provider_observations newer
     WHERE newer.attempt_id=x.attempt_id AND newer.tenant_id=x.tenant_id
      AND newer.store_id=x.store_id AND newer.source='QUERY' AND newer.received_at>x.received_at)) THEN
   INSERT INTO payments.review_cases(tenant_id,store_id,attempt_id,reason,source_report_hash)
    VALUES(a.tenant_id,a.store_id,a.id,'PROVIDER_EXPIRY_UNCONFIRMED',obs.report_hash)
    ON CONFLICT DO NOTHING;
  ELSIF r->>'LocalReason'='ASYNC_PENDING' AND EXISTS(
   SELECT 1 FROM payments.provider_observations x WHERE x.attempt_id=a.id
    AND x.tenant_id=a.tenant_id AND x.store_id=a.store_id AND x.source='QUERY'
    AND x.report->>'Status'='complete' AND x.report->>'PaymentStatus'='unpaid'
    AND x.received_at<=v_now-interval '60 minutes') THEN
   INSERT INTO payments.review_cases(tenant_id,store_id,attempt_id,reason,source_report_hash)
    VALUES(a.tenant_id,a.store_id,a.id,'PROVIDER_ASYNC_PENDING',obs.report_hash)
    ON CONFLICT DO NOTHING;
  END IF;
  RETURN;
 END IF;
 SELECT EXISTS(SELECT 1 FROM payments.facts f WHERE f.tenant_id=a.tenant_id
  AND f.store_id=a.store_id AND f.attempt_id=a.id AND f.kind='CLOSED_UNPAID') INTO v_closed_before;
 SELECT EXISTS(SELECT 1 FROM payments.facts f WHERE f.tenant_id=a.tenant_id
  AND f.store_id=a.store_id AND f.attempt_id=a.id AND f.kind='CAPTURED') INTO v_captured;
 v_paid:=r->>'Status'='complete' AND r->>'PaymentStatus'='paid'
  AND r->>'PaymentIntentStatus'='succeeded';
 IF r->>'Status'='complete' AND r->>'PaymentStatus'='paid' AND NOT v_paid THEN
  INSERT INTO payments.review_cases(tenant_id,store_id,attempt_id,reason,source_report_hash)
   VALUES(a.tenant_id,a.store_id,a.id,'CONFLICTING_REPORT',obs.report_hash)
   ON CONFLICT DO NOTHING;
 END IF;
 IF v_paid THEN
  INSERT INTO payments.facts(tenant_id,store_id,attempt_id,kind,amount_minor,currency,
   provider_reference,connection_id,execution_profile,environment,source_report_hash)
   VALUES(a.tenant_id,a.store_id,a.id,'CAPTURED',a.amount_minor,a.currency,
    sess.session_id,a.connection_id,a.execution_profile,a.environment,obs.report_hash)
   ON CONFLICT(tenant_id,store_id,attempt_id,kind) DO NOTHING;
  v_new_capture:=FOUND;
  v_captured:=true;
  IF v_closed_before THEN
   INSERT INTO payments.review_cases(tenant_id,store_id,attempt_id,reason,source_report_hash)
    VALUES(a.tenant_id,a.store_id,a.id,'CLOSURE_CONTRADICTED',obs.report_hash),
     (a.tenant_id,a.store_id,a.id,'PAID_ALLOCATION_FAILED',obs.report_hash)
    ON CONFLICT DO NOTHING;
  ELSIF v_new_capture AND (res.state<>'PAYMENT_PENDING' OR ord.commercial_state<>'AWAITING_PAYMENT'
   OR a.generation<>ord.generation OR res.generation<>ord.generation) THEN
   INSERT INTO payments.review_cases(tenant_id,store_id,attempt_id,reason,source_report_hash)
    VALUES(a.tenant_id,a.store_id,a.id,'PAID_ALLOCATION_FAILED',obs.report_hash)
    ON CONFLICT DO NOTHING;
  END IF;
  SELECT EXISTS(SELECT 1 FROM payments.review_cases c WHERE c.tenant_id=a.tenant_id
   AND c.store_id=a.store_id AND c.attempt_id=a.id) INTO v_review;
  -- stripe-refund-v1 §4.4 (A1): the ONLY delta against 0061. A review inserted after the capture (every
  -- REFUND_% reason, REFUND_HISTORY, a later CONFLICTING_REPORT or PROVIDER_PRESENTMENT_DRIFT) never
  -- flips the order to PAID_ALLOCATION_FAILED nor rewrites the work item; it falls through to the
  -- existing NOT v_new_capture return below.
  IF v_review AND (v_new_capture OR v_closed_before) THEN
   IF v_closed_before OR (res.state<>'PAYMENT_PENDING' OR ord.commercial_state<>'AWAITING_PAYMENT') THEN
    UPDATE checkout.orders SET fulfillment_state='PAID_ALLOCATION_FAILED',updated_at=clock_timestamp()
     WHERE tenant_id=a.tenant_id AND store_id=a.store_id AND owner_id=a.owner_id AND id=ord.id;
   END IF;
   INSERT INTO fulfillment.payment_work_items(tenant_id,store_id,owner_id,order_id,attempt_id,state)
    VALUES(a.tenant_id,a.store_id,a.owner_id,ord.id,a.id,'REVIEW_REQUIRED')
    ON CONFLICT(tenant_id,store_id,order_id) DO UPDATE SET state='REVIEW_REQUIRED';
   RETURN;
  END IF;
  IF NOT v_new_capture OR EXISTS(SELECT 1 FROM fulfillment.payment_work_items w
   WHERE w.tenant_id=a.tenant_id AND w.store_id=a.store_id AND w.order_id=ord.id) THEN RETURN; END IF;
  FOR ln IN SELECT l.warehouse_id,l.sku_id,l.quantity FROM inventory.reservation_lines l
   WHERE l.tenant_id=a.tenant_id AND l.store_id=a.store_id AND l.reservation_id=ord.id
   ORDER BY l.warehouse_id,l.sku_id LOOP
   PERFORM 1 FROM inventory.lock_balance(ln.warehouse_id,ln.sku_id);
  END LOOP;
  IF NOT FOUND THEN RAISE EXCEPTION 'payment reservation empty' USING ERRCODE='PT409'; END IF;
  FOR ln IN SELECT l.warehouse_id,l.sku_id,l.quantity FROM inventory.reservation_lines l
   WHERE l.tenant_id=a.tenant_id AND l.store_id=a.store_id AND l.reservation_id=ord.id
   ORDER BY l.warehouse_id,l.sku_id LOOP
   INSERT INTO inventory.ledger(tenant_id,store_id,warehouse_id,sku_id,kind,delta_reserved,delta_allocated,
    operation,command_key,reservation_id,principal_id,checkout_id,buyer_owner_id,buyer_session_id,
    actor_kind,payment_attempt_id,payment_fact_kind)
    VALUES(a.tenant_id,a.store_id,ln.warehouse_id,ln.sku_id,'ALLOCATE',-ln.quantity,ln.quantity,
     'checkout.payment.capture',a.id::text,ord.id,NULL,ord.id,ord.owner_id,ord.creator_session_id,
     'SYSTEM_PAYMENT',a.id,'CAPTURED');
  END LOOP;
  UPDATE inventory.reservations SET state='COMMITTED' WHERE tenant_id=a.tenant_id
   AND store_id=a.store_id AND id=ord.id AND state='PAYMENT_PENDING';
  UPDATE checkout.orders SET commercial_state='CONFIRMED',updated_at=clock_timestamp()
   WHERE tenant_id=a.tenant_id AND store_id=a.store_id AND owner_id=a.owner_id AND id=ord.id
    AND commercial_state='AWAITING_PAYMENT';
  INSERT INTO checkout.events(tenant_id,store_id,owner_id,order_id,session_id,generation,action,actor_kind)
   VALUES(a.tenant_id,a.store_id,a.owner_id,ord.id,ord.creator_session_id,ord.generation,
    'checkout.payment_captured','SYSTEM_PAYMENT');
  INSERT INTO fulfillment.payment_work_items(tenant_id,store_id,owner_id,order_id,attempt_id,state)
   VALUES(a.tenant_id,a.store_id,a.owner_id,ord.id,a.id,'READY');
  RETURN;
 END IF;
 v_valid_close:=NOT v_captured AND NOT v_closed_before AND NOT EXISTS(
  SELECT 1 FROM payments.review_cases c WHERE c.tenant_id=a.tenant_id AND c.store_id=a.store_id
   AND c.attempt_id=a.id AND c.reason IN ('PROVIDER_AMOUNT_MISMATCH','PROVIDER_SESSION_DUPLICATE'))
  AND NOT EXISTS(
  SELECT 1 FROM payments.facts f WHERE f.tenant_id=a.tenant_id AND f.store_id=a.store_id
   AND f.attempt_id=a.id AND f.kind='AUTHORIZED') AND (
   (r->>'Status'='expired' AND r->>'PaymentStatus'='unpaid'
    AND r->>'PaymentIntentStatus' IN ('','canceled','requires_payment_method'))
   OR (r->>'Via'='create' AND r->>'ErrorClass'='rejected'
    AND (r->>'SendCount')::integer=1 AND sess.session_id IS NULL
    -- RD4: only the very first send proves non-existence; after any resend the key may have executed.
    AND sess.create_send_count=1 AND sess.create_last_sent_at=sess.create_first_sent_at)
   OR (r->>'Via'='list' AND (r->>'ListMatchCount')::integer=0 AND sess.session_id IS NULL
    AND sess.create_first_sent_at IS NOT NULL AND v_now>=sess.expires_at+interval '15 minutes')
   OR (obs.source='LOCAL' AND r->>'Via'='unsent' AND sess.create_suppressed_at IS NOT NULL
    AND sess.create_first_sent_at IS NULL));
 IF v_valid_close THEN
  v_reference:=coalesce(sess.session_id,a.merchant_trade_no);
  INSERT INTO payments.facts(tenant_id,store_id,attempt_id,kind,amount_minor,currency,
   provider_reference,connection_id,execution_profile,environment,source_report_hash)
   VALUES(a.tenant_id,a.store_id,a.id,'CLOSED_UNPAID',0,a.currency,v_reference,
    a.connection_id,a.execution_profile,a.environment,obs.report_hash)
   ON CONFLICT(tenant_id,store_id,attempt_id,kind) DO NOTHING;
  v_closed:=FOUND;
  IF v_closed AND res.state='PAYMENT_PENDING' AND ord.commercial_state='AWAITING_PAYMENT'
   AND a.generation=ord.generation AND res.generation=ord.generation THEN
   -- §11.5: only a CLOSED_UNPAID fact permits releasing reserved inventory.
   FOR ln IN SELECT l.warehouse_id,l.sku_id,l.quantity FROM inventory.reservation_lines l
    WHERE l.tenant_id=a.tenant_id AND l.store_id=a.store_id AND l.reservation_id=ord.id
    ORDER BY l.warehouse_id,l.sku_id LOOP
    PERFORM 1 FROM inventory.lock_balance(ln.warehouse_id,ln.sku_id);
   END LOOP;
   IF NOT FOUND THEN RAISE EXCEPTION 'payment reservation empty' USING ERRCODE='PT409'; END IF;
   FOR ln IN SELECT l.warehouse_id,l.sku_id,l.quantity FROM inventory.reservation_lines l
    WHERE l.tenant_id=a.tenant_id AND l.store_id=a.store_id AND l.reservation_id=ord.id
    ORDER BY l.warehouse_id,l.sku_id LOOP
    INSERT INTO inventory.ledger(tenant_id,store_id,warehouse_id,sku_id,kind,delta_reserved,delta_allocated,
     operation,command_key,reservation_id,principal_id,checkout_id,buyer_owner_id,buyer_session_id,
     actor_kind,payment_attempt_id,payment_fact_kind)
     VALUES(a.tenant_id,a.store_id,ln.warehouse_id,ln.sku_id,'RELEASE',-ln.quantity,0,
      'checkout.payment.close',a.id::text,ord.id,NULL,ord.id,ord.owner_id,ord.creator_session_id,
      'SYSTEM_PAYMENT',a.id,'CLOSED_UNPAID');
   END LOOP;
   UPDATE inventory.reservations SET state='RELEASED' WHERE tenant_id=a.tenant_id
    AND store_id=a.store_id AND id=ord.id AND state='PAYMENT_PENDING';
   UPDATE checkout.orders SET commercial_state='CANCELLED',fulfillment_state='CANCELLED',
    updated_at=clock_timestamp() WHERE tenant_id=a.tenant_id AND store_id=a.store_id
    AND owner_id=a.owner_id AND id=ord.id AND commercial_state='AWAITING_PAYMENT';
   INSERT INTO checkout.events(tenant_id,store_id,owner_id,order_id,session_id,generation,action,actor_kind)
    VALUES(a.tenant_id,a.store_id,a.owner_id,ord.id,ord.creator_session_id,ord.generation,
     'checkout.payment_closed','SYSTEM_PAYMENT');
  END IF;
 ELSIF (r->>'Status'='expired' AND (r->>'PaymentStatus'='paid'
  OR r->>'PaymentIntentStatus'='succeeded')) OR r->>'PaymentStatus'='no_payment_required' THEN
  INSERT INTO payments.review_cases(tenant_id,store_id,attempt_id,reason,source_report_hash)
   VALUES(a.tenant_id,a.store_id,a.id,'CONFLICTING_REPORT',obs.report_hash)
   ON CONFLICT DO NOTHING;
 END IF;
END $$;
COMMENT ON FUNCTION payments.apply_stripe_observation(uuid,bytea) IS 'payments owner; private Stripe observation to immutable facts and single inventory ledger path; post-capture reviews never change order or work-item state (stripe-refund-v1 §4.4); no network or webhook authority';

-- ------------------------------------------------------------------------------------------------
-- §6 apply rules. Called only by the payments.apply_capture dispatcher (checkout_writer owner, no
-- login EXECUTE). Lock order: order first, then the refund row; operations are never locked here.
-- ------------------------------------------------------------------------------------------------
CREATE FUNCTION payments.apply_stripe_refund(p_attempt uuid,p_report_hash bytea)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE a checkout.payment_attempts%ROWTYPE; obs payments.provider_observations%ROWTYPE;
 ord checkout.orders%ROWTYPE; rf payments.stripe_refunds%ROWTYPE; r jsonb; v_now timestamptz;
 v_status text; v_via text; v_reason text; v_captured bigint; v_sum bigint; v_fact_at timestamptz;
BEGIN
 IF p_attempt IS NULL OR p_report_hash IS NULL OR octet_length(p_report_hash)<>32 THEN
  RAISE EXCEPTION 'invalid refund input' USING ERRCODE='22023'; END IF;
 SELECT x.* INTO a FROM checkout.payment_attempts x WHERE x.id=p_attempt;
 IF NOT FOUND OR a.method_code<>'stripe_checkout' THEN
  RAISE EXCEPTION 'refund observation unavailable' USING ERRCODE='PT409'; END IF;
 PERFORM set_config('app.tenant_id',a.tenant_id::text,true);
 PERFORM set_config('app.store_id',a.store_id::text,true);
 PERFORM set_config('app.buyer_id',a.owner_id::text,true);
 PERFORM set_config('app.principal_id','',true);
 SELECT x.* INTO obs FROM payments.provider_observations x
  WHERE x.tenant_id=a.tenant_id AND x.store_id=a.store_id AND x.attempt_id=a.id
   AND x.report_hash=p_report_hash AND x.source IN ('QUERY','LOCAL');
 IF NOT FOUND OR obs.execution_profile<>a.execution_profile OR obs.environment<>a.environment
  OR obs.report_hash<>sha256(convert_to(obs.report::text,'UTF8')) THEN
  RAISE EXCEPTION 'refund observation mismatch' USING ERRCODE='PT409'; END IF;
 r:=obs.report;
 IF r->>'Provider'<>'stripe' OR r->>'Object' IS DISTINCT FROM 'refund'
  OR r->>'RefundRef' !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' THEN
  RAISE EXCEPTION 'refund observation mismatch' USING ERRCODE='PT409'; END IF;
 -- §11.5: the order is the aggregate lock (same first lock as capture and the request).
 SELECT x.* INTO ord FROM checkout.orders x WHERE x.tenant_id=a.tenant_id AND x.store_id=a.store_id
  AND x.owner_id=a.owner_id AND x.id=a.order_id FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'refund order unavailable' USING ERRCODE='PT409'; END IF;
 SELECT x.* INTO rf FROM payments.stripe_refunds x WHERE x.id=(r->>'RefundRef')::uuid
  AND x.tenant_id=a.tenant_id AND x.store_id=a.store_id AND x.attempt_id=a.id
  AND x.order_id=ord.id FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'refund unavailable' USING ERRCODE='PT409'; END IF;
 v_now:=clock_timestamp();
 v_via:=r->>'Via';
 -- 1. defensive identity guard (the record definer already refused a mismatch)
 IF r->>'RefundID'<>'' AND (r->>'MetadataRefund' IS DISTINCT FROM rf.id::text
  OR r->>'MetadataAttempt' IS DISTINCT FROM a.id::text
  OR r->>'PaymentIntentID' IS DISTINCT FROM rf.payment_intent_id
  OR r->>'AccountID' IS DISTINCT FROM rf.account_id
  OR r->'Livemode' IS DISTINCT FROM 'false'::jsonb) THEN
  INSERT INTO payments.review_cases(tenant_id,store_id,attempt_id,reason,source_report_hash)
   VALUES(a.tenant_id,a.store_id,a.id,'PROVIDER_IDENTITY_MISMATCH',obs.report_hash) ON CONFLICT DO NOTHING;
  RETURN;
 END IF;
 -- 2. local and definitive closures; capacity is released only by these or by provider proof (RD4)
 -- RD4: only the very first send may prove non-existence. Any resend (send_count>1, or a last send
 -- after the first) means the key may have executed, so a stale rejection report releases nothing.
 IF v_via='create' AND r->>'ErrorClass'='rejected' AND (r->>'SendCount')::integer=1
  AND rf.stripe_refund_id IS NULL AND r->>'RefundID'='' AND rf.send_count=1
  AND rf.last_sent_at=rf.first_sent_at THEN
  INSERT INTO payments.refund_facts(tenant_id,store_id,refund_id,attempt_id,kind,amount_minor,currency,
   stripe_refund_id,failure_reason,source_report_hash)
   VALUES(a.tenant_id,a.store_id,rf.id,a.id,'REJECTED',rf.amount_minor,rf.currency,NULL,
    'first_send_rejected',obs.report_hash) ON CONFLICT DO NOTHING;
  RETURN;
 END IF;
 IF obs.source='LOCAL' AND v_via='unsent' THEN
  v_reason:=CASE r->>'LocalReason' WHEN 'external_refund' THEN 'external_refund_detected'
   WHEN 'send_window_closed' THEN 'send_window_closed' ELSE NULL END;
  IF v_reason IS NOT NULL AND rf.suppressed_at IS NOT NULL AND rf.first_sent_at IS NULL THEN
   INSERT INTO payments.refund_facts(tenant_id,store_id,refund_id,attempt_id,kind,amount_minor,currency,
    stripe_refund_id,failure_reason,source_report_hash)
    VALUES(a.tenant_id,a.store_id,rf.id,a.id,'REJECTED',rf.amount_minor,rf.currency,NULL,
     v_reason,obs.report_hash) ON CONFLICT DO NOTHING;
  END IF;
  RETURN;
 END IF;
 IF obs.source='LOCAL' AND v_via='escalate' THEN
  IF r->>'LocalReason'='binding_changed' THEN
   INSERT INTO payments.review_cases(tenant_id,store_id,attempt_id,reason,source_report_hash)
    VALUES(a.tenant_id,a.store_id,a.id,'REFUND_UNRESOLVED',obs.report_hash) ON CONFLICT DO NOTHING;
  END IF;
  RETURN;
 END IF;
 -- 3. list closure mirrors checkout's expires_at+15min rule; earlier zero-match reports are no-ops
 IF v_via='list' AND (r->>'ListMatchCount')::integer=0 THEN
  IF rf.stripe_refund_id IS NULL AND rf.last_sent_at IS NOT NULL
   AND v_now>=greatest(rf.resend_until,rf.last_sent_at+interval '15 minutes') THEN
   INSERT INTO payments.review_cases(tenant_id,store_id,attempt_id,reason,source_report_hash)
    VALUES(a.tenant_id,a.store_id,a.id,'REFUND_UNRESOLVED',obs.report_hash) ON CONFLICT DO NOTHING;
  END IF;
  RETURN;
 END IF;
 IF r->>'RefundID'='' THEN RETURN; END IF;
 -- 4. I05: currency and amount must equal the request, else review and no fact
 IF r->>'Currency' IS DISTINCT FROM rf.currency
  OR (r->>'Amount')::bigint IS DISTINCT FROM rf.amount_minor THEN
  INSERT INTO payments.review_cases(tenant_id,store_id,attempt_id,reason,source_report_hash)
   VALUES(a.tenant_id,a.store_id,a.id,'REFUND_AMOUNT_MISMATCH',obs.report_hash) ON CONFLICT DO NOTHING;
  RETURN;
 END IF;
 -- 5. status to fact
 v_status:=r->>'Status';
 IF v_status='succeeded' THEN
  SELECT so.received_at INTO v_fact_at FROM payments.refund_facts f
   JOIN payments.provider_observations so ON so.tenant_id=f.tenant_id AND so.store_id=f.store_id
    AND so.attempt_id=f.attempt_id AND so.report_hash=f.source_report_hash
   WHERE f.tenant_id=a.tenant_id AND f.store_id=a.store_id AND f.refund_id=rf.id
    AND f.kind IN ('FAILED','CANCELED') ORDER BY so.received_at DESC LIMIT 1;
  IF FOUND THEN
   -- Stripe only moves succeeded to failed: an older succeeded report is stale, a newer one conflicts.
   IF v_fact_at<obs.received_at THEN
    INSERT INTO payments.review_cases(tenant_id,store_id,attempt_id,reason,source_report_hash)
     VALUES(a.tenant_id,a.store_id,a.id,'REFUND_CONFLICTING',obs.report_hash) ON CONFLICT DO NOTHING;
   END IF;
   RETURN;
  END IF;
  INSERT INTO payments.refund_facts(tenant_id,store_id,refund_id,attempt_id,kind,amount_minor,currency,
   stripe_refund_id,failure_reason,source_report_hash)
   VALUES(a.tenant_id,a.store_id,rf.id,a.id,'SUCCEEDED',rf.amount_minor,rf.currency,rf.stripe_refund_id,
    NULL,obs.report_hash) ON CONFLICT DO NOTHING;
 ELSIF v_status IN ('failed','canceled') THEN
  v_reason:=CASE WHEN r->>'FailureReason' IN ('lost_or_stolen_card','expired_or_canceled_card',
   'charge_for_pending_refund_disputed','insufficient_funds','declined','merchant_request','unknown')
   THEN r->>'FailureReason' ELSE 'unknown' END;
  IF NOT EXISTS(SELECT 1 FROM payments.refund_facts f WHERE f.tenant_id=a.tenant_id AND f.store_id=a.store_id
   AND f.refund_id=rf.id AND f.kind IN ('FAILED','CANCELED')) THEN
   INSERT INTO payments.refund_facts(tenant_id,store_id,refund_id,attempt_id,kind,amount_minor,currency,
    stripe_refund_id,failure_reason,source_report_hash)
    VALUES(a.tenant_id,a.store_id,rf.id,a.id,CASE v_status WHEN 'failed' THEN 'FAILED' ELSE 'CANCELED' END,
     rf.amount_minor,rf.currency,rf.stripe_refund_id,v_reason,obs.report_hash) ON CONFLICT DO NOTHING;
  END IF;
 ELSIF v_status='requires_action' AND rf.pinned_at IS NOT NULL
  AND obs.received_at>=rf.pinned_at+interval '60 minutes' THEN
  INSERT INTO payments.review_cases(tenant_id,store_id,attempt_id,reason,source_report_hash)
   VALUES(a.tenant_id,a.store_id,a.id,'REFUND_CONFLICTING',obs.report_hash) ON CONFLICT DO NOTHING;
  RETURN;
 ELSE
  RETURN; -- 7. pending / requires_action inside the window: no fact
 END IF;
 -- 6. invariant re-check under the lock: succeeded and not failed may never exceed the capture
 SELECT f.amount_minor INTO v_captured FROM payments.facts f WHERE f.tenant_id=a.tenant_id
  AND f.store_id=a.store_id AND f.attempt_id=a.id AND f.kind='CAPTURED';
 SELECT coalesce(sum(x.amount_minor),0) INTO v_sum FROM payments.stripe_refunds x
  WHERE x.tenant_id=a.tenant_id AND x.store_id=a.store_id AND x.attempt_id=a.id
   AND EXISTS(SELECT 1 FROM payments.refund_facts f WHERE f.tenant_id=x.tenant_id AND f.store_id=x.store_id
    AND f.refund_id=x.id AND f.kind='SUCCEEDED')
   AND NOT EXISTS(SELECT 1 FROM payments.refund_facts f WHERE f.tenant_id=x.tenant_id AND f.store_id=x.store_id
    AND f.refund_id=x.id AND f.kind IN ('FAILED','CANCELED'));
 IF v_captured IS NULL OR v_sum>v_captured THEN
  INSERT INTO payments.review_cases(tenant_id,store_id,attempt_id,reason,source_report_hash)
   VALUES(a.tenant_id,a.store_id,a.id,'REFUND_CONFLICTING',obs.report_hash) ON CONFLICT DO NOTHING;
 END IF;
END $$;
ALTER FUNCTION payments.apply_stripe_refund(uuid,bytea) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION payments.apply_stripe_refund(uuid,bytea) FROM PUBLIC;
COMMENT ON FUNCTION payments.apply_stripe_refund(uuid,bytea) IS 'payments owner; private Stripe refund observation to append-only refund facts and reviews; never stock, order, fulfilment or work-item state (RD6); no network or webhook authority';

CREATE FUNCTION payments.apply_stripe_charge(p_attempt uuid,p_report_hash bytea)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE a checkout.payment_attempts%ROWTYPE; obs payments.provider_observations%ROWTYPE;
 ord checkout.orders%ROWTYPE; r jsonb; v_captured bigint; v_sent bigint;
BEGIN
 IF p_attempt IS NULL OR p_report_hash IS NULL OR octet_length(p_report_hash)<>32 THEN
  RAISE EXCEPTION 'invalid charge input' USING ERRCODE='22023'; END IF;
 SELECT x.* INTO a FROM checkout.payment_attempts x WHERE x.id=p_attempt;
 IF NOT FOUND OR a.method_code<>'stripe_checkout' THEN
  RAISE EXCEPTION 'charge observation unavailable' USING ERRCODE='PT409'; END IF;
 PERFORM set_config('app.tenant_id',a.tenant_id::text,true);
 PERFORM set_config('app.store_id',a.store_id::text,true);
 PERFORM set_config('app.buyer_id',a.owner_id::text,true);
 PERFORM set_config('app.principal_id','',true);
 SELECT x.* INTO obs FROM payments.provider_observations x
  WHERE x.tenant_id=a.tenant_id AND x.store_id=a.store_id AND x.attempt_id=a.id
   AND x.report_hash=p_report_hash AND x.source='QUERY';
 IF NOT FOUND OR obs.execution_profile<>a.execution_profile OR obs.environment<>a.environment
  OR obs.report_hash<>sha256(convert_to(obs.report::text,'UTF8')) THEN
  RAISE EXCEPTION 'charge observation mismatch' USING ERRCODE='PT409'; END IF;
 r:=obs.report;
 IF r->>'Provider'<>'stripe' OR r->>'Object' IS DISTINCT FROM 'charge' THEN
  RAISE EXCEPTION 'charge observation mismatch' USING ERRCODE='PT409'; END IF;
 SELECT x.* INTO ord FROM checkout.orders x WHERE x.tenant_id=a.tenant_id AND x.store_id=a.store_id
  AND x.owner_id=a.owner_id AND x.id=a.order_id FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'charge order unavailable' USING ERRCODE='PT409'; END IF;
 SELECT f.amount_minor INTO v_captured FROM payments.facts f WHERE f.tenant_id=a.tenant_id
  AND f.store_id=a.store_id AND f.attempt_id=a.id AND f.kind='CAPTURED';
 IF NOT FOUND THEN RAISE EXCEPTION 'charge observation before capture' USING ERRCODE='PT409'; END IF;
 -- A refund counts as released for this report only if a failure/cancel/reject fact was recorded from an
 -- observation no newer than this charge snapshot (A1), so replay in any order converges.
 IF r->>'AmountRefunded' IS NOT NULL THEN
  SELECT coalesce(sum(x.amount_minor),0) INTO v_sent FROM payments.stripe_refunds x
   WHERE x.tenant_id=a.tenant_id AND x.store_id=a.store_id AND x.attempt_id=a.id
    AND x.first_sent_at IS NOT NULL
    AND NOT EXISTS(SELECT 1 FROM payments.refund_facts f
     JOIN payments.provider_observations so ON so.tenant_id=f.tenant_id AND so.store_id=f.store_id
      AND so.attempt_id=f.attempt_id AND so.report_hash=f.source_report_hash
     WHERE f.tenant_id=x.tenant_id AND f.store_id=x.store_id AND f.refund_id=x.id
      AND f.kind IN ('FAILED','CANCELED','REJECTED') AND so.received_at<=obs.received_at);
  IF (r->>'AmountRefunded')::bigint>v_sent THEN
   INSERT INTO payments.review_cases(tenant_id,store_id,attempt_id,reason,source_report_hash)
    VALUES(a.tenant_id,a.store_id,a.id,'REFUND_HISTORY',obs.report_hash) ON CONFLICT DO NOTHING;
  END IF;
 END IF;
 IF (r->>'AmountCaptured' IS NOT NULL AND (r->>'AmountCaptured')::bigint<>v_captured)
  OR r->>'Currency' IS DISTINCT FROM a.currency OR r->'Disputed'='true'::jsonb THEN
  -- disputes are out of scope; refunds are then blocked by the CONFLICTING_REPORT review
  INSERT INTO payments.review_cases(tenant_id,store_id,attempt_id,reason,source_report_hash)
   VALUES(a.tenant_id,a.store_id,a.id,'CONFLICTING_REPORT',obs.report_hash) ON CONFLICT DO NOTHING;
 END IF;
END $$;
ALTER FUNCTION payments.apply_stripe_charge(uuid,bytea) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION payments.apply_stripe_charge(uuid,bytea) FROM PUBLIC;
COMMENT ON FUNCTION payments.apply_stripe_charge(uuid,bytea) IS 'payments owner; private Stripe charge snapshot to review evidence only (external refund, capture drift, dispute); never suppresses or mutates refunds, never stock or order state';

-- ------------------------------------------------------------------------------------------------
-- §4.4 dispatcher: CREATE OR REPLACE of the 0061 body; checkout routing is otherwise unchanged and
-- the PAYUNi body behind apply_capture_payuni_v1 is untouched.
-- ------------------------------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION payments.apply_capture(p_attempt uuid,p_report_hash bytea)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_provider text; v_attempt checkout.payment_attempts%ROWTYPE; v_object text;
BEGIN
 IF p_attempt IS NULL OR p_report_hash IS NULL OR octet_length(p_report_hash)<>32 THEN
  RAISE EXCEPTION 'invalid capture input' USING ERRCODE='22023'; END IF;
 SELECT x.* INTO v_attempt FROM checkout.payment_attempts x WHERE x.id=p_attempt;
 IF NOT FOUND THEN RAISE EXCEPTION 'payment operation unavailable' USING ERRCODE='PT409'; END IF;
 PERFORM set_config('app.tenant_id',v_attempt.tenant_id::text,true);
 PERFORM set_config('app.store_id',v_attempt.store_id::text,true);
 PERFORM set_config('app.buyer_id',v_attempt.owner_id::text,true);
 SELECT o.provider INTO v_provider FROM integration.operations o WHERE o.id=p_attempt
  AND o.tenant_id=v_attempt.tenant_id AND o.store_id=v_attempt.store_id
  AND o.actor_kind='BUYER_PAYMENT_QUERY';
 IF v_provider='payuni' THEN
  PERFORM payments.apply_capture_payuni_v1(p_attempt,p_report_hash);
 ELSIF v_provider='stripe' THEN
  -- refund and charge reports carry an Object key; checkout reports never do
  SELECT x.report->>'Object' INTO v_object FROM payments.provider_observations x
   WHERE x.tenant_id=v_attempt.tenant_id AND x.store_id=v_attempt.store_id AND x.attempt_id=p_attempt
    AND x.report_hash=p_report_hash;
  IF v_object='refund' THEN
   PERFORM payments.apply_stripe_refund(p_attempt,p_report_hash);
  ELSIF v_object='charge' THEN
   PERFORM payments.apply_stripe_charge(p_attempt,p_report_hash);
  ELSE
   PERFORM payments.apply_stripe_observation(p_attempt,p_report_hash);
  END IF;
 ELSE
  RAISE EXCEPTION 'payment operation unavailable' USING ERRCODE='PT409';
 END IF;
END $$;
COMMENT ON FUNCTION payments.apply_capture(uuid,bytea) IS 'payments owner; worker calls provider dispatcher; PAYUNi, Stripe checkout, Stripe refund and Stripe charge retain separate private fact paths, no direct ledger write by worker';

-- ------------------------------------------------------------------------------------------------
-- D8: CREATE OR REPLACE checkout.hosted_payment_view_v2 (same signature, owner and ACL). Adds the
-- refund payment states and a `refund` object only for Stripe attempts with refund activity; PAYUNi
-- and refund-free bytes are unchanged. Everything else is the 0061 body.
-- ------------------------------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION checkout.hosted_payment_view_v2(p_hash bytea,p_store uuid,p_order uuid,
 p_profile text,p_payuni_digest bytea,p_stripe_digest bytea) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_base jsonb; scope record; ord checkout.orders%ROWTYPE;
 a checkout.payment_attempts%ROWTYPE; s payments.stripe_sessions%ROWTYPE;
 v_stripe_method jsonb; v_now timestamptz; v_handoff text;
 v_refunded bigint:=0; v_refund_pending bigint:=0; v_refund_activity boolean:=false;
BEGIN
 IF (p_payuni_digest IS NOT NULL AND octet_length(p_payuni_digest)<>32)
  OR (p_stripe_digest IS NOT NULL AND octet_length(p_stripe_digest)<>32)
  OR (p_payuni_digest IS NULL AND p_stripe_digest IS NULL) THEN
  RAISE EXCEPTION 'invalid payment view digest' USING ERRCODE='PT400'; END IF;
 v_base:=checkout.hosted_payment_view(p_hash,p_store,p_order,p_profile,
  coalesce(p_payuni_digest,decode(repeat('00',32),'hex')));
 IF p_payuni_digest IS NULL THEN v_base:=jsonb_set(v_base,'{methods}','[]'::jsonb); END IF;
 SELECT * INTO scope FROM buyer.resolve_scope(p_hash,p_store);
 IF NOT FOUND THEN RAISE EXCEPTION 'invalid buyer capability' USING ERRCODE='PT401'; END IF;
 SELECT x.* INTO ord FROM checkout.orders x WHERE x.tenant_id=scope.tenant_id AND x.store_id=p_store
  AND x.owner_id=scope.owner_id AND x.id=p_order;
 SELECT x.* INTO a FROM checkout.payment_attempts x WHERE x.tenant_id=scope.tenant_id
  AND x.store_id=p_store AND x.owner_id=scope.owner_id AND x.order_id=p_order;
 v_now:=clock_timestamp();
 IF a.id IS NULL AND p_stripe_digest IS NOT NULL AND p_profile IN ('PROVIDER_MOCK','SANDBOX') THEN
  SELECT jsonb_build_object('code',m.code,'version',m.version,'name_hans',m.name_hans,
   'name_hant',m.name_hant,'name_en',m.name_en) INTO v_stripe_method
  FROM payments.method_heads h
  JOIN payments.method_versions m ON m.tenant_id=h.tenant_id AND m.store_id=h.store_id
   AND m.market_id=h.market_id AND m.country=h.country AND m.code=h.code AND m.version=h.current_version
  JOIN integration.merchant_accounts acct ON acct.tenant_id=m.tenant_id AND acct.store_id=m.store_id
   AND acct.id=m.connection_id AND acct.provider='stripe' AND acct.environment='SANDBOX'
  JOIN integration.bindings b ON b.tenant_id=acct.tenant_id AND b.store_id=acct.store_id
   AND b.id=acct.binding_id AND b.provider='stripe' AND b.external_asset_id=acct.binding_asset
   AND b.semantic_version=m.binding_version AND b.enabled
  JOIN payments.account_qualifications q ON q.tenant_id=m.tenant_id AND q.store_id=m.store_id
   AND q.id=m.qualification_id AND q.connection_id=acct.id AND q.credential_version=acct.credential_version
   AND q.environment='SANDBOX' AND q.code='stripe_checkout'
  JOIN inventory.reservations r ON r.tenant_id=ord.tenant_id AND r.store_id=ord.store_id
   AND r.id=ord.id AND r.state='HELD' AND r.generation=ord.generation
  JOIN pricing.markets market ON market.tenant_id=ord.tenant_id AND market.store_id=ord.store_id
   AND market.id=ord.market_id AND market.currency=ord.currency AND market.active
  WHERE h.tenant_id=ord.tenant_id AND h.store_id=ord.store_id AND h.market_id=ord.market_id
   AND h.country=ord.country AND h.code='stripe_checkout' AND ord.commercial_state='DRAFT'
   AND m.enabled AND m.visible AND m.currency=ord.currency
   AND payments.stripe_amount_ok(ord.currency,ord.total_minor)
   AND ord.total_minor BETWEEN m.min_amount_minor AND m.max_amount_minor
   AND ord.expires_at>v_now AND r.expires_at>v_now
   AND q.observed_at<=v_now AND q.expires_at>v_now AND q.revoked_at IS NULL
   AND q.proof_class=CASE WHEN p_profile='PROVIDER_MOCK' THEN 'PROVIDER_MOCK' ELSE 'REAL_SANDBOX' END;
  IF v_stripe_method IS NOT NULL THEN
   v_base:=jsonb_set(v_base,'{methods}',(v_base->'methods')||jsonb_build_array(v_stripe_method));
  END IF;
 END IF;
 IF a.id IS NOT NULL AND a.method_code='stripe_checkout' THEN
  SELECT x.* INTO s FROM payments.stripe_sessions x WHERE x.attempt_id=a.id;
  IF s.attempt_id IS NULL THEN RAISE EXCEPTION 'Stripe session unavailable' USING ERRCODE='PT409'; END IF;
  -- stripe-refund-v1 D8/§7.2: buyer-safe refund totals; no ids, reasons or provider strings. A refund
  -- holds money unless it has a FAILED, CANCELED or REJECTED fact; SUCCEEDED counts only while not failed.
  SELECT coalesce(sum(r.amount_minor) FILTER (WHERE ok.hit AND NOT bad.hit),0),
   coalesce(sum(r.amount_minor) FILTER (WHERE NOT ok.hit AND NOT bad.hit AND NOT rej.hit),0),
   count(*)>0 INTO v_refunded,v_refund_pending,v_refund_activity
  FROM payments.stripe_refunds r
  CROSS JOIN LATERAL (SELECT EXISTS(SELECT 1 FROM payments.refund_facts f WHERE f.tenant_id=r.tenant_id
   AND f.store_id=r.store_id AND f.refund_id=r.id AND f.kind='SUCCEEDED') AS hit) ok
  CROSS JOIN LATERAL (SELECT EXISTS(SELECT 1 FROM payments.refund_facts f WHERE f.tenant_id=r.tenant_id
   AND f.store_id=r.store_id AND f.refund_id=r.id AND f.kind IN ('FAILED','CANCELED')) AS hit) bad
  CROSS JOIN LATERAL (SELECT EXISTS(SELECT 1 FROM payments.refund_facts f WHERE f.tenant_id=r.tenant_id
   AND f.store_id=r.store_id AND f.refund_id=r.id AND f.kind='REJECTED') AS hit) rej
  WHERE r.tenant_id=a.tenant_id AND r.store_id=a.store_id AND r.attempt_id=a.id;
  IF EXISTS(SELECT 1 FROM payments.review_cases c WHERE c.tenant_id=a.tenant_id
   AND c.store_id=a.store_id AND c.attempt_id=a.id) THEN
   v_base:=jsonb_set(v_base,'{payment_state}','"REVIEW_REQUIRED"'::jsonb);
  ELSIF EXISTS(SELECT 1 FROM payments.facts f WHERE f.tenant_id=a.tenant_id
   AND f.store_id=a.store_id AND f.attempt_id=a.id AND f.kind='CLOSED_UNPAID') THEN
   v_base:=jsonb_set(v_base,'{payment_state}','"CLOSED_UNPAID"'::jsonb);
  ELSIF v_refunded>0 THEN
   -- RD7: derived, never stored. Precedence REVIEW_REQUIRED > REFUNDED > PARTIALLY_REFUNDED > CAPTURED.
   v_base:=jsonb_set(v_base,'{payment_state}',
    to_jsonb(CASE WHEN v_refunded>=a.amount_minor THEN 'REFUNDED' ELSE 'PARTIALLY_REFUNDED' END));
  END IF;
  IF v_refund_activity THEN
   v_base:=jsonb_set(v_base,'{refund}',jsonb_build_object('refunded_minor',v_refunded,'pending_minor',v_refund_pending));
  END IF;
  IF EXISTS(SELECT 1 FROM payments.facts f WHERE f.tenant_id=a.tenant_id
   AND f.store_id=a.store_id AND f.attempt_id=a.id AND f.kind IN ('CAPTURED','CLOSED_UNPAID')) THEN
   v_handoff:='CLOSED';
  ELSIF a.execution_profile<>p_profile OR p_stripe_digest IS NULL
   OR s.config_digest<>p_stripe_digest OR v_now>=s.handoff_cutoff THEN
   v_handoff:='UNAVAILABLE';
  ELSIF s.session_url IS NULL THEN v_handoff:='CREATING';
  ELSE v_handoff:='READY'; END IF;
  v_base:=jsonb_set(v_base,'{handoff_state}',to_jsonb(v_handoff));
  v_base:=jsonb_set(v_base,'{handoff_expires_at}',to_jsonb(s.handoff_cutoff));
  v_base:=jsonb_set(v_base,'{cancel_requested}',to_jsonb(s.cancel_requested_at IS NOT NULL));
 ELSE
  v_base:=jsonb_set(v_base,'{cancel_requested}','false'::jsonb);
 END IF;
 RETURN v_base;
END $$;
COMMENT ON FUNCTION checkout.hosted_payment_view_v2(bytea,uuid,uuid,text,bytea,bytea) IS 'checkout owner; hosted runtime projects PAYUNi and Stripe methods, frozen payment facts and buyer-safe refund totals; never authorizes capture, refund or stock release';

-- ------------------------------------------------------------------------------------------------
-- §7.1 / D4 merchant refund read. Same single-snapshot and fresh-final-auth rules as 0027's
-- identity.read_merchant_orders (which this migration does NOT replace, ruling 1).
-- ------------------------------------------------------------------------------------------------
CREATE FUNCTION identity.read_merchant_refunds(p_hash bytea,p_store uuid,p_order uuid)
RETURNS jsonb LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE s record; v_result jsonb; v_auth_error text; v_now timestamptz;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR p_order IS NULL
  OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid refund read' USING ERRCODE='PT400'; END IF;
 SELECT * INTO s FROM identity.resolve_access(p_hash,p_store,'orders:read');
 IF s.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF s.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF s.access_status IS DISTINCT FROM 'ok' THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 IF current_setting('app.tenant_id',true) IS DISTINCT FROM s.tenant_id::text
  OR current_setting('app.store_id',true) IS DISTINCT FROM p_store::text
  OR current_setting('app.principal_id',true) IS DISTINCT FROM s.principal_id::text THEN
  RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 v_now:=clock_timestamp();
 -- One statement, one snapshot: state, facts and totals cannot split. Item state (D4): FAILED/CANCELED
 -- beat SUCCEEDED beat REJECTED facts; else PENDING if pinned; else UNKNOWN if sent and past
 -- resend_until; else SUBMITTING if sent; else REQUESTED.
 WITH ord AS MATERIALIZED (
  SELECT o.tenant_id,o.store_id,o.owner_id,o.id,o.currency FROM checkout.orders o
   WHERE o.tenant_id=s.tenant_id AND o.store_id=p_store AND o.id=p_order
 ), att AS MATERIALIZED (
  SELECT a.tenant_id,a.store_id,a.id,a.currency FROM checkout.payment_attempts a
   JOIN ord ON ord.tenant_id=a.tenant_id AND ord.store_id=a.store_id AND ord.owner_id=a.owner_id
    AND ord.id=a.order_id
   WHERE EXISTS(SELECT 1 FROM payments.stripe_sessions ss WHERE ss.tenant_id=a.tenant_id
    AND ss.store_id=a.store_id AND ss.attempt_id=a.id)
 ), cap AS MATERIALIZED (
  SELECT att.id AS attempt_id,f.amount_minor FROM att
   JOIN payments.facts f ON f.tenant_id=att.tenant_id AND f.store_id=att.store_id AND f.attempt_id=att.id
    AND f.kind='CAPTURED' AND f.currency=att.currency
 ), rf AS MATERIALIZED (
  SELECT r.id,r.amount_minor,r.reason,r.requested_at,r.stripe_refund_id,fx.reason AS failure_reason,
   CASE WHEN fx.failed THEN 'FAILED' WHEN fx.canceled THEN 'CANCELED' WHEN fx.ok THEN 'SUCCEEDED'
    WHEN fx.rejected THEN 'REJECTED' WHEN r.stripe_refund_id IS NOT NULL THEN 'PENDING'
    WHEN r.first_sent_at IS NOT NULL AND v_now>=r.resend_until THEN 'UNKNOWN'
    WHEN r.first_sent_at IS NOT NULL THEN 'SUBMITTING' ELSE 'REQUESTED' END AS state,
   greatest(r.requested_at,r.first_sent_at,r.last_sent_at,r.pinned_at,fx.last_fact) AS updated_at
  FROM cap JOIN payments.stripe_refunds r ON r.tenant_id=s.tenant_id AND r.store_id=p_store
   AND r.attempt_id=cap.attempt_id
  LEFT JOIN LATERAL (
   SELECT coalesce(bool_or(f.kind='FAILED'),false) AS failed,coalesce(bool_or(f.kind='CANCELED'),false) AS canceled,
    coalesce(bool_or(f.kind='SUCCEEDED'),false) AS ok,coalesce(bool_or(f.kind='REJECTED'),false) AS rejected,
    max(f.received_at) AS last_fact,
    (array_agg(f.failure_reason ORDER BY f.received_at DESC) FILTER (WHERE f.kind IN ('FAILED','CANCELED','REJECTED')))[1] AS reason
   FROM payments.refund_facts f WHERE f.tenant_id=r.tenant_id AND f.store_id=r.store_id AND f.refund_id=r.id
  ) fx ON true
  ORDER BY r.requested_at DESC,r.id DESC LIMIT 20
 )
 SELECT jsonb_build_object(
   'captured_minor',coalesce((SELECT c.amount_minor FROM cap c),0),
   'refunded_minor',coalesce((SELECT sum(x.amount_minor) FROM rf x WHERE x.state='SUCCEEDED'),0),
   'pending_minor',coalesce((SELECT sum(x.amount_minor) FROM rf x
     WHERE x.state IN ('REQUESTED','SUBMITTING','PENDING','UNKNOWN')),0),
   'refundable_minor',coalesce((SELECT c.amount_minor FROM cap c),0)
     -coalesce((SELECT sum(x.amount_minor) FROM rf x WHERE x.state IN ('SUCCEEDED','REQUESTED','SUBMITTING','PENDING','UNKNOWN')),0),
   'currency',(SELECT o.currency FROM ord o),
   'items',coalesce((SELECT jsonb_agg(
     jsonb_build_object('refund_id',x.id,'amount_minor',x.amount_minor,'reason',x.reason,'state',x.state,
      'requested_at',to_char(x.requested_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
      'updated_at',to_char(x.updated_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
      'stripe_refund_id',x.stripe_refund_id)
     || CASE WHEN x.failure_reason IS NULL THEN '{}'::jsonb ELSE jsonb_build_object('failure_reason',x.failure_reason) END
     ORDER BY x.requested_at DESC,x.id DESC) FROM rf x),'[]'::jsonb))
 INTO v_result FROM (VALUES(1)) one WHERE EXISTS(SELECT 1 FROM ord);
 -- Fresh VOLATILE fence after every data wait, before the not-found branch (0027 pattern).
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
 IF v_auth_error IS NOT NULL THEN RAISE EXCEPTION 'refund read access denied' USING ERRCODE=v_auth_error; END IF;
 IF v_result IS NULL THEN RAISE EXCEPTION 'order not found' USING ERRCODE='PT404'; END IF;
 IF octet_length(v_result::text)>65536 THEN RAISE EXCEPTION 'refund read unavailable' USING ERRCODE='PT503'; END IF;
 RETURN v_result;
END $$;
ALTER FUNCTION identity.read_merchant_refunds(bytea,uuid,uuid) OWNER TO commerce_auth;
REVOKE ALL ON FUNCTION identity.read_merchant_refunds(bytea,uuid,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION identity.read_merchant_refunds(bytea,uuid,uuid) TO commerce_runtime;
COMMENT ON FUNCTION identity.read_merchant_refunds(bytea,uuid,uuid) IS 'identity owner (commerce_auth); merchant runtime reads one order refund list under orders:read with fresh final authorization; includes the merchant-only Stripe refund id (R-9); no write, no buyer access';
