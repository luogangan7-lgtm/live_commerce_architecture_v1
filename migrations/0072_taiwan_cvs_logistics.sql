-- 0072 Taiwan CVS logistics schema (contracts/taiwan-cvs-logistics-v1.md FROZEN 2026-09-30, §4.1, §4.2,
-- §16.1-§16.8; docs/delivery/units/cvs-core.md; rulings X5, X8, X9, B19-B22).
--
-- Owns: the widened constraints of §4.1 (CVS kinds, verification kinds, payment mode / collection state,
-- PROVIDER_LABEL_CREATED, API service binding, ecpay_logistics accounts, DEALLOCATE ledger kind), the tables
-- integration.ecpay_logistics_profiles, fulfillment.cvs_selections, fulfillment.cvs_shipments,
-- fulfillment.cvs_shipment_events, fulfillment.cvs_store_settings, and the trigger guards that make those
-- tables and the ledger safe on their own (identity immutability, CAS, order <=> shipment state, ledger
-- provenance for pay-at-pickup ALLOCATE / DEALLOCATE).
--
-- Non-goals: no definer function, grant or policy for the new tables (0073), no `checkout.begin_hold`
-- replacement (post_river/0017: migrate.go applies post_river/0005 after every main migration), no
-- recipient name/phone/address column anywhere (TD8), no ECPay HTTP or River job from SQL.
--
-- Every widened CHECK is re-derived from the LIVE pg_get_constraintdef and only extended (shape asserted
-- first, so an unexpected earlier migration stops this file instead of being silently dropped).
--
-- Depends on: 0010/0012 (service and pickup tables), 0013 (orders, ledger, checkout policies), 0014/0061
-- (merchant accounts), 0016/0018/0061/0062 (operation family, ledger and events CHECKs), 0063 (fulfilment
-- state), 0008 (operations), 0060+ river-independent.
-- Callers: internal/fulfillment, internal/checkout, internal/merchantorders, internal/storefront (Go);
-- every function that writes these tables is defined in 0073 / post_river/0017.

-- ---------------------------------------------------------------------------------------------------
-- §4.1 widened constraints, one helper-free DO block per constraint family.
-- ---------------------------------------------------------------------------------------------------
DO $$
DECLARE v_def text; v_item record;
BEGIN
 -- (table, constraint, text that must be present exactly once, replacement, text that must be absent)
 FOR v_item IN SELECT * FROM (VALUES
  ('fulfillment.pickup_versions','pickup_versions_kind_check',
   '''cvs_familymart''::text]','''cvs_familymart''::text, ''cvs_hilife''::text, ''cvs_okmart''::text]','cvs_hilife'),
  ('storefront.destination_snapshots','destination_snapshots_kind_check',
   '''cvs_familymart''::text]','''cvs_familymart''::text, ''cvs_hilife''::text, ''cvs_okmart''::text]','cvs_hilife'),
  ('storefront.destination_snapshots','destination_snapshots_check',
   '''cvs_familymart''::text]','''cvs_familymart''::text, ''cvs_hilife''::text, ''cvs_okmart''::text]','cvs_hilife'),
  ('fulfillment.service_versions','service_versions_delivery_kind_check',
   '''cvs_familymart''::text]','''cvs_familymart''::text, ''cvs_hilife''::text, ''cvs_okmart''::text]','cvs_hilife'),
  ('checkout.orders','orders_fulfillment_state_check',
   '''MERCHANT_SHIPPED''::text]','''MERCHANT_SHIPPED''::text, ''PROVIDER_LABEL_CREATED''::text]','PROVIDER_LABEL_CREATED'),
  ('integration.merchant_accounts','merchant_accounts_provider_check',
   '''stripe''::text]','''stripe''::text, ''ecpay_logistics''::text]','ecpay_logistics'),
  ('inventory.ledger','ledger_kind_check',
   '''ALLOCATE''::text]','''ALLOCATE''::text, ''DEALLOCATE''::text]','DEALLOCATE'),
  ('checkout.events','events_action_check',
   '''checkout.payment_closed''::text]','''checkout.payment_closed''::text, ''checkout.pay_at_pickup_placed''::text]','pay_at_pickup_placed'),
  ('checkout.events','events_actor_action',
   '''checkout.held''::text, ''checkout.payment_started''::text]',
   '''checkout.held''::text, ''checkout.payment_started''::text, ''checkout.pay_at_pickup_placed''::text]','pay_at_pickup_placed')
 ) AS t(rel,con,needle,repl,absent) LOOP
  SELECT pg_get_constraintdef(c.oid) INTO v_def FROM pg_constraint c
   WHERE c.conrelid=v_item.rel::regclass AND c.conname=v_item.con;
  IF v_def IS NULL OR v_def LIKE '%'||v_item.absent||'%'
   OR length(v_def)-length(replace(v_def,v_item.needle,''))<>length(v_item.needle) THEN
   RAISE EXCEPTION '% % has an unexpected shape: %',v_item.rel,v_item.con,v_def;
  END IF;
  v_def:=replace(v_def,v_item.needle,v_item.repl);
  EXECUTE format('ALTER TABLE %s DROP CONSTRAINT %I',v_item.rel,v_item.con);
  EXECUTE format('ALTER TABLE %s ADD CONSTRAINT %I %s',v_item.rel,v_item.con,v_def);
 END LOOP;
END $$;

-- pickup_versions: verification kinds, nullable principal only for BUYER_ENTERED (a buyer has no membership).
ALTER TABLE fulfillment.pickup_versions DROP CONSTRAINT pickup_versions_verification_kind_check;
ALTER TABLE fulfillment.pickup_versions ADD CONSTRAINT pickup_versions_verification_kind_check
 CHECK(verification_kind IN ('MANUAL_ATTESTED','PROVIDER_DIRECTORY_VERIFIED','BUYER_ENTERED'));
ALTER TABLE fulfillment.pickup_versions ALTER COLUMN principal_id DROP NOT NULL;
ALTER TABLE fulfillment.pickup_versions ADD CONSTRAINT pickup_versions_principal_by_kind
 CHECK((verification_kind='BUYER_ENTERED')=(principal_id IS NULL));

-- A merchant can never claim directory verification or write the provider/buyer namespaces (round 1, finding 7;
-- round 3, R3-4). 0012 grants commerce_runtime INSERT on both tables and UPDATE(current_version,pickup_id,enabled)
-- on heads, so only RESTRICTIVE policies close the namespaces.
CREATE POLICY pickup_versions_manual_only ON fulfillment.pickup_versions AS RESTRICTIVE FOR INSERT TO commerce_runtime
 WITH CHECK(verification_kind='MANUAL_ATTESTED');
CREATE POLICY pickup_versions_closed_namespace ON fulfillment.pickup_versions AS RESTRICTIVE FOR INSERT TO commerce_runtime
 WITH CHECK(namespace !~ '^(ecpay|buyer)\.');
CREATE POLICY pickup_heads_closed_namespace_insert ON fulfillment.pickup_heads AS RESTRICTIVE FOR INSERT TO commerce_runtime
 WITH CHECK(namespace !~ '^(ecpay|buyer)\.');
CREATE POLICY pickup_heads_closed_namespace_update ON fulfillment.pickup_heads AS RESTRICTIVE FOR UPDATE TO commerce_runtime
 USING(namespace !~ '^(ecpay|buyer)\.') WITH CHECK(namespace !~ '^(ecpay|buyer)\.');
-- X9-3: 0012 scoped_read and 0013 checkout_runtime_read are store-wide; one buyer must never read another buyer's
-- entered store (it carries the owner id in its namespace). Both buyer roles, not only commerce_buyer_runtime.
CREATE POLICY buyer_entered_own ON fulfillment.pickup_versions AS RESTRICTIVE FOR SELECT
 TO commerce_buyer_runtime,commerce_checkout_runtime
 USING(namespace !~ '^buyer\.' OR namespace='buyer.'||current_setting('app.buyer_id',true));
CREATE POLICY buyer_entered_own ON fulfillment.pickup_heads AS RESTRICTIVE FOR SELECT
 TO commerce_buyer_runtime,commerce_checkout_runtime
 USING(namespace !~ '^buyer\.' OR namespace='buyer.'||current_setting('app.buyer_id',true));

-- ---------------------------------------------------------------------------------------------------
-- checkout.orders: payment mode + collection state (§16.2, §16.4, §16.8).
-- ---------------------------------------------------------------------------------------------------
ALTER TABLE checkout.orders
 ADD COLUMN payment_mode text NOT NULL DEFAULT 'card' CHECK(payment_mode IN ('card','pay_at_pickup')),
 ADD COLUMN collection_state text CHECK(collection_state IN ('PENDING','COLLECTED','RETURNED','REFUNDED_OFFLINE','CANCELLED','RESTOCKED'));
ALTER TABLE checkout.orders ADD CONSTRAINT orders_payment_collection
 CHECK((payment_mode='pay_at_pickup')=(collection_state IS NOT NULL));
-- R4-3: the per-store and per-owner open pay-at-pickup count (begin_hold) scans exactly this predicate.
CREATE INDEX orders_pay_at_pickup_open ON checkout.orders(tenant_id,store_id,owner_id)
 WHERE payment_mode='pay_at_pickup' AND collection_state='PENDING' AND fulfillment_state='MANUAL_UNASSIGNED';

-- ---------------------------------------------------------------------------------------------------
-- inventory.ledger: DEALLOCATE kind, BUYER ALLOCATE (pay-at-pickup commit) and MERCHANT DEALLOCATE branches.
-- ---------------------------------------------------------------------------------------------------
DO $$
DECLARE v_def text; v_anchor text;
BEGIN
 -- Both constraints are extended by inserting whole OR-branches before a known anchor, so no closing-paren
 -- arithmetic on the live text is needed.
 SELECT pg_get_constraintdef(c.oid) INTO v_def FROM pg_constraint c
  WHERE c.conrelid='inventory.ledger'::regclass AND c.conname='ledger_check';
 v_anchor:=' OR ((kind = ''ALLOCATE''::text)';
 IF v_def IS NULL OR v_def LIKE '%DEALLOCATE%'
  OR length(v_def)-length(replace(v_def,v_anchor,''))<>length(v_anchor) THEN
  RAISE EXCEPTION 'inventory.ledger ledger_check has an unexpected shape: %',v_def; END IF;
 v_def:=replace(v_def,v_anchor,
  ' OR ((kind = ''DEALLOCATE''::text) AND (delta_on_hand = 0) AND (delta_reserved = 0) AND (delta_allocated < 0)'||
  ' AND (reservation_id IS NOT NULL))'||v_anchor);
 ALTER TABLE inventory.ledger DROP CONSTRAINT ledger_check;
 EXECUTE format('ALTER TABLE inventory.ledger ADD CONSTRAINT ledger_check %s',v_def);

 SELECT pg_get_constraintdef(c.oid) INTO v_def FROM pg_constraint c
  WHERE c.conrelid='inventory.ledger'::regclass AND c.conname='ledger_checkout_actor';
 v_anchor:=' OR ((actor_kind = ''SYSTEM_PAYMENT''::text)';
 IF v_def IS NULL OR v_def LIKE '%DEALLOCATE%'
  OR length(v_def)-length(replace(v_def,v_anchor,''))<>length(v_anchor) THEN
  RAISE EXCEPTION 'inventory.ledger ledger_checkout_actor has an unexpected shape: %',v_def; END IF;
 v_def:=replace(v_def,v_anchor,
  -- §16.2 / X9-5: the BUYER ALLOCATE branch carries the same non-NULL checkout/buyer columns as every other
  -- non-MERCHANT branch, so a NULL checkout_id cannot pass the CHECK as NULL.
  ' OR ((actor_kind = ''BUYER''::text) AND (principal_id IS NULL) AND (kind = ''ALLOCATE''::text)'||
  ' AND (checkout_id IS NOT NULL) AND (buyer_owner_id IS NOT NULL) AND (buyer_session_id IS NOT NULL)'||
  ' AND (reservation_id = checkout_id) AND (payment_attempt_id IS NULL) AND (payment_fact_kind IS NULL)'||
  ' AND (operation = ''checkout.pay_at_pickup.commit''::text) AND (command_key = (checkout_id)::text))'||
  -- §16.8: the only MERCHANT row that carries checkout columns; the existing MERCHANT branch keeps NULLs.
  ' OR ((actor_kind = ''MERCHANT''::text) AND (kind = ''DEALLOCATE''::text) AND (principal_id IS NOT NULL)'||
  ' AND (checkout_id IS NOT NULL) AND (buyer_owner_id IS NOT NULL) AND (buyer_session_id IS NOT NULL)'||
  ' AND (reservation_id = checkout_id) AND (payment_attempt_id IS NULL) AND (payment_fact_kind IS NULL)'||
  ' AND (operation = ANY (ARRAY[''fulfillment.pay_at_pickup.cancel''::text, ''fulfillment.pay_at_pickup.restock''::text]))'||
  ' AND (command_key = (checkout_id)::text))'||v_anchor);
 ALTER TABLE inventory.ledger DROP CONSTRAINT ledger_checkout_actor;
 EXECUTE format('ALTER TABLE inventory.ledger ADD CONSTRAINT ledger_checkout_actor %s',v_def);
END $$;
-- One release per order line, ever (a replay with a new key cannot double-release, §16.8).
CREATE UNIQUE INDEX ledger_pay_at_pickup_release_once ON inventory.ledger(tenant_id,store_id,checkout_id,warehouse_id,sku_id)
 WHERE kind='DEALLOCATE';

-- 0013 guard_checkout_ledger, re-derived from 0013:129-158 with one added branch (§16.8): a MERCHANT DEALLOCATE row
-- must carry the definer's principal and the buyer GUCs of the order it releases. Without this branch 0013:134-137
-- refuses every DEALLOCATE row. Every other MERCHANT row keeps the NULL checkout/buyer rule.
CREATE OR REPLACE FUNCTION inventory.guard_checkout_ledger() RETURNS trigger
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_checkout uuid; v_owner uuid; v_session uuid;
BEGIN
 IF NEW.actor_kind='MERCHANT' AND NEW.kind='DEALLOCATE' THEN
  IF NEW.principal_id IS DISTINCT FROM nullif(current_setting('app.principal_id',true),'')::uuid
     OR NEW.checkout_id IS DISTINCT FROM NEW.reservation_id
     OR NEW.buyer_owner_id IS DISTINCT FROM nullif(current_setting('app.buyer_id',true),'')::uuid
     OR NEW.buyer_session_id IS DISTINCT FROM nullif(current_setting('app.buyer_session_id',true),'')::uuid THEN
   RAISE EXCEPTION 'ledger actor mismatch' USING ERRCODE='42501'; END IF;
 ELSIF NEW.actor_kind='MERCHANT' THEN
  IF NEW.principal_id IS DISTINCT FROM nullif(current_setting('app.principal_id',true),'')::uuid
     OR NEW.checkout_id IS NOT NULL OR NEW.buyer_owner_id IS NOT NULL OR NEW.buyer_session_id IS NOT NULL THEN
   RAISE EXCEPTION 'ledger actor mismatch' USING ERRCODE='42501'; END IF;
 ELSE
  IF NEW.principal_id IS NOT NULL OR current_setting('app.principal_id',true)<>''
     OR NEW.buyer_owner_id IS DISTINCT FROM nullif(current_setting('app.buyer_id',true),'')::uuid
     OR NEW.buyer_session_id IS DISTINCT FROM nullif(current_setting('app.buyer_session_id',true),'')::uuid
     OR NEW.checkout_id IS DISTINCT FROM NEW.reservation_id THEN
   RAISE EXCEPTION 'ledger actor mismatch' USING ERRCODE='42501'; END IF;
 END IF;
 IF NEW.reservation_id IS NOT NULL THEN
  SELECT r.checkout_id,r.buyer_owner_id,r.buyer_session_id INTO v_checkout,v_owner,v_session
   FROM inventory.reservations r WHERE r.tenant_id=NEW.tenant_id AND r.store_id=NEW.store_id
    AND r.id=NEW.reservation_id;
  IF NOT FOUND OR v_checkout IS DISTINCT FROM NEW.checkout_id
     OR v_owner IS DISTINCT FROM NEW.buyer_owner_id OR v_session IS DISTINCT FROM NEW.buyer_session_id THEN
   RAISE EXCEPTION 'ledger reservation mismatch' USING ERRCODE='42501'; END IF;
 END IF;
 RETURN NEW;
END $$;
ALTER FUNCTION inventory.guard_checkout_ledger() OWNER TO commerce_inventory_writer;
REVOKE ALL ON FUNCTION inventory.guard_checkout_ledger() FROM PUBLIC;

-- §16.2 + §16.8 ledger provenance (mirror of inventory.guard_payment_ledger, 0061:157): the only BUYER ALLOCATE is
-- the pay-at-pickup commit of an order that is a pay_at_pickup order, for exactly the reserved quantity; the only
-- MERCHANT DEALLOCATE releases a cancelled/restocked pay_at_pickup order's allocation for exactly what was allocated.
CREATE FUNCTION inventory.guard_pay_at_pickup_ledger() RETURNS trigger
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 IF NEW.actor_kind='BUYER' AND NEW.kind='ALLOCATE' THEN
  IF NOT EXISTS(SELECT 1 FROM checkout.orders o WHERE o.tenant_id=NEW.tenant_id AND o.store_id=NEW.store_id
    AND o.owner_id=NEW.buyer_owner_id AND o.id=NEW.checkout_id AND o.creator_session_id=NEW.buyer_session_id
    AND o.payment_mode='pay_at_pickup' AND o.collection_state='PENDING' AND o.commercial_state='CONFIRMED')
   OR NOT EXISTS(SELECT 1 FROM inventory.reservation_lines l WHERE l.tenant_id=NEW.tenant_id
    AND l.store_id=NEW.store_id AND l.reservation_id=NEW.reservation_id AND l.warehouse_id=NEW.warehouse_id
    AND l.sku_id=NEW.sku_id AND l.quantity=-NEW.delta_reserved)
   OR EXISTS(SELECT 1 FROM checkout.payment_attempts a WHERE a.tenant_id=NEW.tenant_id AND a.store_id=NEW.store_id
    AND a.order_id=NEW.checkout_id) THEN
   RAISE EXCEPTION 'pay-at-pickup ledger mismatch' USING ERRCODE='42501'; END IF;
 ELSIF NEW.actor_kind='MERCHANT' AND NEW.kind='DEALLOCATE' THEN
  IF NOT EXISTS(SELECT 1 FROM checkout.orders o WHERE o.tenant_id=NEW.tenant_id AND o.store_id=NEW.store_id
    AND o.owner_id=NEW.buyer_owner_id AND o.id=NEW.checkout_id AND o.payment_mode='pay_at_pickup'
    AND ((NEW.operation='fulfillment.pay_at_pickup.cancel' AND o.collection_state='CANCELLED')
      OR (NEW.operation='fulfillment.pay_at_pickup.restock' AND o.collection_state='RESTOCKED')))
   OR NOT EXISTS(SELECT 1 FROM inventory.reservations r WHERE r.tenant_id=NEW.tenant_id AND r.store_id=NEW.store_id
    AND r.id=NEW.reservation_id AND r.state='RELEASED')
   OR NOT EXISTS(SELECT 1 FROM inventory.ledger a WHERE a.tenant_id=NEW.tenant_id AND a.store_id=NEW.store_id
    AND a.checkout_id=NEW.checkout_id AND a.warehouse_id=NEW.warehouse_id AND a.sku_id=NEW.sku_id
    AND a.kind='ALLOCATE' AND a.actor_kind='BUYER' AND a.delta_allocated=-NEW.delta_allocated)
   OR NOT EXISTS(SELECT 1 FROM inventory.reservation_lines l WHERE l.tenant_id=NEW.tenant_id
    AND l.store_id=NEW.store_id AND l.reservation_id=NEW.reservation_id AND l.warehouse_id=NEW.warehouse_id
    AND l.sku_id=NEW.sku_id AND l.quantity=-NEW.delta_allocated) THEN
   RAISE EXCEPTION 'pay-at-pickup release mismatch' USING ERRCODE='42501'; END IF;
 END IF;
 RETURN NEW;
END $$;
ALTER FUNCTION inventory.guard_pay_at_pickup_ledger() OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION inventory.guard_pay_at_pickup_ledger() FROM PUBLIC;
CREATE TRIGGER zz_pay_at_pickup_ledger_guard BEFORE INSERT ON inventory.ledger
 FOR EACH ROW EXECUTE FUNCTION inventory.guard_pay_at_pickup_ledger();

-- ---------------------------------------------------------------------------------------------------
-- integration.merchant_accounts: ecpay_logistics (TD2, §4.1). Merchant runtime policies (0061) stay provider='payuni'.
-- ---------------------------------------------------------------------------------------------------
ALTER TABLE integration.merchant_accounts ADD CONSTRAINT ecpay_logistics_account_id_check
 CHECK(provider<>'ecpay_logistics' OR account_id ~ '^[A-Za-z0-9]{1,10}$');
CREATE UNIQUE INDEX ecpay_one_account_per_store_environment ON integration.merchant_accounts(tenant_id,store_id,environment)
 WHERE provider='ecpay_logistics';
-- Same rule as 0061 stripe_account_identity_unique: one ECPay MerchantID serves one store (per environment).
CREATE UNIQUE INDEX ecpay_logistics_identity_unique ON integration.merchant_accounts(environment,account_id)
 WHERE provider='ecpay_logistics';
-- Provider-scoped key for the profile FK (the profile must describe an ecpay_logistics account, never PAYUNi/Stripe).
ALTER TABLE integration.merchant_accounts ADD CONSTRAINT account_ecpay_profile_target_unique
 UNIQUE(tenant_id,store_id,id,provider);

-- operation_actor_family already admits any MERCHANT provider/action; CVS create is narrowed by its own CHECK (round 1, #12).
ALTER TABLE integration.operations ADD CONSTRAINT cvs_create_family
 CHECK(provider<>'ecpay_logistics' OR (action='ecpay.cvs_create' AND purpose='transactional' AND actor_kind='MERCHANT'));

-- ---------------------------------------------------------------------------------------------------
-- §4.2 tables.
-- ---------------------------------------------------------------------------------------------------
CREATE TABLE integration.ecpay_logistics_profiles (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, connection_id uuid NOT NULL,
 provider text NOT NULL DEFAULT 'ecpay_logistics' CHECK(provider='ecpay_logistics'),
 mode text NOT NULL CHECK(mode IN ('C2C','B2C')),
 endpoint_id uuid NOT NULL UNIQUE,
 enabled boolean NOT NULL,
 qualified_credential_version bigint,
 qualified_at timestamptz, ok_verified boolean NOT NULL DEFAULT false,
 hilife_verified boolean NOT NULL DEFAULT false,
 version bigint NOT NULL CHECK(version>0), updated_at timestamptz NOT NULL,
 PRIMARY KEY(tenant_id,store_id,connection_id),
 CHECK((qualified_credential_version IS NULL)=(qualified_at IS NULL)),
 FOREIGN KEY(tenant_id,store_id,connection_id,provider) REFERENCES integration.merchant_accounts(tenant_id,store_id,id,provider)
);
CREATE UNIQUE INDEX ecpay_one_enabled_profile ON integration.ecpay_logistics_profiles(tenant_id,store_id) WHERE enabled;

CREATE TABLE fulfillment.cvs_selections (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, owner_id uuid NOT NULL, id uuid NOT NULL,
 session_id uuid NOT NULL, cart_id uuid NOT NULL, cart_version bigint NOT NULL CHECK(cart_version>0),
 kind text NOT NULL CHECK(kind IN ('cvs_711','cvs_familymart','cvs_hilife','cvs_okmart')),
 connection_id uuid NOT NULL, credential_version bigint NOT NULL, logistics_subtype text NOT NULL
  CHECK(logistics_subtype IN ('UNIMARTC2C','FAMIC2C','HILIFEC2C','OKMARTC2C','UNIMART','FAMI','HILIFE')),
 nonce_sha256 bytea NOT NULL UNIQUE CHECK(octet_length(nonce_sha256)=32),
 state text NOT NULL CHECK(state IN ('OPEN','RETURNED','VERIFIED','REJECTED','EXPIRED')),
 returned_store_id text CHECK(returned_store_id ~ '^[A-Za-z0-9]{1,9}$'),
 returned_outside boolean, reject_code text CHECK(reject_code ~ '^[a-z_]{1,40}$'),
 pickup_id uuid, country text NOT NULL DEFAULT 'TW' CHECK(country='TW'),
 return_origin text NOT NULL,
 return_path text NOT NULL CHECK(return_path ~ '^/(zh-TW|zh-CN|en)/products/[A-Za-z0-9_-]{1,64}$'),
 created_at timestamptz NOT NULL, expires_at timestamptz NOT NULL,
 updated_at timestamptz NOT NULL, version bigint NOT NULL CHECK(version>0),
 PRIMARY KEY(tenant_id,store_id,owner_id,id), UNIQUE(id),
 CHECK(expires_at>created_at AND expires_at<=created_at+interval '15 minutes'),
 CHECK((state='VERIFIED')=(pickup_id IS NOT NULL)), CHECK((state='REJECTED')=(reject_code IS NOT NULL)),
 FOREIGN KEY(tenant_id,store_id,owner_id,cart_id) REFERENCES storefront.carts(tenant_id,store_id,owner_id,id),
 FOREIGN KEY(tenant_id,store_id,owner_id,session_id) REFERENCES buyer.capability_sessions(tenant_id,store_id,owner_id,id),
 FOREIGN KEY(tenant_id,store_id,connection_id) REFERENCES integration.ecpay_logistics_profiles(tenant_id,store_id,connection_id),
 FOREIGN KEY(tenant_id,store_id,pickup_id,kind,country) REFERENCES fulfillment.pickup_versions(tenant_id,store_id,id,kind,country)
);
CREATE INDEX cvs_selections_open ON fulfillment.cvs_selections(tenant_id,store_id,owner_id,cart_id) WHERE state='OPEN';

CREATE TABLE fulfillment.cvs_shipments (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, owner_id uuid NOT NULL, order_id uuid NOT NULL,
 attempt smallint NOT NULL CHECK(attempt BETWEEN 1 AND 5),
 connection_id uuid NOT NULL, credential_version bigint NOT NULL CHECK(credential_version>0),
 environment text NOT NULL CHECK(environment IN ('SANDBOX','LIVE')),
 logistics_subtype text NOT NULL CHECK(logistics_subtype IN ('UNIMARTC2C','FAMIC2C','HILIFEC2C','OKMARTC2C','UNIMART','FAMI','HILIFE')),
 receiver_store_id text NOT NULL CHECK(receiver_store_id ~ '^[A-Za-z0-9]{1,6}$'),
 pickup_id uuid NOT NULL, goods_amount integer NOT NULL CHECK(goods_amount BETWEEN 1 AND 20000),
 collection_amount integer CHECK(collection_amount BETWEEN 1 AND 20000),
 merchant_trade_no text NOT NULL CHECK(merchant_trade_no ~ '^LC[A-Z2-7]{18}$'),
 operation_id uuid NOT NULL UNIQUE,
 state text NOT NULL CHECK(state IN ('REQUESTED','CREATED','FAILED','UNKNOWN','AT_DC','AT_STORE','PICKED_UP','UNCLAIMED','ABANDONED')),
 provider_logistics_id text CHECK(provider_logistics_id ~ '^[0-9A-Za-z_-]{1,40}$'),
 cvs_payment_no text CHECK(cvs_payment_no ~ '^[0-9A-Za-z_-]{1,40}$'),
 cvs_validation_no text CHECK(cvs_validation_no ~ '^[0-9A-Za-z_-]{1,40}$'),
 shipment_no text CHECK(shipment_no ~ '^[0-9A-Za-z_-]{1,40}$'),
 last_status_code text CHECK(last_status_code ~ '^[0-9]{1,8}$'), last_status_at timestamptz,
 result_code text CHECK(result_code ~ '^[a-z0-9_.]{1,80}$'),
 principal_id uuid NOT NULL, created_at timestamptz NOT NULL, updated_at timestamptz NOT NULL,
 version bigint NOT NULL CHECK(version>0),
 PRIMARY KEY(tenant_id,store_id,order_id,attempt),
 UNIQUE(connection_id,merchant_trade_no),
 CHECK(state NOT IN ('CREATED','AT_DC','AT_STORE','PICKED_UP','UNCLAIMED') OR provider_logistics_id IS NOT NULL),
 CHECK(collection_amount IS NULL OR collection_amount=goods_amount),
 FOREIGN KEY(tenant_id,store_id,owner_id,order_id) REFERENCES checkout.orders(tenant_id,store_id,owner_id,id),
 FOREIGN KEY(tenant_id,store_id,connection_id) REFERENCES integration.ecpay_logistics_profiles(tenant_id,store_id,connection_id),
 FOREIGN KEY(tenant_id,store_id,operation_id) REFERENCES integration.operations(tenant_id,store_id,id),
 FOREIGN KEY(tenant_id,principal_id) REFERENCES identity.memberships(tenant_id,principal_id)
);
CREATE UNIQUE INDEX cvs_shipments_one_live ON fulfillment.cvs_shipments(tenant_id,store_id,order_id)
 WHERE state NOT IN ('FAILED','ABANDONED');

CREATE TABLE fulfillment.cvs_shipment_events (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, order_id uuid NOT NULL, attempt smallint NOT NULL,
 id uuid NOT NULL DEFAULT gen_random_uuid(),
 source text NOT NULL CHECK(source IN ('local','ecpay_status','ecpay_query')),
 event_code text CHECK(event_code ~ '^[a-z0-9_.]{1,80}$'),
 body_sha256 bytea CHECK(body_sha256 IS NULL OR octet_length(body_sha256)=32),
 provider_code text CHECK(provider_code ~ '^[0-9]{1,8}$'),
 provider_message text CHECK(char_length(provider_message)<=200 AND provider_message !~ '[[:cntrl:]]'),
 provider_updated_at text CHECK(provider_updated_at ~ '^[0-9]{4}/[0-9]{2}/[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}$'),
 from_state text, to_state text, received_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,store_id,order_id,attempt,id),
 UNIQUE(tenant_id,store_id,order_id,attempt,body_sha256),
 FOREIGN KEY(tenant_id,store_id,order_id,attempt) REFERENCES fulfillment.cvs_shipments(tenant_id,store_id,order_id,attempt)
);

CREATE TABLE fulfillment.cvs_store_settings (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL,
 enabled_chains text[] NOT NULL DEFAULT '{cvs_711,cvs_familymart,cvs_hilife,cvs_okmart}'
  CHECK(enabled_chains <@ ARRAY['cvs_711','cvs_familymart','cvs_hilife','cvs_okmart']::text[]),
 pay_at_pickup_enabled boolean NOT NULL DEFAULT false,
 pay_at_pickup_max_twd integer CHECK(pay_at_pickup_max_twd BETWEEN 1 AND 20000),
 pay_at_pickup_max_open integer NOT NULL DEFAULT 20 CHECK(pay_at_pickup_max_open BETWEEN 1 AND 500),
 version bigint NOT NULL CHECK(version>0), updated_at timestamptz NOT NULL,
 PRIMARY KEY(tenant_id,store_id),
 CHECK(NOT pay_at_pickup_enabled OR pay_at_pickup_max_twd IS NOT NULL),
 FOREIGN KEY(tenant_id,store_id) REFERENCES control.stores(tenant_id,id)
);

DO $$ DECLARE r text; BEGIN
 FOREACH r IN ARRAY ARRAY['integration.ecpay_logistics_profiles','fulfillment.cvs_selections','fulfillment.cvs_shipments',
  'fulfillment.cvs_shipment_events','fulfillment.cvs_store_settings'] LOOP
  EXECUTE format('ALTER TABLE %s ENABLE ROW LEVEL SECURITY',r);
  EXECUTE format('ALTER TABLE %s FORCE ROW LEVEL SECURITY',r);
  EXECUTE format('REVOKE ALL ON %s FROM PUBLIC',r);
 END LOOP;
END $$;

-- ---------------------------------------------------------------------------------------------------
-- Trigger guards.
-- ---------------------------------------------------------------------------------------------------
-- cvs_shipments: identity columns are immutable, every change is exactly version+1, rows are never deleted (§4.2).
CREATE FUNCTION fulfillment.guard_cvs_shipment_update() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF TG_OP='DELETE' OR TG_OP='TRUNCATE' THEN
  RAISE EXCEPTION 'cvs shipments are never deleted' USING ERRCODE='42501'; END IF;
 IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id OR NEW.store_id IS DISTINCT FROM OLD.store_id
  OR NEW.owner_id IS DISTINCT FROM OLD.owner_id OR NEW.order_id IS DISTINCT FROM OLD.order_id
  OR NEW.attempt IS DISTINCT FROM OLD.attempt OR NEW.connection_id IS DISTINCT FROM OLD.connection_id
  OR NEW.credential_version IS DISTINCT FROM OLD.credential_version OR NEW.environment IS DISTINCT FROM OLD.environment
  OR NEW.logistics_subtype IS DISTINCT FROM OLD.logistics_subtype OR NEW.receiver_store_id IS DISTINCT FROM OLD.receiver_store_id
  OR NEW.pickup_id IS DISTINCT FROM OLD.pickup_id OR NEW.goods_amount IS DISTINCT FROM OLD.goods_amount
  OR NEW.collection_amount IS DISTINCT FROM OLD.collection_amount OR NEW.merchant_trade_no IS DISTINCT FROM OLD.merchant_trade_no
  OR NEW.operation_id IS DISTINCT FROM OLD.operation_id OR NEW.principal_id IS DISTINCT FROM OLD.principal_id
  OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
  RAISE EXCEPTION 'cvs shipment identity is immutable' USING ERRCODE='42501'; END IF;
 IF NEW.version<>OLD.version+1 THEN
  RAISE EXCEPTION 'cvs shipment version must advance by exactly one' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
REVOKE ALL ON FUNCTION fulfillment.guard_cvs_shipment_update() FROM PUBLIC;
CREATE TRIGGER cvs_shipments_guard BEFORE UPDATE OR DELETE ON fulfillment.cvs_shipments
 FOR EACH ROW EXECUTE FUNCTION fulfillment.guard_cvs_shipment_update();
CREATE TRIGGER cvs_shipments_no_truncate BEFORE TRUNCATE ON fulfillment.cvs_shipments
 FOR EACH STATEMENT EXECUTE FUNCTION fulfillment.guard_cvs_shipment_update();

CREATE FUNCTION fulfillment.guard_cvs_event_immutable() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 RAISE EXCEPTION 'cvs shipment events are append-only' USING ERRCODE='42501';
END $$;
REVOKE ALL ON FUNCTION fulfillment.guard_cvs_event_immutable() FROM PUBLIC;
CREATE TRIGGER cvs_events_append_only BEFORE UPDATE OR DELETE ON fulfillment.cvs_shipment_events
 FOR EACH ROW EXECUTE FUNCTION fulfillment.guard_cvs_event_immutable();
CREATE TRIGGER cvs_events_no_truncate BEFORE TRUNCATE ON fulfillment.cvs_shipment_events
 FOR EACH STATEMENT EXECUTE FUNCTION fulfillment.guard_cvs_event_immutable();

-- Deferred: order.fulfillment_state = PROVIDER_LABEL_CREATED <=> a live shipment in CREATED..UNCLAIMED (§4.1, I13).
-- DEFINER (checkout_writer) so the check sees both tables whoever wrote either of them.
CREATE FUNCTION fulfillment.guard_cvs_shipment_state() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_order uuid; v_state text; v_labelled boolean;
BEGIN
 IF TG_TABLE_NAME='orders' THEN v_order:=NEW.id; ELSE v_order:=NEW.order_id; END IF;
 SELECT o.fulfillment_state INTO v_state FROM checkout.orders o
  WHERE o.tenant_id=NEW.tenant_id AND o.store_id=NEW.store_id AND o.id=v_order;
 IF NOT FOUND THEN RETURN NULL; END IF;
 SELECT EXISTS(SELECT 1 FROM fulfillment.cvs_shipments c WHERE c.tenant_id=NEW.tenant_id AND c.store_id=NEW.store_id
   AND c.order_id=v_order AND c.state IN ('CREATED','AT_DC','AT_STORE','PICKED_UP','UNCLAIMED')) INTO v_labelled;
 IF (v_state='PROVIDER_LABEL_CREATED')<>v_labelled THEN
  RAISE EXCEPTION 'order fulfillment state and cvs shipment disagree' USING ERRCODE='23514'; END IF;
 RETURN NULL;
END $$;
ALTER FUNCTION fulfillment.guard_cvs_shipment_state() OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION fulfillment.guard_cvs_shipment_state() FROM PUBLIC;
CREATE CONSTRAINT TRIGGER cvs_shipment_state_orders AFTER UPDATE OF fulfillment_state ON checkout.orders
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
 WHEN (OLD.fulfillment_state='PROVIDER_LABEL_CREATED' OR NEW.fulfillment_state='PROVIDER_LABEL_CREATED')
 EXECUTE FUNCTION fulfillment.guard_cvs_shipment_state();
CREATE CONSTRAINT TRIGGER cvs_shipment_state_rows AFTER INSERT OR UPDATE ON fulfillment.cvs_shipments
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
 EXECUTE FUNCTION fulfillment.guard_cvs_shipment_state();

-- API service <=> qualified same-store ecpay_logistics profile (R-2, §4.1): the old "API cannot be enabled" CHECK
-- is replaced by "an enabled API row needs a binding" plus this trigger. DEFINER owned by the integration writer,
-- whose worker read policies already see merchant_accounts and the new profile table.
DO $$
DECLARE v_name text; v_def text;
BEGIN
 SELECT c.conname,pg_get_constraintdef(c.oid) INTO v_name,v_def FROM pg_constraint c
  WHERE c.conrelid='fulfillment.service_versions'::regclass AND c.contype='c'
   AND pg_get_constraintdef(c.oid)='CHECK (((mode <> ''API''::text) OR (NOT enabled)))';
 IF v_name IS NULL THEN RAISE EXCEPTION 'service_versions API-disabled CHECK not found'; END IF;
 EXECUTE format('ALTER TABLE fulfillment.service_versions DROP CONSTRAINT %I',v_name);
END $$;
ALTER TABLE fulfillment.service_versions ADD CONSTRAINT service_versions_api_needs_binding
 CHECK(mode<>'API' OR NOT enabled OR binding_id IS NOT NULL);

CREATE FUNCTION fulfillment.guard_api_service_binding() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE p record;
BEGIN
 IF NEW.mode<>'API' OR NOT NEW.enabled THEN RETURN NULL; END IF;
 SELECT a.id,a.credential_version,pr.enabled,pr.qualified_credential_version,pr.ok_verified,pr.hilife_verified
  INTO p FROM integration.merchant_accounts a
  JOIN integration.ecpay_logistics_profiles pr ON pr.tenant_id=a.tenant_id AND pr.store_id=a.store_id AND pr.connection_id=a.id
  WHERE a.tenant_id=NEW.tenant_id AND a.store_id=NEW.store_id AND a.binding_id=NEW.binding_id
   AND a.provider='ecpay_logistics';
 IF NOT FOUND OR NOT p.enabled OR p.qualified_credential_version IS DISTINCT FROM p.credential_version
  OR NEW.delivery_kind NOT IN ('cvs_711','cvs_familymart','cvs_hilife','cvs_okmart')
  OR (NEW.delivery_kind='cvs_okmart' AND NOT p.ok_verified)
  OR (NEW.delivery_kind='cvs_hilife' AND NOT p.hilife_verified) THEN
  RAISE EXCEPTION 'API delivery service needs a qualified enabled ecpay_logistics profile of the same store'
   USING ERRCODE='23514'; END IF;
 RETURN NULL;
END $$;
ALTER FUNCTION fulfillment.guard_api_service_binding() OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION fulfillment.guard_api_service_binding() FROM PUBLIC;
CREATE CONSTRAINT TRIGGER service_versions_api_binding AFTER INSERT ON fulfillment.service_versions
 DEFERRABLE INITIALLY IMMEDIATE FOR EACH ROW
 EXECUTE FUNCTION fulfillment.guard_api_service_binding();

-- ---------------------------------------------------------------------------------------------------
-- Documentation (PROCESS §5): owning package, allowed roles, non-goals.
-- ---------------------------------------------------------------------------------------------------
COMMENT ON TABLE integration.ecpay_logistics_profiles IS
 'internal/fulfillment (CVS): one ECPay logistics profile per ecpay_logistics account (mode C2C|B2C, status endpoint id, qualification of a credential version). Written only by integration.register_ecpay_logistics / set_ecpay_logistics_enabled (owner commerce_integration_writer); read by commerce_checkout_writer and commerce_integration_writer definers. No grant to commerce_runtime, commerce_checkout_runtime or any buyer role. Holds no credential and no PII.';
COMMENT ON TABLE fulfillment.cvs_selections IS
 'internal/fulfillment + internal/checkout (CVS): one buyer ECPay e-map round-trip (OPEN -> RETURNED -> VERIFIED|REJECTED|EXPIRED). Written only by open_cvs_selection / record_cvs_map_return / verify_cvs_selection (owner commerce_checkout_writer). Stores only the sha256 of the map nonce. Non-goal: never a source of store name/address (the directory row is).';
COMMENT ON TABLE fulfillment.cvs_shipments IS
 'internal/fulfillment (CVS): at most one live ECPay shipment per order (cvs_shipments_one_live), attempts 1..5. Mutable only through the 0073 definers with version+1 CAS (guard_cvs_shipment_update); identity columns immutable. Contains no recipient name, phone or address (TD8) and no credential.';
COMMENT ON TABLE fulfillment.cvs_shipment_events IS
 'internal/fulfillment (CVS): append-only shipment history (local transitions, ECPay status receipts, query results). Duplicate provider notification bodies collapse on (attempt, body_sha256). Never stores a status body, recipient fields or raw provider values outside the CHECK-bounded columns.';
COMMENT ON TABLE fulfillment.cvs_store_settings IS
 'internal/fulfillment (CVS, C4): per-store enabled chains and pay-at-pickup limits. Written only by fulfillment.set_cvs_store_settings (integration:manage); read by begin_hold, read_cvs_offer, record_buyer_cvs_store. No grant to runtime or buyer roles. A missing row means defaults (pay-at-pickup off).';
COMMENT ON COLUMN checkout.orders.payment_mode IS 'internal/checkout: card (Stripe) or pay_at_pickup (§16.2, no payment attempt, platform never holds the money). Set once by begin_hold.';
COMMENT ON COLUMN checkout.orders.collection_state IS 'internal/fulfillment: pay_at_pickup collection lifecycle (PENDING -> COLLECTED|RETURNED|CANCELLED, COLLECTED -> REFUNDED_OFFLINE, RETURNED -> RESTOCKED); NULL for card orders. Written only by ingest_ecpay_status, record_collection and inventory.release_pay_at_pickup.';
COMMENT ON COLUMN fulfillment.pickup_versions.principal_id IS 'Attesting membership; NULL exactly for BUYER_ENTERED rows (a buyer has no membership). PROVIDER_DIRECTORY_VERIFIED rows carry the connection principal (R-3).';
COMMENT ON COLUMN integration.ecpay_logistics_profiles.endpoint_id IS 'Random id that names this profile in the public status URL /v1/cvs/ecpay/status/{endpoint_id}; a route key, not a secret (the MAC is).';
COMMENT ON COLUMN integration.ecpay_logistics_profiles.qualified_credential_version IS 'Credential version whose GetStoreList probe passed (register_ecpay_logistics probe_ok); an enabled profile is usable only while it equals merchant_accounts.credential_version.';
COMMENT ON COLUMN integration.ecpay_logistics_profiles.ok_verified IS 'Set only from TCV10 evidence by an operator (no Go writer): OK mart directory + map proven. Until then OK mart stays coming_soon.';
COMMENT ON COLUMN integration.ecpay_logistics_profiles.hilife_verified IS 'Set only from TCV07/TCV12 evidence by an operator (F17): a Hi-Life create accepted with a directory StoreId.';
COMMENT ON COLUMN fulfillment.cvs_selections.nonce_sha256 IS 'sha256 of the 20-char map MerchantTradeNo generated in Go; the nonce itself is never stored (whoever knows only selection_id cannot advance the selection).';
COMMENT ON COLUMN fulfillment.cvs_selections.return_origin IS 'The store''s ACTIVE storefront_domains origin authenticated at open time; the map-return 303 goes only here (never taken from the map-return body).';
COMMENT ON COLUMN fulfillment.cvs_shipments.merchant_trade_no IS 'LC + base32(sha256(operation id))[:18]; frozen at request time, the provider idempotency key (I06); never logged.';
COMMENT ON COLUMN fulfillment.cvs_shipments.collection_amount IS 'Pay-at-pickup only: frozen order total in whole TWD, equal to goods_amount (F20); NULL for card orders. I05: server-computed from orders.total_minor, never client-supplied.';
COMMENT ON COLUMN fulfillment.cvs_shipments.result_code IS 'Bounded provider-neutral result code (ecpay.*); never a raw provider message.';
-- PROCESS §5 column comments (TestTaiwanCvsSchemaColumnComments): every 0072 column says what it holds and who writes it.
COMMENT ON COLUMN integration.ecpay_logistics_profiles.tenant_id IS 'Tenant scope (server-side auth, RLS key); never from a request body.';
COMMENT ON COLUMN integration.ecpay_logistics_profiles.store_id IS 'Store scope (server-side auth, RLS key); never from a request body.';
COMMENT ON COLUMN integration.ecpay_logistics_profiles.connection_id IS 'integration.merchant_accounts id of the store''s ECPay logistics connection (provider ecpay_logistics).';
COMMENT ON COLUMN integration.ecpay_logistics_profiles.provider IS 'Always ecpay_logistics; part of the FK to merchant_accounts so a profile can never point at another provider''s connection.';
COMMENT ON COLUMN integration.ecpay_logistics_profiles.mode IS 'ECPay contract type of the merchant account: C2C (store-to-store, R2 default) or B2C.';
COMMENT ON COLUMN integration.ecpay_logistics_profiles.enabled IS 'Merchant switch (register_ecpay_logistics); at most one enabled profile per store (ecpay_one_enabled_profile).';
COMMENT ON COLUMN integration.ecpay_logistics_profiles.qualified_at IS 'DB time the GetStoreList probe passed for qualified_credential_version; NULL together with it.';
COMMENT ON COLUMN integration.ecpay_logistics_profiles.version IS 'Optimistic-concurrency version; +1 per write.';
COMMENT ON COLUMN integration.ecpay_logistics_profiles.updated_at IS 'DB time of the last write.';
COMMENT ON COLUMN fulfillment.cvs_selections.tenant_id IS 'Tenant scope (server-side auth, RLS key); never from a request body.';
COMMENT ON COLUMN fulfillment.cvs_selections.store_id IS 'Store scope (server-side auth, RLS key); never from a request body.';
COMMENT ON COLUMN fulfillment.cvs_selections.owner_id IS 'Buyer owner (buyer.WithScope) that opened the selection; the only reader.';
COMMENT ON COLUMN fulfillment.cvs_selections.id IS 'Selection id; appears in the storefront URL and the map ServerReplyURL, so it is not a capability (the nonce is).';
COMMENT ON COLUMN fulfillment.cvs_selections.session_id IS 'buyer.capability_sessions id that opened the selection.';
COMMENT ON COLUMN fulfillment.cvs_selections.cart_id IS 'storefront.carts id the pickup store is chosen for.';
COMMENT ON COLUMN fulfillment.cvs_selections.cart_version IS 'Cart version at open; a later cart change makes the selection stale for checkout.';
COMMENT ON COLUMN fulfillment.cvs_selections.kind IS 'Delivery kind of the chosen service (cvs_711, cvs_familymart, cvs_hilife, cvs_okmart).';
COMMENT ON COLUMN fulfillment.cvs_selections.connection_id IS 'ECPay logistics profile (connection) whose MerchantID the map form used.';
COMMENT ON COLUMN fulfillment.cvs_selections.credential_version IS 'merchant_accounts.credential_version at open; verification refuses a rotated credential.';
COMMENT ON COLUMN fulfillment.cvs_selections.logistics_subtype IS 'ECPay LogisticsSubType derived from kind + profile mode (§4.3 open_cvs_selection); never from input.';
COMMENT ON COLUMN fulfillment.cvs_selections.state IS 'OPEN -> RETURNED -> VERIFIED, or REJECTED/EXPIRED (record_cvs_map_return, verify_cvs_selection).';
COMMENT ON COLUMN fulfillment.cvs_selections.returned_store_id IS 'CVSStoreID from the map return after the nonce check; a lookup key, never a trusted description (F3 S(9)).';
COMMENT ON COLUMN fulfillment.cvs_selections.returned_outside IS 'CVSOutSide from the map return (outlying island flag); NULL until RETURNED.';
COMMENT ON COLUMN fulfillment.cvs_selections.reject_code IS 'expired | merchant_mismatch | subtype_mismatch | bad_store_id | directory codes; set exactly when REJECTED.';
COMMENT ON COLUMN fulfillment.cvs_selections.pickup_id IS 'fulfillment.pickup_versions row created from the ECPay directory on VERIFIED; the only address source.';
COMMENT ON COLUMN fulfillment.cvs_selections.country IS 'Always TW (Taiwan CVS only).';
COMMENT ON COLUMN fulfillment.cvs_selections.return_path IS 'Allowlisted storefront product path the buyer returns to after the map (never read from the map-return body).';
COMMENT ON COLUMN fulfillment.cvs_selections.created_at IS 'DB time the selection was opened.';
COMMENT ON COLUMN fulfillment.cvs_selections.expires_at IS 'created_at + at most 15 minutes; expired selections are REJECTED expired.';
COMMENT ON COLUMN fulfillment.cvs_selections.updated_at IS 'DB time of the last write.';
COMMENT ON COLUMN fulfillment.cvs_selections.version IS 'Optimistic-concurrency version; +1 per write.';
COMMENT ON COLUMN fulfillment.cvs_shipments.tenant_id IS 'Tenant scope (server-side auth, RLS key); never from a request body.';
COMMENT ON COLUMN fulfillment.cvs_shipments.store_id IS 'Store scope (server-side auth, RLS key); never from a request body.';
COMMENT ON COLUMN fulfillment.cvs_shipments.owner_id IS 'Buyer owner of the order (FK to checkout.orders).';
COMMENT ON COLUMN fulfillment.cvs_shipments.order_id IS 'checkout.orders id this shipment attempt ships.';
COMMENT ON COLUMN fulfillment.cvs_shipments.attempt IS 'Attempt number 1..5 per order; FAILED/ABANDONED attempts stay for audit, one live attempt (cvs_shipments_one_live).';
COMMENT ON COLUMN fulfillment.cvs_shipments.connection_id IS 'ECPay logistics profile used for Create; frozen at request time.';
COMMENT ON COLUMN fulfillment.cvs_shipments.credential_version IS 'merchant_accounts.credential_version frozen at request time; a rotation makes the operation STALE_BINDING.';
COMMENT ON COLUMN fulfillment.cvs_shipments.environment IS 'SANDBOX or LIVE of the connection; LIVE Create additionally needs CVS_ECPAY_LIVE_CREATE.';
COMMENT ON COLUMN fulfillment.cvs_shipments.logistics_subtype IS 'ECPay LogisticsSubType frozen at request time.';
COMMENT ON COLUMN fulfillment.cvs_shipments.receiver_store_id IS 'ReceiverStoreID sent to Create, from the verified pickup version (F17 String(6) maximum).';
COMMENT ON COLUMN fulfillment.cvs_shipments.pickup_id IS 'fulfillment.pickup_versions row the destination snapshot came from.';
COMMENT ON COLUMN fulfillment.cvs_shipments.goods_amount IS 'GoodsAmount in whole TWD 1..20000 (F17); I05: server-computed from the order total.';
COMMENT ON COLUMN fulfillment.cvs_shipments.operation_id IS 'integration.operations id of the ecpay.cvs_create dispatch (one per attempt).';
COMMENT ON COLUMN fulfillment.cvs_shipments.state IS 'REQUESTED -> CREATED -> AT_DC -> AT_STORE -> PICKED_UP | UNCLAIMED, or FAILED/UNKNOWN/ABANDONED (§6).';
COMMENT ON COLUMN fulfillment.cvs_shipments.provider_logistics_id IS 'ECPay AllPayLogisticsID; required from CREATED on.';
COMMENT ON COLUMN fulfillment.cvs_shipments.cvs_payment_no IS 'ECPay CVSPaymentNo (C2C shipping code); conforming values only, others become ecpay.code_nonconforming events.';
COMMENT ON COLUMN fulfillment.cvs_shipments.cvs_validation_no IS 'ECPay CVSValidationNo (7-11 C2C); conforming values only.';
COMMENT ON COLUMN fulfillment.cvs_shipments.shipment_no IS 'ECPay B2C ShipmentNo from Query/V5; conforming values only.';
COMMENT ON COLUMN fulfillment.cvs_shipments.last_status_code IS 'Last ECPay status RtnCode/LogisticsStatus seen (digits only).';
COMMENT ON COLUMN fulfillment.cvs_shipments.last_status_at IS 'DB time last_status_code was stored.';
COMMENT ON COLUMN fulfillment.cvs_shipments.principal_id IS 'Membership that requested the shipment (fulfillment:write); used for audit and Finish GUCs.';
COMMENT ON COLUMN fulfillment.cvs_shipments.created_at IS 'DB time the attempt was requested.';
COMMENT ON COLUMN fulfillment.cvs_shipments.updated_at IS 'DB time of the last write.';
COMMENT ON COLUMN fulfillment.cvs_shipments.version IS 'Optimistic-concurrency version; +1 per write (guard_cvs_shipment_update).';
COMMENT ON COLUMN fulfillment.cvs_shipment_events.tenant_id IS 'Tenant scope (server-side auth, RLS key); never from a request body.';
COMMENT ON COLUMN fulfillment.cvs_shipment_events.store_id IS 'Store scope (server-side auth, RLS key); never from a request body.';
COMMENT ON COLUMN fulfillment.cvs_shipment_events.order_id IS 'checkout.orders id of the shipment.';
COMMENT ON COLUMN fulfillment.cvs_shipment_events.attempt IS 'Shipment attempt the event belongs to.';
COMMENT ON COLUMN fulfillment.cvs_shipment_events.id IS 'Event id (append-only table).';
COMMENT ON COLUMN fulfillment.cvs_shipment_events.source IS 'local (definer decision), ecpay_status (signed notification) or ecpay_query (Query/V5).';
COMMENT ON COLUMN fulfillment.cvs_shipment_events.event_code IS 'Bounded code: shipment.status, ecpay.created, alert.duplicate_label_risk, alert.collection_conflict, ecpay.code_nonconforming ...';
COMMENT ON COLUMN fulfillment.cvs_shipment_events.body_sha256 IS 'sha256 of the raw provider body; the unique key that dedupes ECPay''s identical retries. Never the body itself.';
COMMENT ON COLUMN fulfillment.cvs_shipment_events.provider_code IS 'ECPay RtnCode of the report (digits only).';
COMMENT ON COLUMN fulfillment.cvs_shipment_events.provider_message IS 'ECPay RtnMsg, at most 200 chars, no control characters; never recipient PII (dropped after MAC check).';
COMMENT ON COLUMN fulfillment.cvs_shipment_events.provider_updated_at IS 'ECPay UpdateStatusDate as sent (yyyy/MM/dd HH:mm:ss, Asia/Taipei), kept as text.';
COMMENT ON COLUMN fulfillment.cvs_shipment_events.from_state IS 'Shipment (or collection) state before the event; NULL when not a transition.';
COMMENT ON COLUMN fulfillment.cvs_shipment_events.to_state IS 'Shipment (or collection) state after the event; equal to from_state for alert-only events.';
COMMENT ON COLUMN fulfillment.cvs_shipment_events.received_at IS 'DB time the event was stored.';
COMMENT ON COLUMN fulfillment.cvs_store_settings.tenant_id IS 'Tenant scope (server-side auth, RLS key); never from a request body.';
COMMENT ON COLUMN fulfillment.cvs_store_settings.store_id IS 'Store scope (server-side auth, RLS key); never from a request body.';
COMMENT ON COLUMN fulfillment.cvs_store_settings.enabled_chains IS 'C4: chains the store offers (subset of cvs_711, cvs_familymart, cvs_hilife, cvs_okmart).';
COMMENT ON COLUMN fulfillment.cvs_store_settings.pay_at_pickup_enabled IS 'C4: pay-at-pickup (超商取貨付款) on/off; needs pay_at_pickup_max_twd.';
COMMENT ON COLUMN fulfillment.cvs_store_settings.pay_at_pickup_max_twd IS 'C4: per-order pay-at-pickup cap in whole TWD 1..20000 (I05 comparison in begin_hold).';
COMMENT ON COLUMN fulfillment.cvs_store_settings.pay_at_pickup_max_open IS 'R4-3: open PENDING pay-at-pickup orders allowed per store before PT429 pay_at_pickup_limit.';
COMMENT ON COLUMN fulfillment.cvs_store_settings.version IS 'Optimistic-concurrency version; +1 per write.';
COMMENT ON COLUMN fulfillment.cvs_store_settings.updated_at IS 'DB time of the last write.';
COMMENT ON FUNCTION fulfillment.guard_cvs_shipment_update() IS 'fulfillment trigger guard: cvs_shipments identity columns immutable, version advances by exactly one, no delete/truncate.';
COMMENT ON FUNCTION fulfillment.guard_cvs_event_immutable() IS 'fulfillment trigger guard: cvs_shipment_events is append-only.';
COMMENT ON FUNCTION fulfillment.guard_cvs_shipment_state() IS 'Deferred constraint trigger body on checkout.orders and cvs_shipments: fulfillment_state PROVIDER_LABEL_CREATED <=> a live shipment in CREATED..UNCLAIMED (I13). DEFINER (commerce_checkout_writer).';
COMMENT ON FUNCTION fulfillment.guard_api_service_binding() IS 'Constraint trigger body on service_versions (AFTER INSERT): an enabled API row needs an enabled, qualified ecpay_logistics profile of the same store and a delivery kind the profile supports (R-2). DEFINER (commerce_integration_writer).';
COMMENT ON FUNCTION inventory.guard_pay_at_pickup_ledger() IS 'inventory trigger guard (BEFORE INSERT on ledger): the only BUYER ALLOCATE is the pay-at-pickup commit of a pay_at_pickup order for the reserved quantity; the only MERCHANT DEALLOCATE releases a cancelled/restocked pay_at_pickup order for exactly the allocated quantity (§16.2, §16.8). DEFINER (commerce_checkout_writer).';
COMMENT ON FUNCTION inventory.guard_checkout_ledger() IS 'inventory owner; 0013 actor provenance guard with the 0072 MERCHANT DEALLOCATE branch (principal + buyer GUCs of the released order, §16.8).';
COMMENT ON INDEX fulfillment.cvs_shipments_one_live IS 'MD1: at most one live shipment per order; FAILED/ABANDONED attempts never block a new attempt and a late status for them can never violate this index.';
COMMENT ON INDEX inventory.ledger_pay_at_pickup_release_once IS '§16.8: at most one DEALLOCATE ledger row per order line, whatever the idempotency key.';
COMMENT ON INDEX checkout.orders_pay_at_pickup_open IS '§16.2 R4-3: open pay-at-pickup orders (store and owner counts in begin_hold).';
COMMENT ON POLICY pickup_versions_manual_only ON fulfillment.pickup_versions IS '0072: a merchant can never claim directory verification (§4.1).';
COMMENT ON POLICY pickup_versions_closed_namespace ON fulfillment.pickup_versions IS '0072: ecpay.* and buyer.* pickup namespaces are closed to commerce_runtime (§4.1).';
COMMENT ON POLICY buyer_entered_own ON fulfillment.pickup_versions IS '0072 X9-3: a buyer role reads buyer.<owner> rows only for its own owner.';
COMMENT ON POLICY buyer_entered_own ON fulfillment.pickup_heads IS '0072 X9-3: a buyer role reads buyer.<owner> heads only for its own owner.';
