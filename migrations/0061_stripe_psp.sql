-- Stripe shares the existing payment operation, observation, fact and stock paths.
-- §0.1 is authoritative over the older global-account and JPY draft text.

CREATE FUNCTION payments.stripe_amount_ok(p_currency text,p_minor bigint)
RETURNS boolean LANGUAGE sql IMMUTABLE SET search_path=pg_catalog AS $$
 SELECT coalesce(CASE p_currency
  WHEN 'HKD' THEN p_minor BETWEEN 400 AND 99999999
  WHEN 'USD' THEN p_minor BETWEEN 50 AND 99999999
  WHEN 'SGD' THEN p_minor BETWEEN 50 AND 99999999
  WHEN 'MYR' THEN p_minor BETWEEN 200 AND 99999999
  -- TWD min 2500: Stripe SANDBOX rejected 100/1200, accepted 2500 (2026-09-29)
  WHEN 'TWD' THEN p_minor BETWEEN 2500 AND 99999900 AND p_minor%100=0
  ELSE false END,false)
$$;
CREATE FUNCTION payments.stripe_unit_amount(p_currency text,p_minor bigint)
RETURNS bigint LANGUAGE sql IMMUTABLE SET search_path=pg_catalog AS $$
 SELECT CASE WHEN payments.stripe_amount_ok(p_currency,p_minor) THEN p_minor ELSE NULL END
$$;
REVOKE ALL ON FUNCTION payments.stripe_amount_ok(text,bigint),payments.stripe_unit_amount(text,bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION payments.stripe_amount_ok(text,bigint),payments.stripe_unit_amount(text,bigint)
 TO commerce_checkout_writer,commerce_integration_writer;
COMMENT ON FUNCTION payments.stripe_amount_ok(text,bigint) IS 'payments owner; checkout and integration writers validate the closed Stripe currency/minor-unit table; no conversion or provider guarantee';
COMMENT ON FUNCTION payments.stripe_unit_amount(text,bigint) IS 'payments owner; checkout and integration writers use exact ISO minor units; no rounding or repricing';

ALTER TABLE integration.merchant_accounts DROP CONSTRAINT merchant_accounts_provider_check;
ALTER TABLE integration.merchant_accounts ADD CONSTRAINT merchant_accounts_provider_check
 CHECK(provider IN ('payuni','stripe'));
ALTER TABLE integration.merchant_accounts ADD CONSTRAINT stripe_account_id_check
 CHECK(provider<>'stripe' OR account_id ~ '^acct_[A-Za-z0-9]{1,59}$');
CREATE UNIQUE INDEX stripe_one_account_per_store_environment ON integration.merchant_accounts
 (tenant_id,store_id,environment) WHERE provider='stripe';
CREATE UNIQUE INDEX stripe_account_identity_unique ON integration.merchant_accounts
 (environment,account_id) WHERE provider='stripe';
ALTER TABLE integration.merchant_accounts ADD CONSTRAINT account_stripe_endpoint_target_unique
 UNIQUE(tenant_id,store_id,id,provider,environment,account_id);
CREATE POLICY stripe_account_merchant_insert ON integration.merchant_accounts AS RESTRICTIVE
 FOR INSERT TO commerce_runtime WITH CHECK(provider='payuni');
CREATE POLICY stripe_account_merchant_update ON integration.merchant_accounts AS RESTRICTIVE
 FOR UPDATE TO commerce_runtime USING(provider='payuni') WITH CHECK(provider='payuni');

-- Stripe API material retains the already-audited, immutable DB_AEAD version shape.
-- The registrar is the only Stripe credential writer; ordinary merchant INSERT
-- remains limited by the original account/credential validation path.
CREATE POLICY stripe_credential_merchant_insert ON integration.account_credentials AS RESTRICTIVE
 FOR INSERT TO commerce_runtime WITH CHECK(EXISTS(
  SELECT 1 FROM integration.merchant_accounts a WHERE a.tenant_id=account_credentials.tenant_id
   AND a.store_id=account_credentials.store_id AND a.id=account_credentials.connection_id
   AND a.provider='payuni'));

ALTER TABLE payments.method_versions DROP CONSTRAINT method_versions_country_check;
ALTER TABLE payments.method_versions DROP CONSTRAINT method_versions_code_check;
ALTER TABLE payments.method_versions DROP CONSTRAINT method_versions_provider_check;
ALTER TABLE payments.method_versions DROP CONSTRAINT method_versions_currency_check;
ALTER TABLE payments.method_versions ADD CONSTRAINT method_stripe_target_check CHECK(
 (provider='payuni' AND code IN ('payuni_credit','payuni_installment','payuni_atm','payuni_cvs','payuni_linepay')
  AND country='TW' AND currency='TWD')
 OR (provider='stripe' AND code='stripe_checkout' AND country ~ '^[A-Z]{2}$'
  AND currency IN ('HKD','USD','SGD','MYR','TWD')));
-- Keep the merchant's existing PAYUNi draft path, but reserve Stripe admission
-- and head movement for the scoped registrar definer.
CREATE POLICY stripe_method_merchant_insert ON payments.method_versions AS RESTRICTIVE
 FOR INSERT TO commerce_runtime WITH CHECK(provider='payuni');
CREATE POLICY stripe_method_head_merchant_insert ON payments.method_heads AS RESTRICTIVE
 FOR INSERT TO commerce_runtime WITH CHECK(code<>'stripe_checkout');
CREATE POLICY stripe_method_head_merchant_update ON payments.method_heads AS RESTRICTIVE
 FOR UPDATE TO commerce_runtime USING(code<>'stripe_checkout') WITH CHECK(code<>'stripe_checkout');
ALTER TABLE payments.account_qualifications DROP CONSTRAINT account_qualifications_code_check;
ALTER TABLE payments.account_qualifications ADD CONSTRAINT account_qualifications_code_check
 CHECK(code IN ('payuni_credit','stripe_checkout'));
ALTER TABLE payments.account_qualifications ADD CONSTRAINT stripe_qualification_no_live_check
 CHECK(code<>'stripe_checkout' OR proof_class<>'REAL_LIVE');

ALTER TABLE checkout.payment_attempts DROP CONSTRAINT payment_attempts_method_code_check;
ALTER TABLE checkout.payment_attempts DROP CONSTRAINT payment_attempts_currency_check;
ALTER TABLE checkout.payment_attempts DROP CONSTRAINT payment_attempts_amount_minor_check;
ALTER TABLE checkout.payment_attempts ADD CONSTRAINT attempt_method_amount_check CHECK(
 (method_code='payuni_credit' AND currency='TWD' AND amount_minor BETWEEN 100 AND 19999900
  AND amount_minor%100=0)
 OR (method_code='stripe_checkout' AND payments.stripe_amount_ok(currency,amount_minor)));

ALTER TABLE integration.operations DROP CONSTRAINT operation_actor_family;
ALTER TABLE integration.operations ADD CONSTRAINT operation_actor_family CHECK (
 (actor_kind='MERCHANT' AND principal_id IS NOT NULL AND media_attempt_id IS NULL
  AND payment_attempt_id IS NULL AND buyer_owner_id IS NULL AND buyer_session_id IS NULL
  AND action NOT IN ('livekit.egress.start','livekit.egress.stop'))
 OR (actor_kind='BUYER_PAYMENT_QUERY' AND principal_id IS NULL AND media_attempt_id IS NULL
  AND payment_attempt_id=id AND payment_attempt_id IS NOT NULL
  AND buyer_owner_id IS NOT NULL AND buyer_session_id IS NOT NULL
  AND ((provider='payuni' AND action='payuni.query')
   OR (provider='stripe' AND action='stripe.checkout_session')) AND purpose='transactional'
  AND state NOT IN ('READY','DISPATCHING','BLOCKED_POLICY','STALE_BINDING') AND lease_mode<>'dispatch')
 OR (actor_kind='MEDIA_ATTEMPT' AND principal_id IS NOT NULL AND media_attempt_id IS NOT NULL
  AND payment_attempt_id IS NULL AND buyer_owner_id IS NULL AND buyer_session_id IS NULL
  AND provider='livekit' AND purpose='service' AND action='livekit.egress.start')
);
ALTER TABLE integration.operations DROP CONSTRAINT operations_provider_reference_check;
ALTER TABLE integration.operations ADD CONSTRAINT operations_provider_reference_check
 CHECK(char_length(provider_reference)<=255 AND provider_reference !~ '[[:cntrl:]]');

ALTER TABLE payments.provider_observations DROP CONSTRAINT provider_observations_source_check;
ALTER TABLE payments.provider_observations ADD CONSTRAINT provider_observations_source_check
 CHECK(source IN ('QUERY','LOCAL'));
ALTER TABLE payments.provider_observations ADD CONSTRAINT stripe_local_observation_check
 CHECK(source='QUERY' OR report->>'Provider'='stripe');

ALTER TABLE payments.facts DROP CONSTRAINT facts_kind_check;
ALTER TABLE payments.facts DROP CONSTRAINT facts_amount_minor_check;
ALTER TABLE payments.facts DROP CONSTRAINT facts_currency_check;
ALTER TABLE payments.facts DROP CONSTRAINT facts_provider_reference_check;
ALTER TABLE payments.facts ADD CONSTRAINT facts_kind_check
 CHECK(kind IN ('AUTHORIZED','CAPTURED','CLOSED_UNPAID'));
ALTER TABLE payments.facts ADD CONSTRAINT facts_amount_minor_check CHECK(
 (kind='CLOSED_UNPAID' AND amount_minor=0)
 OR (kind<>'CLOSED_UNPAID' AND amount_minor BETWEEN 1 AND 999999999999));
ALTER TABLE payments.facts ADD CONSTRAINT facts_currency_check CHECK(currency ~ '^[A-Z]{3}$');
ALTER TABLE payments.facts ADD CONSTRAINT facts_provider_reference_check
 CHECK(provider_reference ~ '^[A-Za-z0-9_-]{1,255}$');
CREATE FUNCTION payments.guard_closed_unpaid_fact() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 IF NEW.kind='CLOSED_UNPAID' AND EXISTS(
  SELECT 1 FROM payments.facts f WHERE f.tenant_id=NEW.tenant_id AND f.store_id=NEW.store_id
   AND f.attempt_id=NEW.attempt_id AND f.kind IN ('AUTHORIZED','CAPTURED')) THEN
  RAISE EXCEPTION 'financial fact already exists' USING ERRCODE='PT409';
 END IF;
 RETURN NEW;
END $$;
ALTER FUNCTION payments.guard_closed_unpaid_fact() OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION payments.guard_closed_unpaid_fact() FROM PUBLIC;
CREATE TRIGGER guard_closed_unpaid_fact BEFORE INSERT ON payments.facts
 FOR EACH ROW EXECUTE FUNCTION payments.guard_closed_unpaid_fact();
COMMENT ON FUNCTION payments.guard_closed_unpaid_fact() IS 'payments owner; checkout writer prevents closing a captured or authorized attempt; no external PSP calls';

ALTER TABLE payments.review_cases DROP CONSTRAINT review_cases_reason_check;
ALTER TABLE payments.review_cases ADD CONSTRAINT review_cases_reason_check CHECK(reason IN (
 'CAPTURE_EVIDENCE_INCOMPLETE','REFUND_HISTORY','CONFLICTING_REPORT','PAID_ALLOCATION_FAILED',
 'PROVIDER_AMOUNT_MISMATCH','PROVIDER_PRESENTMENT_DRIFT','PROVIDER_SESSION_DUPLICATE',
 'PROVIDER_IDENTITY_MISMATCH','PROVIDER_EXPIRY_UNCONFIRMED','PROVIDER_ASYNC_PENDING',
 'CLOSURE_CONTRADICTED'));

ALTER TABLE inventory.ledger DROP CONSTRAINT ledger_checkout_actor;
ALTER TABLE inventory.ledger ADD CONSTRAINT ledger_checkout_actor CHECK(
 (actor_kind='MERCHANT' AND principal_id IS NOT NULL AND checkout_id IS NULL
  AND buyer_owner_id IS NULL AND buyer_session_id IS NULL AND payment_attempt_id IS NULL AND payment_fact_kind IS NULL)
 OR (actor_kind='BUYER' AND principal_id IS NULL AND checkout_id IS NOT NULL
  AND buyer_owner_id IS NOT NULL AND buyer_session_id IS NOT NULL
  AND reservation_id=checkout_id AND kind='RESERVE' AND payment_attempt_id IS NULL AND payment_fact_kind IS NULL)
 OR (actor_kind='SYSTEM_EXPIRY' AND principal_id IS NULL AND checkout_id IS NOT NULL
  AND buyer_owner_id IS NOT NULL AND buyer_session_id IS NOT NULL
  AND reservation_id=checkout_id AND kind='RELEASE' AND payment_attempt_id IS NULL AND payment_fact_kind IS NULL)
 OR (actor_kind='SYSTEM_PAYMENT' AND principal_id IS NULL AND checkout_id IS NOT NULL
  AND buyer_owner_id IS NOT NULL AND buyer_session_id IS NOT NULL AND reservation_id=checkout_id
  AND payment_attempt_id IS NOT NULL AND
  ((kind='ALLOCATE' AND payment_fact_kind='CAPTURED' AND operation='checkout.payment.capture')
   OR (kind='RELEASE' AND payment_fact_kind='CLOSED_UNPAID' AND operation='checkout.payment.close'))
  AND command_key=payment_attempt_id::text));
CREATE OR REPLACE FUNCTION inventory.guard_payment_ledger() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 IF NEW.actor_kind='SYSTEM_PAYMENT' AND NOT EXISTS(
  SELECT 1 FROM payments.facts f JOIN checkout.payment_attempts a
   ON a.tenant_id=f.tenant_id AND a.store_id=f.store_id AND a.id=f.attempt_id
   JOIN checkout.orders o ON o.tenant_id=a.tenant_id AND o.store_id=a.store_id
    AND o.owner_id=a.owner_id AND o.id=a.order_id
   WHERE f.tenant_id=NEW.tenant_id AND f.store_id=NEW.store_id
    AND f.attempt_id=NEW.payment_attempt_id AND f.kind=NEW.payment_fact_kind
    AND a.order_id=NEW.checkout_id AND o.creator_session_id=NEW.buyer_session_id
    AND a.owner_id=NEW.buyer_owner_id AND f.currency=a.currency
    AND ((NEW.kind='ALLOCATE' AND f.kind='CAPTURED' AND f.amount_minor=a.amount_minor)
     OR (NEW.kind='RELEASE' AND f.kind='CLOSED_UNPAID' AND f.amount_minor=0
      AND NOT EXISTS(SELECT 1 FROM payments.facts paid WHERE paid.tenant_id=f.tenant_id
       AND paid.store_id=f.store_id AND paid.attempt_id=f.attempt_id AND paid.kind='CAPTURED')))) THEN
  RAISE EXCEPTION 'payment ledger mismatch' USING ERRCODE='42501'; END IF;
 IF NEW.actor_kind='SYSTEM_PAYMENT' AND NOT EXISTS(
  SELECT 1 FROM inventory.reservation_lines l WHERE l.tenant_id=NEW.tenant_id
   AND l.store_id=NEW.store_id AND l.reservation_id=NEW.reservation_id
   AND l.warehouse_id=NEW.warehouse_id AND l.sku_id=NEW.sku_id
   AND l.quantity=-NEW.delta_reserved) THEN
  RAISE EXCEPTION 'payment ledger quantity mismatch' USING ERRCODE='42501'; END IF;
 RETURN NEW;
END $$;
COMMENT ON FUNCTION inventory.guard_payment_ledger() IS 'inventory owner; checkout writer ties payment allocation or closure release to an immutable payment fact; no direct worker stock write';

ALTER TABLE checkout.events DROP CONSTRAINT events_action_check;
ALTER TABLE checkout.events DROP CONSTRAINT events_actor_action;
ALTER TABLE checkout.events ADD CONSTRAINT events_action_check CHECK(action IN
 ('checkout.held','checkout.expired','checkout.payment_started','checkout.payment_captured','checkout.payment_closed'));
ALTER TABLE checkout.events ADD CONSTRAINT events_actor_action CHECK(
 (action IN ('checkout.held','checkout.payment_started') AND actor_kind='BUYER')
 OR (action='checkout.expired' AND actor_kind='SYSTEM_EXPIRY')
 OR (action IN ('checkout.payment_captured','checkout.payment_closed') AND actor_kind='SYSTEM_PAYMENT'));

CREATE ROLE commerce_stripe_ingress NOLOGIN INHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
CREATE ROLE commerce_payment_registrar NOLOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
CREATE ROLE commerce_payment_registry_writer NOLOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
COMMENT ON ROLE commerce_stripe_ingress IS 'payments webhook ingress; executes signature-fenced receipt functions and River InsertTx only, no merchant or payment table access';
COMMENT ON ROLE commerce_payment_registrar IS 'payments operator CLI; only executes registry definers; no merchant self-service or LIVE qualification';
COMMENT ON ROLE commerce_payment_registry_writer IS 'payments non-login registrar definer; writes account, method and audit rows, never queries raw PSP traffic';
GRANT USAGE ON SCHEMA integration,payments,identity,control,pricing,ops
 TO commerce_payment_registry_writer;
GRANT USAGE ON SCHEMA integration,payments TO commerce_payment_registrar;
GRANT SELECT ON control.stores,identity.memberships,identity.store_grants,pricing.markets
 TO commerce_payment_registry_writer;
GRANT UPDATE(id) ON pricing.markets,integration.bindings,payments.account_qualifications
 TO commerce_payment_registry_writer;
CREATE POLICY stripe_registry_store_read ON control.stores FOR SELECT TO commerce_payment_registry_writer
 USING(true);
CREATE POLICY stripe_registry_market_read ON pricing.markets FOR SELECT TO commerce_payment_registry_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
CREATE POLICY stripe_registry_market_lock ON pricing.markets FOR UPDATE TO commerce_payment_registry_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid) WITH CHECK(false);
GRANT SELECT,INSERT ON integration.bindings,integration.merchant_accounts,
 payments.account_qualifications,payments.method_versions,payments.method_heads
 TO commerce_payment_registry_writer;
GRANT INSERT ON integration.account_credentials,ops.audit_events TO commerce_payment_registry_writer;
-- S4: payments.stripe_registrar_credential is the only reader; SELECT stays scope-pinned by RLS below.
GRANT SELECT ON integration.account_credentials TO commerce_payment_registry_writer;
GRANT UPDATE(credential_version,updated_at) ON integration.merchant_accounts
 TO commerce_payment_registry_writer;
GRANT UPDATE(current_version) ON payments.method_heads TO commerce_payment_registry_writer;
CREATE POLICY stripe_registry_binding_read ON integration.bindings FOR SELECT TO commerce_payment_registry_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
CREATE POLICY stripe_registry_binding_insert ON integration.bindings FOR INSERT TO commerce_payment_registry_writer
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND principal_id=nullif(current_setting('app.principal_id',true),'')::uuid);
CREATE POLICY stripe_registry_binding_lock ON integration.bindings FOR UPDATE TO commerce_payment_registry_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid) WITH CHECK(false);
CREATE POLICY stripe_registry_account_read ON integration.merchant_accounts FOR SELECT
 TO commerce_payment_registry_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
CREATE POLICY stripe_registry_account_insert ON integration.merchant_accounts FOR INSERT
 TO commerce_payment_registry_writer
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND principal_id=nullif(current_setting('app.principal_id',true),'')::uuid);
CREATE POLICY stripe_registry_account_update ON integration.merchant_accounts FOR UPDATE
 TO commerce_payment_registry_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
CREATE POLICY stripe_registry_credential ON integration.account_credentials FOR INSERT
 TO commerce_payment_registry_writer
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND principal_id=nullif(current_setting('app.principal_id',true),'')::uuid);
CREATE POLICY stripe_registry_credential_read ON integration.account_credentials FOR SELECT
 TO commerce_payment_registry_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
CREATE POLICY stripe_registry_qualification_read ON payments.account_qualifications
 FOR SELECT TO commerce_payment_registry_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
CREATE POLICY stripe_registry_qualification_insert ON payments.account_qualifications
 FOR INSERT TO commerce_payment_registry_writer
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
CREATE POLICY stripe_registry_qualification_lock ON payments.account_qualifications
 FOR UPDATE TO commerce_payment_registry_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid) WITH CHECK(false);
CREATE POLICY stripe_registry_method ON payments.method_versions TO commerce_payment_registry_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND principal_id=nullif(current_setting('app.principal_id',true),'')::uuid);
CREATE POLICY stripe_registry_method_head ON payments.method_heads TO commerce_payment_registry_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
CREATE POLICY stripe_registry_audit ON ops.audit_events FOR INSERT TO commerce_payment_registry_writer
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND principal_id=nullif(current_setting('app.principal_id',true),'')::uuid);

CREATE TABLE payments.stripe_sessions (
 tenant_id uuid NOT NULL,store_id uuid NOT NULL,owner_id uuid NOT NULL,attempt_id uuid PRIMARY KEY,
 environment text NOT NULL CHECK(environment IN ('SANDBOX','LIVE')),
 account_id text NOT NULL CHECK(account_id ~ '^acct_[A-Za-z0-9]{1,59}$'),
 locale text NOT NULL CHECK(locale IN ('zh-CN','zh-TW','en')),
 config_digest bytea NOT NULL CHECK(octet_length(config_digest)=32),
 unit_amount bigint NOT NULL CHECK(unit_amount BETWEEN 1 AND 99999999),
 create_params jsonb NOT NULL CHECK(jsonb_typeof(create_params)='object' AND octet_length(create_params::text)<=4096),
 attempt_created_at timestamptz NOT NULL,expires_at timestamptz NOT NULL,
 send_deadline timestamptz NOT NULL,handoff_cutoff timestamptz NOT NULL,
 create_first_sent_at timestamptz,create_last_sent_at timestamptz,
 create_send_count integer NOT NULL DEFAULT 0 CHECK(create_send_count BETWEEN 0 AND 200),
 create_body_sha256 bytea CHECK(create_body_sha256 IS NULL OR octet_length(create_body_sha256)=32),
 create_suppressed_at timestamptz,
 session_id text UNIQUE CHECK(session_id ~ '^[A-Za-z0-9_]{1,255}$'),
 session_url text CHECK(octet_length(session_url)<=4096 AND session_url ~ '^https://checkout\.stripe\.com/[!-~]+$'),
 payment_intent_id text CHECK(payment_intent_id ~ '^[A-Za-z0-9_]{1,255}$'),
 pinned_at timestamptz,url_purged_at timestamptz,first_handed_out_at timestamptz,
 cancel_requested_at timestamptz,
 refresh_count integer NOT NULL DEFAULT 0 CHECK(refresh_count BETWEEN 0 AND 30),last_refresh_at timestamptz,
 signal_count integer NOT NULL DEFAULT 0 CHECK(signal_count BETWEEN 0 AND 64),
 expire_calls integer NOT NULL DEFAULT 0 CHECK(expire_calls BETWEEN 0 AND 500),last_expire_at timestamptz,
 CHECK(expires_at=date_trunc('second',attempt_created_at)+interval '40 minutes'),
 CHECK(send_deadline=attempt_created_at+interval '7 minutes'),
 CHECK(handoff_cutoff=expires_at-interval '5 minutes'),
 CHECK((session_id IS NULL)=(pinned_at IS NULL)),
 CHECK(session_url IS NULL OR session_id IS NOT NULL),
 CHECK(url_purged_at IS NULL OR session_url IS NULL),
 CHECK((create_first_sent_at IS NULL)=(create_send_count=0)
   AND (create_first_sent_at IS NULL)=(create_body_sha256 IS NULL)
   AND (create_first_sent_at IS NULL)=(create_last_sent_at IS NULL)),
 CHECK(create_suppressed_at IS NULL OR create_first_sent_at IS NULL),
 CHECK(create_first_sent_at IS NULL OR create_first_sent_at<send_deadline),
 FOREIGN KEY(tenant_id,store_id,owner_id,attempt_id)
  REFERENCES checkout.payment_attempts(tenant_id,store_id,owner_id,id),
 FOREIGN KEY(tenant_id,store_id,attempt_id) REFERENCES integration.operations(tenant_id,store_id,id)
);
CREATE TABLE payments.stripe_webhook_receipts (
 id uuid PRIMARY KEY,endpoint_id uuid NOT NULL,
 environment text NOT NULL CHECK(environment IN ('SANDBOX','LIVE')),
 account_id text NOT NULL CHECK(account_id ~ '^acct_[A-Za-z0-9]{1,59}$'),
 event_id text CHECK(event_id ~ '^[A-Za-z0-9_]{1,255}$'),
 event_type text CHECK(event_type ~ '^[a-z0-9_.]{1,100}$'),event_created bigint,
 api_version text CHECK(api_version ~ '^[A-Za-z0-9._-]{0,64}$'),
 object_type text CHECK(object_type ~ '^[a-z0-9_.]{0,64}$'),
 session_id text CHECK(session_id ~ '^[A-Za-z0-9_]{1,255}$'),
 body_sha256 bytea NOT NULL CHECK(octet_length(body_sha256)=32),signed_at bigint NOT NULL CHECK(signed_at>0),
 disposition text NOT NULL CHECK(disposition IN ('ACCEPTED','IGNORED','QUARANTINED','MALFORMED')),
 reason text NOT NULL CHECK(reason IN ('accepted','unsubscribed_type','probe_session','connect_event',
  'livemode_mismatch','account_unregistered','object_mismatch','unknown_session','reference_mismatch',
  'profile_mismatch','signal_cap','malformed_json')),
 attempt_id uuid,tenant_id uuid,store_id uuid,signal_id uuid UNIQUE,
 redelivery_count integer NOT NULL DEFAULT 0 CHECK(redelivery_count BETWEEN 0 AND 100000),
 received_at timestamptz NOT NULL DEFAULT clock_timestamp(),last_redelivered_at timestamptz,
 CHECK((disposition='MALFORMED')=(event_id IS NULL)),
 CHECK((disposition='ACCEPTED')=(signal_id IS NOT NULL AND attempt_id IS NOT NULL)),
 CHECK((attempt_id IS NULL)=(tenant_id IS NULL) AND (attempt_id IS NULL)=(store_id IS NULL)),
 UNIQUE(endpoint_id,event_id)
);
CREATE UNIQUE INDEX stripe_malformed_body ON payments.stripe_webhook_receipts
 (endpoint_id,body_sha256) WHERE event_id IS NULL;
CREATE TABLE payments.stripe_webhook_endpoints (
 endpoint_id uuid PRIMARY KEY,tenant_id uuid NOT NULL,store_id uuid NOT NULL,connection_id uuid NOT NULL,
 provider text GENERATED ALWAYS AS ('stripe'::text) STORED,
 environment text NOT NULL CHECK(environment='SANDBOX'),
 account_id text NOT NULL CHECK(account_id ~ '^acct_[A-Za-z0-9]{1,59}$'),
 execution_profile text NOT NULL CHECK(execution_profile IN ('PROVIDER_MOCK','SANDBOX')),
 enabled boolean NOT NULL,key_version bigint NOT NULL CHECK(key_version>0),
 key_id text NOT NULL CHECK(key_id ~ '^[A-Za-z0-9_-]{1,40}$'),
 nonce bytea NOT NULL CHECK(octet_length(nonce)=12),
 ciphertext bytea NOT NULL CHECK(octet_length(ciphertext) BETWEEN 17 AND 8192),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(tenant_id,store_id,connection_id,execution_profile),
 FOREIGN KEY(tenant_id,store_id,connection_id,provider,environment,account_id)
  REFERENCES integration.merchant_accounts(tenant_id,store_id,id,provider,environment,account_id)
);
ALTER TABLE payments.stripe_webhook_receipts ADD CONSTRAINT stripe_receipt_endpoint_fk
 FOREIGN KEY(endpoint_id) REFERENCES payments.stripe_webhook_endpoints(endpoint_id);
CREATE TABLE payments.stripe_signals (
 id uuid PRIMARY KEY,tenant_id uuid NOT NULL,store_id uuid NOT NULL,attempt_id uuid NOT NULL,
 source text NOT NULL CHECK(source IN ('STRIPE_WEBHOOK','BUYER_REFRESH','BUYER_CANCEL')),
 receipt_id uuid UNIQUE REFERENCES payments.stripe_webhook_receipts(id),
 session_id text CHECK(session_id ~ '^[A-Za-z0-9_]{1,255}$'),
 job_id bigint NOT NULL UNIQUE CHECK(job_id>0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),consumed_at timestamptz,
 outcome text CHECK(outcome IN ('OBSERVED','EXPIRE_REQUESTED','NOOP_TERMINAL','STALE_DROPPED')),
 CHECK((source='STRIPE_WEBHOOK')=(receipt_id IS NOT NULL)),
 CHECK((consumed_at IS NULL)=(outcome IS NULL)),
 FOREIGN KEY(tenant_id,store_id,attempt_id) REFERENCES integration.operations(tenant_id,store_id,id)
);

DO $$ DECLARE v_table text; v_col record; BEGIN
 FOREACH v_table IN ARRAY ARRAY['stripe_sessions','stripe_webhook_receipts',
  'stripe_webhook_endpoints','stripe_signals'] LOOP
  EXECUTE format('ALTER TABLE payments.%I ENABLE ROW LEVEL SECURITY',v_table);
  EXECUTE format('ALTER TABLE payments.%I FORCE ROW LEVEL SECURITY',v_table);
  EXECUTE format('REVOKE ALL ON payments.%I FROM PUBLIC',v_table);
  EXECUTE format('COMMENT ON TABLE payments.%I IS %L',v_table,
   'payments owner; private Stripe metadata for checkout and integration definers; never stores raw webhook body or key');
  FOR v_col IN SELECT column_name FROM information_schema.columns
   WHERE table_schema='payments' AND table_name=v_table LOOP
   EXECUTE format('COMMENT ON COLUMN payments.%I.%I IS %L',v_table,v_col.column_name,
    'payments owner; checkout/integration writer bounded metadata; no direct runtime access or PSP authority');
  END LOOP;
 END LOOP;
END $$;

CREATE POLICY stripe_session_checkout ON payments.stripe_sessions TO commerce_checkout_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND owner_id=nullif(current_setting('app.buyer_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND owner_id=nullif(current_setting('app.buyer_id',true),'')::uuid);
CREATE POLICY stripe_session_integration ON payments.stripe_sessions TO commerce_integration_writer
 USING(true) WITH CHECK(true);
CREATE POLICY stripe_receipt_integration ON payments.stripe_webhook_receipts TO commerce_integration_writer
 USING(true) WITH CHECK(true);
CREATE POLICY stripe_endpoint_integration ON payments.stripe_webhook_endpoints
 FOR SELECT TO commerce_integration_writer USING(true);
CREATE POLICY stripe_endpoint_ingress_lock ON payments.stripe_webhook_endpoints
 FOR UPDATE TO commerce_integration_writer USING(true) WITH CHECK(false);
CREATE POLICY stripe_signal_checkout ON payments.stripe_signals TO commerce_checkout_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND EXISTS(SELECT 1 FROM checkout.payment_attempts a WHERE a.id=attempt_id
   AND a.tenant_id=stripe_signals.tenant_id AND a.store_id=stripe_signals.store_id
   AND a.owner_id=nullif(current_setting('app.buyer_id',true),'')::uuid))
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND EXISTS(SELECT 1 FROM checkout.payment_attempts a WHERE a.id=attempt_id
   AND a.tenant_id=stripe_signals.tenant_id AND a.store_id=stripe_signals.store_id
   AND a.owner_id=nullif(current_setting('app.buyer_id',true),'')::uuid));
CREATE POLICY stripe_signal_integration ON payments.stripe_signals TO commerce_integration_writer
 USING(true) WITH CHECK(true);

GRANT SELECT,INSERT ON payments.stripe_sessions TO commerce_checkout_writer;
GRANT UPDATE(first_handed_out_at,cancel_requested_at,refresh_count,last_refresh_at,signal_count)
 ON payments.stripe_sessions TO commerce_checkout_writer;
GRANT SELECT,INSERT ON payments.stripe_signals TO commerce_checkout_writer;
GRANT SELECT ON payments.stripe_sessions TO commerce_integration_writer;
GRANT UPDATE(create_first_sent_at,create_last_sent_at,create_send_count,create_body_sha256,
 create_suppressed_at,session_id,session_url,payment_intent_id,pinned_at,url_purged_at,
 expire_calls,last_expire_at,signal_count)
 ON payments.stripe_sessions TO commerce_integration_writer;
GRANT SELECT,INSERT ON payments.stripe_webhook_receipts TO commerce_integration_writer;
GRANT UPDATE(redelivery_count,last_redelivered_at) ON payments.stripe_webhook_receipts TO commerce_integration_writer;
GRANT SELECT ON payments.stripe_webhook_endpoints TO commerce_integration_writer;
GRANT UPDATE(endpoint_id) ON payments.stripe_webhook_endpoints TO commerce_integration_writer;
GRANT SELECT,INSERT ON payments.stripe_signals TO commerce_integration_writer;
GRANT UPDATE(consumed_at,outcome) ON payments.stripe_signals TO commerce_integration_writer;
GRANT USAGE ON SCHEMA payments TO commerce_stripe_ingress;

CREATE FUNCTION payments.stripe_webhook_material(p_endpoint uuid)
RETURNS TABLE(tenant_id uuid,store_id uuid,connection_id uuid,environment text,
 account_id text,execution_profile text,key_version bigint,key_id text,nonce bytea,ciphertext bytea)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT e.tenant_id,e.store_id,e.connection_id,e.environment,e.account_id,e.execution_profile,
  e.key_version,e.key_id,e.nonce,e.ciphertext
 FROM payments.stripe_webhook_endpoints e
 JOIN integration.merchant_accounts a ON a.tenant_id=e.tenant_id AND a.store_id=e.store_id
  AND a.id=e.connection_id AND a.environment=e.environment AND a.account_id=e.account_id
 JOIN integration.bindings b ON b.tenant_id=a.tenant_id AND b.store_id=a.store_id
  AND b.id=a.binding_id AND b.provider='stripe' AND b.external_asset_id=a.binding_asset
 WHERE e.endpoint_id=p_endpoint AND e.enabled AND a.provider='stripe'
$$;
ALTER FUNCTION payments.stripe_webhook_material(uuid) OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION payments.stripe_webhook_material(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION payments.stripe_webhook_material(uuid) TO commerce_stripe_ingress;
COMMENT ON FUNCTION payments.stripe_webhook_material(uuid) IS 'payments owner; ingress reads one enabled endpoint signing key envelope only; no Stripe API credential or tenant authority from event';

CREATE FUNCTION payments.stripe_webhook_prepare(p_endpoint uuid,p_key_version bigint,
 p_event_id text,p_event_type text,p_event_created bigint,p_api_version text,p_object_type text,
 p_session_id text,p_client_reference text,p_metadata_attempt text,p_account_present boolean,
 p_livemode boolean,p_probe boolean,p_malformed boolean,p_body_sha256 bytea,p_signed_at bigint)
RETURNS TABLE(disposition text,receipt_id uuid,attempt_id uuid,session_id text,signal_id uuid)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE ep payments.stripe_webhook_endpoints%ROWTYPE; a checkout.payment_attempts%ROWTYPE;
 sess payments.stripe_sessions%ROWTYPE; existing payments.stripe_webhook_receipts%ROWTYPE;
 v_receipt uuid; v_signal uuid; v_disposition text; v_reason text; v_attempt uuid;
 v_tenant uuid; v_store uuid; v_session text;
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
   RETURN QUERY SELECT 'DUPLICATE'::text,existing.id,existing.attempt_id,existing.session_id,existing.signal_id;
   RETURN;
  END IF;
  v_receipt:=gen_random_uuid();
  INSERT INTO payments.stripe_webhook_receipts(id,endpoint_id,environment,account_id,event_id,body_sha256,
   signed_at,disposition,reason)
   VALUES(v_receipt,ep.endpoint_id,ep.environment,ep.account_id,NULL,p_body_sha256,p_signed_at,'MALFORMED','malformed_json');
  RETURN QUERY SELECT 'MALFORMED'::text,v_receipt,NULL::uuid,NULL::text,NULL::uuid;
  RETURN;
 END IF;
 IF p_event_id IS NULL OR p_event_id !~ '^[A-Za-z0-9_]{1,255}$'
  OR p_event_type IS NULL OR p_event_type !~ '^[a-z0-9_.]{1,100}$'
  OR p_session_id IS NULL OR (p_session_id<>'' AND p_session_id !~ '^[A-Za-z0-9_]{1,255}$')
  OR p_event_created IS NULL OR p_event_created<=0 THEN
  RAISE EXCEPTION 'invalid Stripe webhook projection' USING ERRCODE='22023'; END IF;
 PERFORM pg_advisory_xact_lock(hashtextextended('stripe.webhook.event|'
  ||ep.endpoint_id||'|'||p_event_id,0));
 SELECT x.* INTO existing FROM payments.stripe_webhook_receipts x
  WHERE x.endpoint_id=ep.endpoint_id AND x.event_id=p_event_id FOR UPDATE;
 IF FOUND THEN
  UPDATE payments.stripe_webhook_receipts SET redelivery_count=redelivery_count+1,
   last_redelivered_at=clock_timestamp() WHERE id=existing.id;
  RETURN QUERY SELECT 'DUPLICATE'::text,existing.id,existing.attempt_id,existing.session_id,existing.signal_id;
  RETURN;
 END IF;
 v_disposition:='QUARANTINED'; v_reason:='unknown_session';
 IF p_account_present THEN v_reason:='connect_event';
 ELSIF p_livemode<>(ep.environment='LIVE') THEN v_reason:='livemode_mismatch';
 ELSIF p_event_type NOT IN ('checkout.session.completed','checkout.session.async_payment_succeeded',
  'checkout.session.async_payment_failed','checkout.session.expired') THEN
  v_disposition:='IGNORED'; v_reason:='unsubscribed_type';
 ELSIF p_probe THEN v_disposition:='IGNORED'; v_reason:='probe_session';
 ELSIF p_object_type<>'checkout.session' THEN v_reason:='object_mismatch';
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
  attempt_id,tenant_id,store_id,signal_id)
 VALUES(v_receipt,ep.endpoint_id,ep.environment,ep.account_id,p_event_id,p_event_type,p_event_created,
  p_api_version,p_object_type,nullif(p_session_id,''),p_body_sha256,p_signed_at,
  v_disposition,v_reason,v_attempt,v_tenant,v_store,v_signal);
 RETURN QUERY SELECT CASE WHEN v_disposition='ACCEPTED' THEN 'ACCEPT_PENDING' ELSE v_disposition END,
  v_receipt,v_attempt,v_session,v_signal;
END $$;
ALTER FUNCTION payments.stripe_webhook_prepare(uuid,bigint,text,text,bigint,text,text,text,text,text,boolean,boolean,boolean,boolean,bytea,bigint)
 OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION payments.stripe_webhook_prepare(uuid,bigint,text,text,bigint,text,text,text,text,text,boolean,boolean,boolean,boolean,bytea,bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION payments.stripe_webhook_prepare(uuid,bigint,text,text,bigint,text,text,text,text,text,boolean,boolean,boolean,boolean,bytea,bigint)
 TO commerce_stripe_ingress;
COMMENT ON FUNCTION payments.stripe_webhook_prepare(uuid,bigint,text,text,bigint,text,text,text,text,text,boolean,boolean,boolean,boolean,bytea,bigint) IS 'payments owner; signed ingress metadata dedupe and scoped signal preallocation; no raw body or financial claim';

CREATE FUNCTION payments.guard_stripe_session() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 IF (OLD.tenant_id,OLD.store_id,OLD.owner_id,OLD.attempt_id,OLD.environment,OLD.account_id,
  OLD.locale,OLD.config_digest,OLD.unit_amount,OLD.create_params,OLD.attempt_created_at,
  OLD.expires_at,OLD.send_deadline,OLD.handoff_cutoff)
  IS DISTINCT FROM
  (NEW.tenant_id,NEW.store_id,NEW.owner_id,NEW.attempt_id,NEW.environment,NEW.account_id,
  NEW.locale,NEW.config_digest,NEW.unit_amount,NEW.create_params,NEW.attempt_created_at,
  NEW.expires_at,NEW.send_deadline,NEW.handoff_cutoff)
  OR (OLD.session_id IS NOT NULL AND NEW.session_id IS DISTINCT FROM OLD.session_id)
  OR (OLD.payment_intent_id IS NOT NULL AND NEW.payment_intent_id IS DISTINCT FROM OLD.payment_intent_id)
  OR (OLD.create_first_sent_at IS NOT NULL AND NEW.create_first_sent_at IS DISTINCT FROM OLD.create_first_sent_at)
  OR (OLD.create_body_sha256 IS NOT NULL AND NEW.create_body_sha256 IS DISTINCT FROM OLD.create_body_sha256)
  OR (OLD.create_suppressed_at IS NOT NULL AND NEW.create_suppressed_at IS DISTINCT FROM OLD.create_suppressed_at)
  OR (OLD.pinned_at IS NOT NULL AND NEW.pinned_at IS DISTINCT FROM OLD.pinned_at)
  OR (OLD.url_purged_at IS NOT NULL AND NEW.url_purged_at IS DISTINCT FROM OLD.url_purged_at)
  OR (OLD.first_handed_out_at IS NOT NULL AND NEW.first_handed_out_at IS DISTINCT FROM OLD.first_handed_out_at)
  OR (OLD.cancel_requested_at IS NOT NULL AND NEW.cancel_requested_at IS DISTINCT FROM OLD.cancel_requested_at)
  OR (OLD.session_url IS NOT NULL AND NEW.session_url IS DISTINCT FROM OLD.session_url
   AND NOT (NEW.session_url IS NULL AND NEW.url_purged_at IS NOT NULL))
  OR (OLD.url_purged_at IS NOT NULL AND NEW.session_url IS NOT NULL)
  OR (OLD.create_last_sent_at IS NOT NULL AND
   (NEW.create_last_sent_at IS NULL OR NEW.create_last_sent_at<OLD.create_last_sent_at))
  OR (OLD.last_refresh_at IS NOT NULL AND
   (NEW.last_refresh_at IS NULL OR NEW.last_refresh_at<OLD.last_refresh_at))
  OR (OLD.last_expire_at IS NOT NULL AND
   (NEW.last_expire_at IS NULL OR NEW.last_expire_at<OLD.last_expire_at))
  OR NEW.create_send_count<OLD.create_send_count OR NEW.refresh_count<OLD.refresh_count
  OR NEW.signal_count<OLD.signal_count OR NEW.expire_calls<OLD.expire_calls THEN
  RAISE EXCEPTION 'Stripe session immutable field changed' USING ERRCODE='42501';
 END IF;
 RETURN NEW;
END $$;
ALTER FUNCTION payments.guard_stripe_session() OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION payments.guard_stripe_session() FROM PUBLIC;
CREATE TRIGGER guard_stripe_session BEFORE UPDATE ON payments.stripe_sessions
 FOR EACH ROW EXECUTE FUNCTION payments.guard_stripe_session();
COMMENT ON FUNCTION payments.guard_stripe_session() IS 'payments owner; checkout/integration writers enforce set-once identity and monotone counters; no PSP network authority';

CREATE FUNCTION payments.guard_stripe_receipt() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 IF (to_jsonb(NEW)-'redelivery_count'-'last_redelivered_at') IS DISTINCT FROM
  (to_jsonb(OLD)-'redelivery_count'-'last_redelivered_at')
  OR NEW.redelivery_count<OLD.redelivery_count THEN
  RAISE EXCEPTION 'Stripe receipt immutable field changed' USING ERRCODE='42501';
 END IF;
 RETURN NEW;
END $$;
ALTER FUNCTION payments.guard_stripe_receipt() OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION payments.guard_stripe_receipt() FROM PUBLIC;
CREATE TRIGGER guard_stripe_receipt BEFORE UPDATE ON payments.stripe_webhook_receipts
 FOR EACH ROW EXECUTE FUNCTION payments.guard_stripe_receipt();
COMMENT ON FUNCTION payments.guard_stripe_receipt() IS 'payments owner; integration writer allows only webhook redelivery metadata to change; no body retention';

-- Preserve the PAYUNi body exactly: only its name and grants change.
ALTER FUNCTION payments.apply_capture(uuid,bytea) RENAME TO apply_capture_payuni_v1;
REVOKE ALL ON FUNCTION payments.apply_capture_payuni_v1(uuid,bytea) FROM commerce_worker,PUBLIC;
COMMENT ON FUNCTION payments.apply_capture_payuni_v1(uuid,bytea) IS 'payments owner; checkout writer private PAYUNi capture path, byte-identical to migration 0018 body; no Stripe dispatch';

CREATE FUNCTION payments.apply_stripe_observation(p_attempt uuid,p_report_hash bytea)
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
  IF v_review THEN
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
ALTER FUNCTION payments.apply_stripe_observation(uuid,bytea) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION payments.apply_stripe_observation(uuid,bytea) FROM PUBLIC;
COMMENT ON FUNCTION payments.apply_stripe_observation(uuid,bytea) IS 'payments owner; private Stripe observation to immutable facts and single inventory ledger path; no network or webhook authority';

CREATE FUNCTION payments.apply_capture(p_attempt uuid,p_report_hash bytea)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_provider text; v_attempt checkout.payment_attempts%ROWTYPE;
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
  PERFORM payments.apply_stripe_observation(p_attempt,p_report_hash);
 ELSE
  RAISE EXCEPTION 'payment operation unavailable' USING ERRCODE='PT409';
 END IF;
END $$;
ALTER FUNCTION payments.apply_capture(uuid,bytea) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION payments.apply_capture(uuid,bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION payments.apply_capture(uuid,bytea) TO commerce_worker;
COMMENT ON FUNCTION payments.apply_capture(uuid,bytea) IS 'payments owner; worker calls provider dispatcher; PAYUNi and Stripe retain separate private fact paths, no direct ledger write by worker';

-- Worker definers read terminal facts only after a lease has bound the attempt.
GRANT SELECT ON payments.facts TO commerce_integration_writer;
CREATE POLICY stripe_worker_fact_read ON payments.facts FOR SELECT TO commerce_integration_writer USING(true);

CREATE FUNCTION integration.require_stripe_query(p_id uuid,p_generation bigint,p_token bytea,p_profile text)
RETURNS checkout.payment_attempts LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE o integration.operations%ROWTYPE; b integration.bindings%ROWTYPE;
 a checkout.payment_attempts%ROWTYPE; v_binding uuid;
BEGIN
 IF p_id IS NULL OR p_generation IS NULL OR p_generation<2 OR p_token IS NULL OR octet_length(p_token)<>32
  OR p_profile IS NULL OR p_profile NOT IN ('PROVIDER_MOCK','SANDBOX','LIVE') THEN
  RAISE EXCEPTION 'invalid Stripe query' USING ERRCODE='22023'; END IF;
 SELECT x.binding_id INTO v_binding FROM integration.operations x
  WHERE x.id=p_id AND x.actor_kind='BUYER_PAYMENT_QUERY' AND x.provider='stripe';
 IF NOT FOUND THEN RAISE EXCEPTION 'Stripe query unavailable' USING ERRCODE='P0002'; END IF;
 SELECT x.* INTO b FROM integration.bindings x WHERE x.id=v_binding FOR SHARE;
 SELECT x.* INTO o FROM integration.operations x WHERE x.id=p_id FOR UPDATE;
 SELECT x.* INTO a FROM checkout.payment_attempts x WHERE x.id=p_id
  AND x.tenant_id=o.tenant_id AND x.store_id=o.store_id AND x.owner_id=o.buyer_owner_id
  AND x.session_id=o.buyer_session_id AND x.binding_id=o.binding_id;
 IF NOT FOUND OR o.actor_kind<>'BUYER_PAYMENT_QUERY' OR o.action<>'stripe.checkout_session'
  OR o.provider<>'stripe' OR o.purpose<>'transactional' OR o.payment_attempt_id<>a.id
  OR o.binding_version<>a.binding_version OR a.method_code<>'stripe_checkout'
  OR b.id IS NULL OR b.tenant_id<>a.tenant_id OR b.store_id<>a.store_id OR b.provider<>o.provider
  OR b.external_asset_id<>o.external_asset_id OR a.execution_profile<>p_profile
  OR (p_profile='PROVIDER_MOCK' AND a.environment<>'SANDBOX')
  OR (p_profile<>'PROVIDER_MOCK' AND a.environment<>p_profile) THEN
  RAISE EXCEPTION 'Stripe query unavailable' USING ERRCODE='PT409'; END IF;
 IF o.state<>'UNKNOWN' OR o.lease_mode<>'reconcile' OR o.generation<>p_generation
  OR o.lease_until IS NULL OR o.lease_until<=clock_timestamp()
  OR o.lease_token_hash IS DISTINCT FROM sha256(p_token) THEN
  RAISE EXCEPTION 'Stripe query lease conflict' USING ERRCODE='40001'; END IF;
 RETURN a;
END $$;
ALTER FUNCTION integration.require_stripe_query(uuid,bigint,bytea,text) OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION integration.require_stripe_query(uuid,bigint,bytea,text) FROM PUBLIC;
COMMENT ON FUNCTION integration.require_stripe_query(uuid,bigint,bytea,text) IS 'integration owner; private Stripe attempt lease and profile fence; never reveals credentials';

CREATE FUNCTION integration.load_stripe_credential(p_id uuid,p_generation bigint,p_token bytea,p_profile text)
RETURNS TABLE(tenant_id uuid,store_id uuid,connection_id uuid,credential_version bigint,
 environment text,account_id text,key_id text,nonce bytea,ciphertext bytea)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE a checkout.payment_attempts%ROWTYPE; c integration.account_credentials%ROWTYPE;
 m integration.merchant_accounts%ROWTYPE; o integration.operations%ROWTYPE;
BEGIN
 a:=integration.require_stripe_query(p_id,p_generation,p_token,p_profile);
 SELECT x.* INTO m FROM integration.merchant_accounts x WHERE x.id=a.connection_id
  AND x.tenant_id=a.tenant_id AND x.store_id=a.store_id AND x.binding_id=a.binding_id
  AND x.provider='stripe' AND x.environment=a.environment;
 IF NOT FOUND THEN RAISE EXCEPTION 'Stripe account unavailable' USING ERRCODE='PT409'; END IF;
 SELECT x.* INTO c FROM integration.account_credentials x WHERE x.tenant_id=a.tenant_id
  AND x.store_id=a.store_id AND x.connection_id=a.connection_id AND x.version=a.credential_version;
 IF NOT FOUND THEN RAISE EXCEPTION 'Stripe credential unavailable' USING ERRCODE='PT409'; END IF;
 SELECT x.* INTO o FROM integration.operations x WHERE x.id=a.id;
 IF o.external_asset_id<>m.environment||':'||m.account_id THEN
  RAISE EXCEPTION 'Stripe account mismatch' USING ERRCODE='PT409'; END IF;
 -- Recheck after waits so an expired claim cannot release secret material.
 PERFORM integration.require_stripe_query(p_id,p_generation,p_token,p_profile);
 RETURN QUERY SELECT a.tenant_id,a.store_id,a.connection_id,a.credential_version,
  a.environment,m.account_id,c.key_id,c.nonce,c.ciphertext;
END $$;
ALTER FUNCTION integration.load_stripe_credential(uuid,bigint,bytea,text) OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION integration.load_stripe_credential(uuid,bigint,bytea,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION integration.load_stripe_credential(uuid,bigint,bytea,text) TO commerce_worker;
COMMENT ON FUNCTION integration.load_stripe_credential(uuid,bigint,bytea,text) IS 'integration owner; worker reads exact frozen Stripe API credential version under scoped lease; never current head or global key';

CREATE FUNCTION integration.load_stripe_session(p_id uuid,p_generation bigint,p_token bytea,p_profile text)
RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE a checkout.payment_attempts%ROWTYPE; s payments.stripe_sessions%ROWTYPE;
 o integration.operations%ROWTYPE; v_latest jsonb; v_candidate text; v_paid boolean; v_closed boolean;
 v_first_pending timestamptz;
BEGIN
 a:=integration.require_stripe_query(p_id,p_generation,p_token,p_profile);
 SELECT x.* INTO s FROM payments.stripe_sessions x WHERE x.attempt_id=a.id
  AND x.tenant_id=a.tenant_id AND x.store_id=a.store_id;
 SELECT x.* INTO o FROM integration.operations x WHERE x.id=a.id;
 IF NOT FOUND OR s.attempt_id IS NULL OR s.environment<>a.environment
  OR o.external_asset_id<>s.environment||':'||s.account_id THEN
  RAISE EXCEPTION 'Stripe session unavailable' USING ERRCODE='PT409'; END IF;
 SELECT x.report INTO v_latest FROM payments.provider_observations x
  WHERE x.tenant_id=a.tenant_id AND x.store_id=a.store_id AND x.attempt_id=a.id
   AND x.source='QUERY' ORDER BY x.received_at DESC LIMIT 1;
 SELECT min(x.received_at) INTO v_first_pending FROM payments.provider_observations x
  WHERE x.tenant_id=a.tenant_id AND x.store_id=a.store_id AND x.attempt_id=a.id
   AND x.source='QUERY' AND x.report->>'Status'='complete'
   AND x.report->>'PaymentStatus'='unpaid';
 SELECT x.session_id INTO v_candidate FROM payments.stripe_signals x
  WHERE x.tenant_id=a.tenant_id AND x.store_id=a.store_id AND x.attempt_id=a.id
   AND x.consumed_at IS NULL AND x.session_id IS NOT NULL
  ORDER BY x.created_at DESC LIMIT 1;
 SELECT EXISTS(SELECT 1 FROM payments.facts f WHERE f.tenant_id=a.tenant_id
  AND f.store_id=a.store_id AND f.attempt_id=a.id AND f.kind='CAPTURED') INTO v_paid;
 SELECT EXISTS(SELECT 1 FROM payments.facts f WHERE f.tenant_id=a.tenant_id
  AND f.store_id=a.store_id AND f.attempt_id=a.id AND f.kind='CLOSED_UNPAID') INTO v_closed;
 PERFORM integration.require_stripe_query(p_id,p_generation,p_token,p_profile);
 RETURN jsonb_build_object('tenant_id',a.tenant_id,'store_id',a.store_id,'attempt_id',a.id,
  'connection_id',a.connection_id,'credential_version',a.credential_version,
  'environment',a.environment,'account_id',s.account_id,'currency',a.currency,
  'amount_minor',a.amount_minor,'unit_amount',s.unit_amount,'profile',a.execution_profile,
  'create_params',s.create_params,'expires_at',s.expires_at,'send_deadline',s.send_deadline,
  'handoff_cutoff',s.handoff_cutoff,'create_first_sent_at',s.create_first_sent_at,
  'create_last_sent_at',s.create_last_sent_at,'create_send_count',s.create_send_count,
  'create_body_sha256',encode(s.create_body_sha256,'hex'),'create_suppressed_at',s.create_suppressed_at,
  'session_id',s.session_id,'session_url',s.session_url,'payment_intent_id',s.payment_intent_id,
  'first_handed_out_at',s.first_handed_out_at,'cancel_requested_at',s.cancel_requested_at,
  'signal_count',s.signal_count,'expire_calls',s.expire_calls,'latest_report',v_latest,
  'first_complete_unpaid_at',v_first_pending,'signal_candidate',v_candidate,
  'captured',v_paid,'closed_unpaid',v_closed,'db_now',clock_timestamp());
END $$;
ALTER FUNCTION integration.load_stripe_session(uuid,bigint,bytea,text) OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION integration.load_stripe_session(uuid,bigint,bytea,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION integration.load_stripe_session(uuid,bigint,bytea,text) TO commerce_worker;
COMMENT ON FUNCTION integration.load_stripe_session(uuid,bigint,bytea,text) IS 'integration owner; worker reads frozen Stripe session and terminal flags under a lease; no secret material';

CREATE FUNCTION integration.mark_stripe_create_sent(p_id uuid,p_generation bigint,p_token bytea,
 p_profile text,p_body_sha256 bytea) RETURNS text
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE a checkout.payment_attempts%ROWTYPE; s payments.stripe_sessions%ROWTYPE; v_now timestamptz;
BEGIN
 a:=integration.require_stripe_query(p_id,p_generation,p_token,p_profile);
 IF p_body_sha256 IS NULL OR octet_length(p_body_sha256)<>32 THEN
  RAISE EXCEPTION 'invalid Stripe body hash' USING ERRCODE='22023'; END IF;
 SELECT x.* INTO s FROM payments.stripe_sessions x WHERE x.attempt_id=a.id FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'Stripe session unavailable' USING ERRCODE='PT409'; END IF;
 v_now:=clock_timestamp();
 IF s.session_id IS NOT NULL OR s.create_suppressed_at IS NOT NULL OR s.cancel_requested_at IS NOT NULL
  -- RD4 / §10: a recorded first-send rejection is final even before payment_reconcile_v1 applies its
  -- CLOSED_UNPAID. Stripe does not cache a 400 / pre-idempotency 401, so a resend could create a
  -- session after the attempt closes and its stock is released.
  OR EXISTS(SELECT 1 FROM payments.provider_observations o WHERE o.tenant_id=a.tenant_id
   AND o.store_id=a.store_id AND o.attempt_id=a.id AND o.source='QUERY'
   AND o.report->>'Via'='create' AND o.report->>'ErrorClass'='rejected'
   AND o.report->>'SendCount'='1' AND o.report->>'SessionID'='') THEN
  RETURN 'CLOSED';
 END IF;
 IF s.create_first_sent_at IS NULL THEN
  IF v_now>=s.send_deadline THEN RETURN 'CLOSED'; END IF;
  UPDATE payments.stripe_sessions SET create_first_sent_at=v_now,create_last_sent_at=v_now,
   create_send_count=1,create_body_sha256=p_body_sha256 WHERE attempt_id=a.id;
  PERFORM integration.require_stripe_query(p_id,p_generation,p_token,p_profile);
  RETURN 'SEND';
 END IF;
 IF v_now>=s.expires_at+interval '15 minutes' OR s.create_body_sha256<>p_body_sha256
  OR s.create_send_count>=200 THEN RETURN 'CLOSED'; END IF;
 UPDATE payments.stripe_sessions SET create_last_sent_at=v_now,create_send_count=create_send_count+1
  WHERE attempt_id=a.id;
 PERFORM integration.require_stripe_query(p_id,p_generation,p_token,p_profile);
 RETURN 'RESEND';
END $$;
ALTER FUNCTION integration.mark_stripe_create_sent(uuid,bigint,bytea,text,bytea) OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION integration.mark_stripe_create_sent(uuid,bigint,bytea,text,bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION integration.mark_stripe_create_sent(uuid,bigint,bytea,text,bytea) TO commerce_worker;
COMMENT ON FUNCTION integration.mark_stripe_create_sent(uuid,bigint,bytea,text,bytea) IS 'integration owner; worker records exact create body before network send and permits same-key retries only within bound; CLOSED once a first-send rejection is recorded (never resend after it)';

CREATE FUNCTION integration.note_stripe_expire(p_id uuid,p_generation bigint,p_token bytea,p_profile text)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE a checkout.payment_attempts%ROWTYPE; s payments.stripe_sessions%ROWTYPE;
BEGIN
 a:=integration.require_stripe_query(p_id,p_generation,p_token,p_profile);
 SELECT x.* INTO s FROM payments.stripe_sessions x WHERE x.attempt_id=a.id FOR UPDATE;
 IF NOT FOUND OR s.session_id IS NULL OR s.expire_calls>=500 THEN
  RAISE EXCEPTION 'Stripe expire unavailable' USING ERRCODE='PT409'; END IF;
 UPDATE payments.stripe_sessions SET expire_calls=expire_calls+1,last_expire_at=clock_timestamp()
  WHERE attempt_id=a.id;
 PERFORM integration.require_stripe_query(p_id,p_generation,p_token,p_profile);
END $$;
ALTER FUNCTION integration.note_stripe_expire(uuid,bigint,bytea,text) OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION integration.note_stripe_expire(uuid,bigint,bytea,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION integration.note_stripe_expire(uuid,bigint,bytea,text) TO commerce_worker;
COMMENT ON FUNCTION integration.note_stripe_expire(uuid,bigint,bytea,text) IS 'integration owner; worker audits each expire POST before network I/O; no financial closure without retrieve';

CREATE FUNCTION integration.finish_stripe_query(p_id uuid,p_generation bigint,p_token bytea,
 p_profile text,p_code text) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE a checkout.payment_attempts%ROWTYPE; v_lease timestamptz; v_reference text;
BEGIN
 a:=integration.require_stripe_query(p_id,p_generation,p_token,p_profile);
 IF p_code NOT IN ('stripe_create_uncertain','stripe_retrieve_failed','stripe_rate_limited',
  'stripe_timeout','stripe_panic','stripe_record_failed','stripe_session_mismatch',
  'stripe_idempotency_alarm','stripe_unavailable','stripe_budget_exhausted','stripe_terminal_observed') THEN
  RAISE EXCEPTION 'invalid Stripe query completion' USING ERRCODE='22023'; END IF;
 IF p_code='stripe_terminal_observed' THEN
  IF NOT EXISTS(SELECT 1 FROM payments.facts f WHERE f.tenant_id=a.tenant_id
   AND f.store_id=a.store_id AND f.attempt_id=a.id AND f.kind IN ('CAPTURED','CLOSED_UNPAID')) THEN
   RAISE EXCEPTION 'Stripe terminal fact missing' USING ERRCODE='PT409'; END IF;
  UPDATE payments.stripe_sessions SET session_url=NULL,url_purged_at=coalesce(url_purged_at,clock_timestamp())
   WHERE attempt_id=a.id AND session_url IS NOT NULL;
 END IF;
 SELECT x.lease_until,x.provider_reference INTO v_lease,v_reference
  FROM integration.operations x WHERE x.id=a.id;
 PERFORM integration.complete_operation(p_id,p_generation,p_token,'UNKNOWN',p_code,v_reference);
 IF clock_timestamp()>=v_lease THEN
  RAISE EXCEPTION 'Stripe query lease conflict' USING ERRCODE='40001'; END IF;
END $$;
ALTER FUNCTION integration.finish_stripe_query(uuid,bigint,bytea,text,text) OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION integration.finish_stripe_query(uuid,bigint,bytea,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION integration.finish_stripe_query(uuid,bigint,bytea,text,text) TO commerce_worker;
COMMENT ON FUNCTION integration.finish_stripe_query(uuid,bigint,bytea,text,text) IS 'integration owner; worker completes UNKNOWN with fixed reason and terminal URL purge under a lease; no blind retry';

CREATE FUNCTION integration.consume_stripe_signal(p_signal uuid,p_operation uuid,p_generation bigint,
 p_token bytea,p_profile text,p_outcome text) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE a checkout.payment_attempts%ROWTYPE; v_lease timestamptz;
BEGIN
 a:=integration.require_stripe_query(p_operation,p_generation,p_token,p_profile);
 IF p_signal IS NULL OR p_outcome NOT IN
  ('OBSERVED','EXPIRE_REQUESTED','NOOP_TERMINAL','STALE_DROPPED') THEN
  RAISE EXCEPTION 'invalid Stripe signal outcome' USING ERRCODE='22023'; END IF;
 SELECT x.lease_until INTO v_lease FROM integration.operations x WHERE x.id=a.id;
 UPDATE payments.stripe_signals SET consumed_at=clock_timestamp(),outcome=p_outcome
  WHERE id=p_signal AND tenant_id=a.tenant_id AND store_id=a.store_id AND attempt_id=a.id
   AND consumed_at IS NULL;
 IF NOT FOUND THEN RAISE EXCEPTION 'Stripe signal unavailable' USING ERRCODE='PT409'; END IF;
 IF clock_timestamp()>=v_lease THEN
  RAISE EXCEPTION 'Stripe signal lease conflict' USING ERRCODE='40001'; END IF;
END $$;
ALTER FUNCTION integration.consume_stripe_signal(uuid,uuid,bigint,bytea,text,text) OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION integration.consume_stripe_signal(uuid,uuid,bigint,bytea,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION integration.consume_stripe_signal(uuid,uuid,bigint,bytea,text,text) TO commerce_worker;
COMMENT ON FUNCTION integration.consume_stripe_signal(uuid,uuid,bigint,bytea,text,text) IS 'integration owner; worker marks one durable Stripe signal consumed under operation lease; no financial authority';

CREATE FUNCTION checkout.take_stripe_handoff(p_hash bytea,p_store uuid,p_order uuid,
 p_profile text,p_config_digest bytea) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE scope record; scope_final record; ord checkout.orders%ROWTYPE;
 a checkout.payment_attempts%ROWTYPE; s payments.stripe_sessions%ROWTYPE;
 m payments.method_versions%ROWTYPE; q payments.account_qualifications%ROWTYPE;
 acct integration.merchant_accounts%ROWTYPE; b integration.bindings%ROWTYPE;
 v_now timestamptz; v_disposition text;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR p_order IS NULL
  OR p_profile NOT IN ('PROVIDER_MOCK','SANDBOX','LIVE')
  OR p_config_digest IS NULL OR octet_length(p_config_digest)<>32
  OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid Stripe handoff' USING ERRCODE='PT400'; END IF;
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
 IF NOT FOUND OR a.method_code<>'stripe_checkout' THEN
  RAISE EXCEPTION 'Stripe attempt unavailable' USING ERRCODE='PT409'; END IF;
 SELECT x.* INTO s FROM payments.stripe_sessions x WHERE x.attempt_id=a.id FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'Stripe session unavailable' USING ERRCODE='PT409'; END IF;
 v_now:=clock_timestamp();
 IF EXISTS(SELECT 1 FROM payments.facts f WHERE f.tenant_id=a.tenant_id
  AND f.store_id=a.store_id AND f.attempt_id=a.id AND f.kind IN ('CAPTURED','CLOSED_UNPAID')) THEN
  v_disposition:='CLOSED';
 ELSIF ord.commercial_state<>'AWAITING_PAYMENT'
  OR a.execution_profile<>p_profile OR s.config_digest<>p_config_digest
  OR v_now>=s.handoff_cutoff OR s.url_purged_at IS NOT NULL THEN
  v_disposition:='UNAVAILABLE';
 ELSE
  SELECT x.* INTO m FROM payments.method_versions x WHERE x.tenant_id=a.tenant_id
   AND x.store_id=a.store_id AND x.market_id=a.market_id AND x.country=a.country
   AND x.code=a.method_code AND x.version=a.method_version;
  SELECT x.* INTO q FROM payments.account_qualifications x WHERE x.id=a.qualification_id
   AND x.tenant_id=a.tenant_id AND x.store_id=a.store_id;
  SELECT x.* INTO acct FROM integration.merchant_accounts x WHERE x.id=a.connection_id
   AND x.tenant_id=a.tenant_id AND x.store_id=a.store_id;
  SELECT x.* INTO b FROM integration.bindings x WHERE x.id=a.binding_id
   AND x.tenant_id=a.tenant_id AND x.store_id=a.store_id;
  IF m.code IS NULL OR NOT m.enabled OR NOT m.visible OR m.qualification_id<>a.qualification_id
   OR acct.id IS NULL OR acct.provider<>'stripe' OR acct.environment<>a.environment
   -- API-key rotation fences new starts, not this already-pinned Checkout URL.
   -- The attempt and qualification retain the exact historical credential below.
   OR b.id IS NULL OR NOT b.enabled
   OR b.semantic_version<>a.binding_version OR b.external_asset_id<>acct.binding_asset
   OR q.id IS NULL OR q.revoked_at IS NOT NULL OR q.expires_at<=v_now
   OR q.credential_version<>a.credential_version THEN
   v_disposition:='UNAVAILABLE';
  ELSIF s.session_url IS NULL THEN v_disposition:='CREATING';
  ELSE
   v_disposition:='REDIRECT';
   UPDATE payments.stripe_sessions SET first_handed_out_at=coalesce(first_handed_out_at,v_now)
    WHERE attempt_id=a.id;
  END IF;
 END IF;
 SELECT * INTO scope_final FROM buyer.resolve_scope(p_hash,p_store);
 IF NOT FOUND OR scope_final.tenant_id<>scope.tenant_id OR scope_final.owner_id<>scope.owner_id
  OR scope_final.session_id<>scope.session_id THEN
  RAISE EXCEPTION 'buyer capability expired' USING ERRCODE='PT401'; END IF;
 IF v_disposition='REDIRECT' AND clock_timestamp()>=s.handoff_cutoff THEN
  RAISE EXCEPTION 'Stripe handoff cutoff reached' USING ERRCODE='PT409'; END IF;
 RETURN jsonb_build_object('order_id',p_order,'disposition',v_disposition,
  'expires_at',s.handoff_cutoff) ||
  CASE WHEN v_disposition='REDIRECT' THEN jsonb_build_object('redirect_url',s.session_url)
   ELSE '{}'::jsonb END;
END $$;
ALTER FUNCTION checkout.take_stripe_handoff(bytea,uuid,uuid,text,bytea) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION checkout.take_stripe_handoff(bytea,uuid,uuid,text,bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION checkout.take_stripe_handoff(bytea,uuid,uuid,text,bytea) TO commerce_hosted_runtime;
COMMENT ON FUNCTION checkout.take_stripe_handoff(bytea,uuid,uuid,text,bytea) IS 'checkout owner; hosted runtime issues repeatable owned Stripe redirect before cutoff; never creates payment or releases stock';

CREATE FUNCTION checkout.hosted_payment_view_v2(p_hash bytea,p_store uuid,p_order uuid,
 p_profile text,p_payuni_digest bytea,p_stripe_digest bytea) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_base jsonb; scope record; ord checkout.orders%ROWTYPE;
 a checkout.payment_attempts%ROWTYPE; s payments.stripe_sessions%ROWTYPE;
 v_stripe_method jsonb; v_now timestamptz; v_handoff text;
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
  IF EXISTS(SELECT 1 FROM payments.review_cases c WHERE c.tenant_id=a.tenant_id
   AND c.store_id=a.store_id AND c.attempt_id=a.id) THEN
   v_base:=jsonb_set(v_base,'{payment_state}','"REVIEW_REQUIRED"'::jsonb);
  ELSIF EXISTS(SELECT 1 FROM payments.facts f WHERE f.tenant_id=a.tenant_id
   AND f.store_id=a.store_id AND f.attempt_id=a.id AND f.kind='CLOSED_UNPAID') THEN
   v_base:=jsonb_set(v_base,'{payment_state}','"CLOSED_UNPAID"'::jsonb);
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
ALTER FUNCTION checkout.hosted_payment_view_v2(bytea,uuid,uuid,text,bytea,bytea) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION checkout.hosted_payment_view_v2(bytea,uuid,uuid,text,bytea,bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION checkout.hosted_payment_view_v2(bytea,uuid,uuid,text,bytea,bytea)
 TO commerce_hosted_runtime;
COMMENT ON FUNCTION checkout.hosted_payment_view_v2(bytea,uuid,uuid,text,bytea,bytea) IS 'checkout owner; hosted runtime projects PAYUNi and Stripe methods and frozen payment facts; never authorizes capture or stock release';

-- Integrator ruling 2 (docs/delivery/units/stripe-b1-rulings.md): the Go hosted service asks which
-- provider owns the order's payment attempt before choosing take_stripe_handoff or take_hosted_page,
-- so it never has to match an error message. Read-only, takes no row lock, same scope resolution as
-- the sibling hosted definer functions. Caller: internal/checkout (HostedPaymentStarter.TakeHosted).
CREATE FUNCTION checkout.hosted_payment_provider(p_hash bytea,p_store uuid,p_order uuid) RETURNS text
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE scope record; v_code text;
BEGIN
 IF current_setting('transaction_isolation')<>'read committed'
  OR p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR p_order IS NULL THEN
  RAISE EXCEPTION 'invalid provider lookup' USING ERRCODE='PT400'; END IF;
 SELECT * INTO scope FROM buyer.resolve_scope(p_hash,p_store);
 IF NOT FOUND THEN RAISE EXCEPTION 'invalid buyer capability' USING ERRCODE='PT401'; END IF;
 PERFORM set_config('app.tenant_id',scope.tenant_id::text,true);
 PERFORM set_config('app.store_id',p_store::text,true);
 PERFORM set_config('app.buyer_id',scope.owner_id::text,true);
 PERFORM set_config('app.buyer_session_id',scope.session_id::text,true);
 PERFORM set_config('app.principal_id','',true);
 SELECT x.method_code INTO v_code FROM checkout.payment_attempts x WHERE x.tenant_id=scope.tenant_id
  AND x.store_id=p_store AND x.owner_id=scope.owner_id AND x.order_id=p_order;
 RETURN CASE v_code WHEN 'stripe_checkout' THEN 'stripe' WHEN 'payuni_credit' THEN 'payuni' ELSE NULL END;
END $$;
ALTER FUNCTION checkout.hosted_payment_provider(bytea,uuid,uuid) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION checkout.hosted_payment_provider(bytea,uuid,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION checkout.hosted_payment_provider(bytea,uuid,uuid) TO commerce_hosted_runtime;
COMMENT ON FUNCTION checkout.hosted_payment_provider(bytea,uuid,uuid) IS 'checkout owner; hosted runtime reads the provider (stripe|payuni|NULL) of the caller-scoped order attempt to route handoff; read-only, no lock, never returns provider material';

GRANT SELECT,INSERT ON payments.stripe_webhook_endpoints TO commerce_payment_registry_writer;
GRANT UPDATE(enabled,key_version,key_id,nonce,ciphertext,updated_at)
 ON payments.stripe_webhook_endpoints TO commerce_payment_registry_writer;
CREATE POLICY stripe_endpoint_registry ON payments.stripe_webhook_endpoints
 TO commerce_payment_registry_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);

CREATE FUNCTION payments.guard_stripe_endpoint() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 -- provider is GENERATED ALWAYS; its NEW value is computed after BEFORE triggers.
 IF (OLD.endpoint_id,OLD.tenant_id,OLD.store_id,OLD.connection_id,
  OLD.environment,OLD.account_id,OLD.execution_profile,OLD.created_at)
  IS DISTINCT FROM
  (NEW.endpoint_id,NEW.tenant_id,NEW.store_id,NEW.connection_id,
  NEW.environment,NEW.account_id,NEW.execution_profile,NEW.created_at)
  OR NEW.key_version<>OLD.key_version+1 THEN
  RAISE EXCEPTION 'Stripe endpoint identity or version changed' USING ERRCODE='42501'; END IF;
 RETURN NEW;
END $$;
ALTER FUNCTION payments.guard_stripe_endpoint() OWNER TO commerce_payment_registry_writer;
REVOKE ALL ON FUNCTION payments.guard_stripe_endpoint() FROM PUBLIC;
CREATE TRIGGER guard_stripe_endpoint BEFORE UPDATE ON payments.stripe_webhook_endpoints
 FOR EACH ROW EXECUTE FUNCTION payments.guard_stripe_endpoint();
COMMENT ON FUNCTION payments.guard_stripe_endpoint() IS 'payments owner; registry writer may rotate endpoint signing envelope only at next version; no rebind or API-key custody';

CREATE FUNCTION integration.require_stripe_registrar_scope(p_tenant uuid,p_store uuid,p_principal uuid)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 IF p_tenant IS NULL OR p_store IS NULL OR p_principal IS NULL
  OR NOT EXISTS(SELECT 1 FROM control.stores s JOIN identity.memberships m
    ON m.tenant_id=s.tenant_id AND m.principal_id=p_principal AND m.active
   JOIN identity.store_grants g ON g.tenant_id=s.tenant_id AND g.store_id=s.id
    AND g.principal_id=m.principal_id AND g.permission='integration:manage'
   WHERE s.tenant_id=p_tenant AND s.id=p_store AND s.active) THEN
  RAISE EXCEPTION 'Stripe registrar scope unavailable' USING ERRCODE='42501'; END IF;
 PERFORM set_config('app.tenant_id',p_tenant::text,true);
 PERFORM set_config('app.store_id',p_store::text,true);
 PERFORM set_config('app.principal_id',p_principal::text,true);
END $$;
ALTER FUNCTION integration.require_stripe_registrar_scope(uuid,uuid,uuid)
 OWNER TO commerce_payment_registry_writer;
REVOKE ALL ON FUNCTION integration.require_stripe_registrar_scope(uuid,uuid,uuid) FROM PUBLIC;
COMMENT ON FUNCTION integration.require_stripe_registrar_scope(uuid,uuid,uuid) IS 'integration owner; private registrar validates active store/member integration grant and pins GUC scope; no merchant login access';

CREATE FUNCTION integration.register_stripe_account(p_tenant uuid,p_store uuid,p_principal uuid,
 p_connection uuid,p_binding uuid,p_environment text,p_account text,p_key_id text,
 p_nonce bytea,p_ciphertext bytea) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 PERFORM integration.require_stripe_registrar_scope(p_tenant,p_store,p_principal);
 IF p_connection IS NULL OR p_binding IS NULL OR p_environment<>'SANDBOX'
  OR p_account !~ '^acct_[A-Za-z0-9]{1,59}$'
  OR p_key_id !~ '^[A-Za-z0-9_-]{1,40}$'
  OR octet_length(p_nonce)<>12 OR octet_length(p_ciphertext) NOT BETWEEN 17 AND 8192 THEN
  RAISE EXCEPTION 'invalid Stripe registrar input' USING ERRCODE='22023'; END IF;
 INSERT INTO integration.bindings(id,tenant_id,store_id,principal_id,provider,external_asset_id)
 VALUES(p_binding,p_tenant,p_store,p_principal,'stripe',p_environment||':'||p_account);
 INSERT INTO integration.merchant_accounts(id,tenant_id,store_id,principal_id,provider,
  environment,account_id,binding_id,credential_version)
 VALUES(p_connection,p_tenant,p_store,p_principal,'stripe',p_environment,p_account,p_binding,1);
 INSERT INTO integration.account_credentials(tenant_id,store_id,connection_id,version,key_id,
  nonce,ciphertext,principal_id)
 VALUES(p_tenant,p_store,p_connection,1,p_key_id,p_nonce,p_ciphertext,p_principal);
 INSERT INTO ops.audit_events(tenant_id,store_id,principal_id,action)
 VALUES(p_tenant,p_store,p_principal,'stripe.account_registered');
 RETURN p_connection;
END $$;
ALTER FUNCTION integration.register_stripe_account(uuid,uuid,uuid,uuid,uuid,text,text,text,bytea,bytea)
 OWNER TO commerce_payment_registry_writer;
REVOKE ALL ON FUNCTION integration.register_stripe_account(uuid,uuid,uuid,uuid,uuid,text,text,text,bytea,bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION integration.register_stripe_account(uuid,uuid,uuid,uuid,uuid,text,text,text,bytea,bytea)
 TO commerce_payment_registrar;
COMMENT ON FUNCTION integration.register_stripe_account(uuid,uuid,uuid,uuid,uuid,text,text,text,bytea,bytea) IS 'integration owner; operator registrar atomically binds one SANDBOX Stripe account and encrypted API credential to one store; no plaintext or pooled funds';

CREATE FUNCTION integration.rotate_stripe_key(p_tenant uuid,p_store uuid,p_principal uuid,
 p_connection uuid,p_expected_version bigint,p_key_id text,p_nonce bytea,p_ciphertext bytea)
RETURNS bigint LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE acct integration.merchant_accounts%ROWTYPE; v_next bigint;
BEGIN
 PERFORM integration.require_stripe_registrar_scope(p_tenant,p_store,p_principal);
 IF p_connection IS NULL OR p_expected_version IS NULL OR p_expected_version<1
  OR p_key_id !~ '^[A-Za-z0-9_-]{1,40}$'
  OR octet_length(p_nonce)<>12 OR octet_length(p_ciphertext) NOT BETWEEN 17 AND 8192 THEN
  RAISE EXCEPTION 'invalid Stripe rotation input' USING ERRCODE='22023'; END IF;
 SELECT x.* INTO acct FROM integration.merchant_accounts x WHERE x.tenant_id=p_tenant
  AND x.store_id=p_store AND x.id=p_connection AND x.provider='stripe' FOR UPDATE;
 IF NOT FOUND OR acct.credential_version<>p_expected_version OR acct.environment<>'SANDBOX' THEN
  RAISE EXCEPTION 'Stripe credential version changed' USING ERRCODE='PT409'; END IF;
 v_next:=p_expected_version+1;
 INSERT INTO integration.account_credentials(tenant_id,store_id,connection_id,version,key_id,
  nonce,ciphertext,principal_id)
 VALUES(p_tenant,p_store,p_connection,v_next,p_key_id,p_nonce,p_ciphertext,p_principal);
 UPDATE integration.merchant_accounts SET credential_version=v_next,updated_at=clock_timestamp()
  WHERE tenant_id=p_tenant AND store_id=p_store AND id=p_connection;
 INSERT INTO ops.audit_events(tenant_id,store_id,principal_id,action)
 VALUES(p_tenant,p_store,p_principal,'stripe.key_rotated');
 RETURN v_next;
END $$;
ALTER FUNCTION integration.rotate_stripe_key(uuid,uuid,uuid,uuid,bigint,text,bytea,bytea)
 OWNER TO commerce_payment_registry_writer;
REVOKE ALL ON FUNCTION integration.rotate_stripe_key(uuid,uuid,uuid,uuid,bigint,text,bytea,bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION integration.rotate_stripe_key(uuid,uuid,uuid,uuid,bigint,text,bytea,bytea)
 TO commerce_payment_registrar;
COMMENT ON FUNCTION integration.rotate_stripe_key(uuid,uuid,uuid,uuid,bigint,text,bytea,bytea) IS 'integration owner; operator registrar appends exact next encrypted Stripe API key version and advances head; historical attempts remain frozen';

CREATE FUNCTION payments.set_stripe_webhook_endpoint(p_tenant uuid,p_store uuid,p_principal uuid,
 p_connection uuid,p_endpoint uuid,p_profile text,p_expected_version bigint,p_enabled boolean,
 p_key_id text,p_nonce bytea,p_ciphertext bytea) RETURNS bigint
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE acct integration.merchant_accounts%ROWTYPE; ep payments.stripe_webhook_endpoints%ROWTYPE;
 v_next bigint;
BEGIN
 PERFORM integration.require_stripe_registrar_scope(p_tenant,p_store,p_principal);
 IF p_connection IS NULL OR p_endpoint IS NULL OR p_profile NOT IN ('PROVIDER_MOCK','SANDBOX')
  OR p_expected_version IS NULL OR p_expected_version<0 OR p_enabled IS NULL
  OR p_key_id !~ '^[A-Za-z0-9_-]{1,40}$'
  OR octet_length(p_nonce)<>12 OR octet_length(p_ciphertext) NOT BETWEEN 17 AND 8192 THEN
  RAISE EXCEPTION 'invalid Stripe endpoint input' USING ERRCODE='22023'; END IF;
 SELECT x.* INTO acct FROM integration.merchant_accounts x WHERE x.tenant_id=p_tenant
  AND x.store_id=p_store AND x.id=p_connection AND x.provider='stripe' FOR SHARE;
 IF NOT FOUND OR acct.environment<>'SANDBOX' THEN
  RAISE EXCEPTION 'Stripe endpoint account unavailable' USING ERRCODE='PT409'; END IF;
 SELECT x.* INTO ep FROM payments.stripe_webhook_endpoints x WHERE x.endpoint_id=p_endpoint FOR UPDATE;
 IF p_expected_version=0 THEN
  IF FOUND THEN RAISE EXCEPTION 'Stripe endpoint already exists' USING ERRCODE='PT409'; END IF;
  INSERT INTO payments.stripe_webhook_endpoints(endpoint_id,tenant_id,store_id,connection_id,
   environment,account_id,execution_profile,enabled,key_version,key_id,nonce,ciphertext)
  VALUES(p_endpoint,p_tenant,p_store,p_connection,acct.environment,acct.account_id,p_profile,
   p_enabled,1,p_key_id,p_nonce,p_ciphertext);
  v_next:=1;
 ELSE
  IF NOT FOUND OR ep.tenant_id<>p_tenant OR ep.store_id<>p_store
   OR ep.connection_id<>p_connection OR ep.execution_profile<>p_profile
   OR ep.key_version<>p_expected_version THEN
   RAISE EXCEPTION 'Stripe endpoint version changed' USING ERRCODE='PT409'; END IF;
  v_next:=p_expected_version+1;
  UPDATE payments.stripe_webhook_endpoints SET enabled=p_enabled,key_version=v_next,
   key_id=p_key_id,nonce=p_nonce,ciphertext=p_ciphertext,updated_at=clock_timestamp()
   WHERE endpoint_id=p_endpoint;
 END IF;
 INSERT INTO ops.audit_events(tenant_id,store_id,principal_id,action)
 VALUES(p_tenant,p_store,p_principal,'stripe.webhook_endpoint_set');
 RETURN v_next;
END $$;
ALTER FUNCTION payments.set_stripe_webhook_endpoint(uuid,uuid,uuid,uuid,uuid,text,bigint,boolean,text,bytea,bytea)
 OWNER TO commerce_payment_registry_writer;
REVOKE ALL ON FUNCTION payments.set_stripe_webhook_endpoint(uuid,uuid,uuid,uuid,uuid,text,bigint,boolean,text,bytea,bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION payments.set_stripe_webhook_endpoint(uuid,uuid,uuid,uuid,uuid,text,bigint,boolean,text,bytea,bytea)
 TO commerce_payment_registrar;
COMMENT ON FUNCTION payments.set_stripe_webhook_endpoint(uuid,uuid,uuid,uuid,uuid,text,bigint,boolean,text,bytea,bytea) IS 'payments owner; operator registrar creates or rotates one endpoint signing envelope by CAS; no API key, rebind or LIVE ingress';

-- §0.2: an endpoint's account derives from its registered connection, never from operator env.
-- The registrar must seal the stripe-webhook-v1 AAD with that same account before calling
-- set_stripe_webhook_endpoint, so it reads it here (scoped, read-only; account ids are not secret).
CREATE FUNCTION payments.stripe_endpoint_account(p_tenant uuid,p_store uuid,p_principal uuid,
 p_connection uuid) RETURNS text
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_account text;
BEGIN
 PERFORM integration.require_stripe_registrar_scope(p_tenant,p_store,p_principal);
 SELECT x.account_id INTO v_account FROM integration.merchant_accounts x WHERE x.tenant_id=p_tenant
  AND x.store_id=p_store AND x.id=p_connection AND x.provider='stripe' AND x.environment='SANDBOX';
 IF v_account IS NULL THEN
  RAISE EXCEPTION 'Stripe endpoint account unavailable' USING ERRCODE='PT409'; END IF;
 RETURN v_account;
END $$;
ALTER FUNCTION payments.stripe_endpoint_account(uuid,uuid,uuid,uuid)
 OWNER TO commerce_payment_registry_writer;
REVOKE ALL ON FUNCTION payments.stripe_endpoint_account(uuid,uuid,uuid,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION payments.stripe_endpoint_account(uuid,uuid,uuid,uuid)
 TO commerce_payment_registrar;
COMMENT ON FUNCTION payments.stripe_endpoint_account(uuid,uuid,uuid,uuid) IS 'payments owner; operator registrar reads the registered SANDBOX account id of one in-scope Stripe connection to seal webhook AAD; no key material, no writes';

-- S4: qualify probes with the credential the connection actually stores at the expected version,
-- against its registered account, so an old key or another account's key cannot qualify a head.
-- Only the head version is returned (a stale expected version is PT409, before any network call).
CREATE FUNCTION payments.stripe_registrar_credential(p_tenant uuid,p_store uuid,p_principal uuid,
 p_connection uuid,p_expected_version bigint)
RETURNS TABLE(account_id text,key_id text,nonce bytea,ciphertext bytea)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE acct integration.merchant_accounts%ROWTYPE; c integration.account_credentials%ROWTYPE;
BEGIN
 PERFORM integration.require_stripe_registrar_scope(p_tenant,p_store,p_principal);
 IF p_connection IS NULL OR p_expected_version IS NULL OR p_expected_version<1 THEN
  RAISE EXCEPTION 'invalid Stripe credential read' USING ERRCODE='22023'; END IF;
 SELECT x.* INTO acct FROM integration.merchant_accounts x WHERE x.tenant_id=p_tenant
  AND x.store_id=p_store AND x.id=p_connection AND x.provider='stripe' AND x.environment='SANDBOX';
 IF NOT FOUND OR acct.credential_version<>p_expected_version THEN
  RAISE EXCEPTION 'Stripe credential version changed' USING ERRCODE='PT409'; END IF;
 SELECT x.* INTO c FROM integration.account_credentials x WHERE x.tenant_id=p_tenant
  AND x.store_id=p_store AND x.connection_id=p_connection AND x.version=p_expected_version;
 IF NOT FOUND THEN RAISE EXCEPTION 'Stripe credential unavailable' USING ERRCODE='PT409'; END IF;
 RETURN QUERY SELECT acct.account_id,c.key_id,c.nonce,c.ciphertext;
END $$;
ALTER FUNCTION payments.stripe_registrar_credential(uuid,uuid,uuid,uuid,bigint)
 OWNER TO commerce_payment_registry_writer;
REVOKE ALL ON FUNCTION payments.stripe_registrar_credential(uuid,uuid,uuid,uuid,bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION payments.stripe_registrar_credential(uuid,uuid,uuid,uuid,bigint)
 TO commerce_payment_registrar;
COMMENT ON FUNCTION payments.stripe_registrar_credential(uuid,uuid,uuid,uuid,bigint) IS 'payments owner; operator registrar reads the sealed API credential envelope at the connection head version plus its registered account so SANDBOX qualify probes with the stored key; ciphertext only, no writes, no worker or runtime access';

CREATE FUNCTION payments.qualify_stripe_method(p_tenant uuid,p_store uuid,p_principal uuid,
 p_qualification uuid,p_connection uuid,p_expected_version bigint,p_profile text,p_evidence_ref text,
 p_observed_at timestamptz,p_expires_at timestamptz) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE acct integration.merchant_accounts%ROWTYPE; b integration.bindings%ROWTYPE;
 v_proof text;
BEGIN
 PERFORM integration.require_stripe_registrar_scope(p_tenant,p_store,p_principal);
 IF p_qualification IS NULL OR p_connection IS NULL OR p_expected_version IS NULL
  OR p_expected_version<1 OR p_profile NOT IN ('PROVIDER_MOCK','SANDBOX')
  OR p_evidence_ref IS NULL OR length(p_evidence_ref) NOT BETWEEN 1 AND 200
  OR p_evidence_ref ~ '[[:cntrl:]]' OR p_observed_at IS NULL OR p_expires_at IS NULL
  OR p_observed_at>clock_timestamp() OR p_expires_at<=p_observed_at
  OR p_expires_at>p_observed_at+interval '30 days' THEN
  RAISE EXCEPTION 'invalid Stripe qualification input' USING ERRCODE='22023'; END IF;
 IF p_profile='SANDBOX' AND p_evidence_ref !~ '^stripe-probe:[A-Za-z0-9_]{1,255}$' THEN
  RAISE EXCEPTION 'Stripe probe evidence missing' USING ERRCODE='22023'; END IF;
 SELECT x.* INTO acct FROM integration.merchant_accounts x WHERE x.tenant_id=p_tenant
  AND x.store_id=p_store AND x.id=p_connection AND x.provider='stripe' FOR SHARE;
 IF NOT FOUND OR acct.environment<>'SANDBOX' OR acct.credential_version<>p_expected_version THEN
  RAISE EXCEPTION 'Stripe account unavailable' USING ERRCODE='PT409'; END IF;
 SELECT x.* INTO b FROM integration.bindings x WHERE x.tenant_id=p_tenant AND x.store_id=p_store
  AND x.id=acct.binding_id FOR SHARE;
 IF NOT FOUND OR NOT b.enabled OR b.provider<>'stripe'
  OR b.external_asset_id<>acct.binding_asset THEN
  RAISE EXCEPTION 'Stripe binding unavailable' USING ERRCODE='PT409'; END IF;
 v_proof:=CASE WHEN p_profile='PROVIDER_MOCK' THEN 'PROVIDER_MOCK' ELSE 'REAL_SANDBOX' END;
 INSERT INTO payments.account_qualifications(id,tenant_id,store_id,connection_id,credential_version,
  environment,code,proof_class,evidence_ref,observed_at,expires_at)
 VALUES(p_qualification,p_tenant,p_store,p_connection,acct.credential_version,'SANDBOX',
  'stripe_checkout',v_proof,p_evidence_ref,p_observed_at,p_expires_at);
 INSERT INTO ops.audit_events(tenant_id,store_id,principal_id,action)
 VALUES(p_tenant,p_store,p_principal,'stripe.method_qualified');
 RETURN p_qualification;
END $$;
ALTER FUNCTION payments.qualify_stripe_method(uuid,uuid,uuid,uuid,uuid,bigint,text,text,timestamptz,timestamptz)
 OWNER TO commerce_payment_registry_writer;
REVOKE ALL ON FUNCTION payments.qualify_stripe_method(uuid,uuid,uuid,uuid,uuid,bigint,text,text,timestamptz,timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION payments.qualify_stripe_method(uuid,uuid,uuid,uuid,uuid,bigint,text,text,timestamptz,timestamptz)
 TO commerce_payment_registrar;
COMMENT ON FUNCTION payments.qualify_stripe_method(uuid,uuid,uuid,uuid,uuid,bigint,text,text,timestamptz,timestamptz) IS 'payments owner; operator registrar records bounded SANDBOX or mock probe evidence for expected Stripe credential version; no LIVE qualification';

-- The registry role is created above. Grant only the pure amount predicate
-- needed by set_stripe_method, not new table rights or merchant runtime access.
GRANT EXECUTE ON FUNCTION payments.stripe_amount_ok(text,bigint) TO commerce_payment_registry_writer;
CREATE FUNCTION payments.set_stripe_method(p_tenant uuid,p_store uuid,p_principal uuid,
 p_market uuid,p_country text,p_connection uuid,p_qualification uuid,p_expected_version bigint,
 p_enabled boolean,p_visible boolean,p_sort integer,p_min bigint,p_max bigint,
 p_name_hans text,p_name_hant text,p_name_en text) RETURNS bigint
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE market pricing.markets%ROWTYPE; acct integration.merchant_accounts%ROWTYPE;
 b integration.bindings%ROWTYPE; q payments.account_qualifications%ROWTYPE;
 v_current bigint; v_next bigint; v_now timestamptz;
BEGIN
 PERFORM integration.require_stripe_registrar_scope(p_tenant,p_store,p_principal);
 IF p_market IS NULL OR p_connection IS NULL OR p_qualification IS NULL
  OR p_country !~ '^[A-Z]{2}$' OR p_expected_version IS NULL OR p_expected_version<0
  OR p_enabled IS NULL OR p_visible IS NULL OR p_sort NOT BETWEEN 0 AND 1000
  OR p_min IS NULL OR p_max IS NULL OR p_max<p_min THEN
  RAISE EXCEPTION 'invalid Stripe method input' USING ERRCODE='22023'; END IF;
 SELECT x.* INTO market FROM pricing.markets x WHERE x.tenant_id=p_tenant
  AND x.store_id=p_store AND x.id=p_market FOR SHARE;
 IF NOT FOUND OR NOT market.active OR NOT payments.stripe_amount_ok(market.currency,p_min)
  OR NOT payments.stripe_amount_ok(market.currency,p_max) THEN
  RAISE EXCEPTION 'Stripe market amount unavailable' USING ERRCODE='PT409'; END IF;
 SELECT x.* INTO acct FROM integration.merchant_accounts x WHERE x.tenant_id=p_tenant
  AND x.store_id=p_store AND x.id=p_connection AND x.provider='stripe' FOR SHARE;
 IF NOT FOUND OR acct.environment<>'SANDBOX' THEN
  RAISE EXCEPTION 'Stripe account unavailable' USING ERRCODE='PT409'; END IF;
 SELECT x.* INTO b FROM integration.bindings x WHERE x.tenant_id=p_tenant
  AND x.store_id=p_store AND x.id=acct.binding_id FOR SHARE;
 IF NOT FOUND OR NOT b.enabled OR b.provider<>'stripe'
  OR b.external_asset_id<>acct.binding_asset THEN
  RAISE EXCEPTION 'Stripe binding unavailable' USING ERRCODE='PT409'; END IF;
 SELECT x.* INTO q FROM payments.account_qualifications x WHERE x.id=p_qualification
  AND x.tenant_id=p_tenant AND x.store_id=p_store FOR SHARE;
 v_now:=clock_timestamp();
 IF NOT FOUND OR q.connection_id<>p_connection OR q.credential_version<>acct.credential_version
  OR q.environment<>'SANDBOX' OR q.code<>'stripe_checkout' OR q.proof_class='REAL_LIVE'
  OR q.revoked_at IS NOT NULL OR q.observed_at>v_now OR q.expires_at<=v_now THEN
  RAISE EXCEPTION 'Stripe qualification unavailable' USING ERRCODE='PT409'; END IF;
 SELECT h.current_version INTO v_current FROM payments.method_heads h WHERE h.tenant_id=p_tenant
  AND h.store_id=p_store AND h.market_id=p_market AND h.country=p_country
  AND h.code='stripe_checkout' FOR UPDATE;
 IF p_expected_version=0 THEN
  IF FOUND THEN RAISE EXCEPTION 'Stripe method already exists' USING ERRCODE='PT409'; END IF;
  v_next:=1;
 ELSE
  IF NOT FOUND OR v_current<>p_expected_version THEN
   RAISE EXCEPTION 'Stripe method version changed' USING ERRCODE='PT409'; END IF;
  v_next:=p_expected_version+1;
 END IF;
 INSERT INTO payments.method_versions(tenant_id,store_id,market_id,country,code,version,provider,
  environment,connection_id,binding_version,currency,name_hans,name_hant,name_en,
  enabled,visible,sort_order,min_amount_minor,max_amount_minor,principal_id,qualification_id)
 VALUES(p_tenant,p_store,p_market,p_country,'stripe_checkout',v_next,'stripe','SANDBOX',
  p_connection,b.semantic_version,market.currency,p_name_hans,p_name_hant,p_name_en,
  p_enabled,p_visible,p_sort,p_min,p_max,p_principal,p_qualification);
 IF p_expected_version=0 THEN
  INSERT INTO payments.method_heads(tenant_id,store_id,market_id,country,code,current_version)
  VALUES(p_tenant,p_store,p_market,p_country,'stripe_checkout',v_next);
 ELSE
  UPDATE payments.method_heads SET current_version=v_next WHERE tenant_id=p_tenant
   AND store_id=p_store AND market_id=p_market AND country=p_country AND code='stripe_checkout';
 END IF;
 INSERT INTO ops.audit_events(tenant_id,store_id,principal_id,action)
 VALUES(p_tenant,p_store,p_principal,'stripe.method_set');
 RETURN v_next;
END $$;
ALTER FUNCTION payments.set_stripe_method(uuid,uuid,uuid,uuid,text,uuid,uuid,bigint,boolean,boolean,integer,bigint,bigint,text,text,text)
 OWNER TO commerce_payment_registry_writer;
REVOKE ALL ON FUNCTION payments.set_stripe_method(uuid,uuid,uuid,uuid,text,uuid,uuid,bigint,boolean,boolean,integer,bigint,bigint,text,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION payments.set_stripe_method(uuid,uuid,uuid,uuid,text,uuid,uuid,bigint,boolean,boolean,integer,bigint,bigint,text,text,text)
 TO commerce_payment_registrar;
COMMENT ON FUNCTION payments.set_stripe_method(uuid,uuid,uuid,uuid,text,uuid,uuid,bigint,boolean,boolean,integer,bigint,bigint,text,text,text) IS 'payments owner; operator registrar appends Stripe method revision with exact scoped account/qualification and amount bounds; merchant SetMethod remains PAYUNi only';
