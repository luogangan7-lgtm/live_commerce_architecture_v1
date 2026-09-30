-- 0079 platform billing (contracts/customers-billing-v1.md FROZEN 2026-09-30, §3.2, §0.1 C-3/C-4/C-5/C-8;
-- docs/delivery/units/billing-core.md defaults B1-B12).
--
-- Owns: the platform-fee subscription MIRROR (billing.store_customers, billing.subscriptions), the derived
-- standing (billing.store_standing), the one guard that refuses NEW claim-window opens under RESTRICTED
-- (trigger billing_guard_window_open on live.claim_windows, SQLSTATE PT412), the merchant billing reads
-- (identity.read_billing, identity.read_billing_standing) and the permission `billing:manage`.
--
-- Non-goals: no Stripe call, no invoice or event table, no usage ledger or meter, no River job, no LIVE
-- environment (BD8: environment='SANDBOX' CHECK), no block on buyer checkout, payments, refunds, fulfilment,
-- order reads/exports or privacy actions (BD5), no grant to any R3 role (meta-ads 0080 grants store_standing
-- to commerce_ads_writer, C-7/C-8), no backfill of grants for pre-0079 stores (scripts/ops/grant-r2-permissions.sql).
--
-- Depends on: 0078 (permission CHECK already carries customers:read / customers:privacy; this file fails when
-- they are missing), 0060 (live.claim_windows), 0061 (integration.merchant_accounts provider='stripe'), 0063
-- (permission CHECK re-derivation pattern, GUC-verified merchant definers), 0064 (live.claim_window_intervals,
-- integration.operations action meta.private_reply), 0065 (identity.create_initial_store, fifth version).
--
-- Callers: internal/billing (Service: Status, StartCheckout, OpenPortal, WebhookHandler), internal/httpapi
-- billing routes, internal/claims (PT412 mapping). Roles: commerce_runtime reaches this only through the
-- definers below; commerce_stripe_ingress (the platform webhook login, C-4) executes apply_subscription only.

-- ---------------------------------------------------------------------------------------
-- Preconditions and permission vocabulary. Re-derive the CURRENT constraint (0078 added customers:*) instead
-- of rebuilding it, so a later value can never be silently dropped (0063 pattern).
-- ---------------------------------------------------------------------------------------
DO $$
DECLARE v_def text; v_list text[];
BEGIN
 IF to_regclass('live.claim_windows') IS NULL OR to_regclass('live.claim_window_intervals') IS NULL
  OR to_regclass('integration.merchant_accounts') IS NULL OR to_regclass('integration.operations') IS NULL
  OR to_regclass('payments.facts') IS NULL THEN
  RAISE EXCEPTION '0079 requires 0060, 0061 and 0064 (claim windows, merchant accounts, operations, facts)';
 END IF;
 SELECT pg_get_constraintdef(c.oid) INTO STRICT v_def FROM pg_constraint c
  WHERE c.conrelid='identity.store_grants'::regclass AND c.conname='store_grants_permission_check';
 SELECT array_agg(t.m[1] ORDER BY t.ord) INTO v_list
  FROM regexp_matches(v_def,'''([a-z_]+:[a-z_]+)''::text','g') WITH ORDINALITY AS t(m,ord);
 IF v_list IS NULL OR NOT ('orders:read'=ANY(v_list) AND 'orders:export'=ANY(v_list)
   AND 'customers:read'=ANY(v_list) AND 'customers:privacy'=ANY(v_list)) THEN
  RAISE EXCEPTION '0079 requires 0078 (store_grants_permission_check lacks customers:read/customers:privacy): %',v_def;
 END IF;
 IF NOT 'billing:manage'=ANY(v_list) THEN
  v_list:=array_append(v_list,'billing:manage'::text);
  ALTER TABLE identity.store_grants DROP CONSTRAINT store_grants_permission_check;
  EXECUTE format('ALTER TABLE identity.store_grants ADD CONSTRAINT store_grants_permission_check CHECK (permission IN (%s))',
   (SELECT string_agg(quote_literal(p),',' ORDER BY ord) FROM unnest(v_list) WITH ORDINALITY AS u(p,ord)));
 END IF;
END $$;

-- ---------------------------------------------------------------------------------------
-- Role, schema, tables (contract §3.2 verbatim, plus RLS and grants).
-- ---------------------------------------------------------------------------------------
CREATE ROLE commerce_billing_writer NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
COMMENT ON ROLE commerce_billing_writer IS
 'internal/billing NOLOGIN definer owner (0079): writes billing.store_customers/subscriptions and the billing audit row; reads only integration.merchant_accounts(provider,account_id) for the BD1 account-conflict check; never a login, never a buyer, merchant-payment or claims writer.';

CREATE SCHEMA billing;
REVOKE ALL ON SCHEMA billing FROM PUBLIC;
-- USAGE for the callers is required to EXECUTE a billing.* function: commerce_runtime (merchant definers),
-- commerce_stripe_ingress (apply_subscription), commerce_auth (store_standing, read_billing). None of them gets a
-- table privilege on billing.* except commerce_auth SELECT (GUC-scoped).
GRANT USAGE ON SCHEMA billing TO commerce_billing_writer, commerce_auth, commerce_runtime, commerce_stripe_ingress;
COMMENT ON SCHEMA billing IS
 'internal/billing (0079): platform-fee subscription mirror and derived standing. Written only by commerce_billing_writer definers; read by commerce_auth definers. Never invoices, events, usage ledgers or merchant PSP data.';

CREATE TABLE billing.store_customers (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL,
 environment text NOT NULL CHECK (environment='SANDBOX'),                 -- BD8
 stripe_customer_id text NOT NULL UNIQUE CHECK (stripe_customer_id ~ '^cus_[A-Za-z0-9]{1,64}$'),
 platform_account_id text NOT NULL CHECK (platform_account_id ~ '^acct_[A-Za-z0-9]{1,59}$'),  -- BD1, from GET /v1/account
 open_checkout_session_id text CHECK (open_checkout_session_id ~ '^cs_[A-Za-z0-9_]{1,255}$'),
 open_checkout_expires_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (tenant_id,store_id,environment),
 CHECK ((open_checkout_session_id IS NULL)=(open_checkout_expires_at IS NULL)),
 UNIQUE (tenant_id,store_id,stripe_customer_id),
 FOREIGN KEY (tenant_id,store_id) REFERENCES control.stores(tenant_id,id));

CREATE TABLE billing.subscriptions (
 stripe_subscription_id text PRIMARY KEY CHECK (stripe_subscription_id ~ '^sub_[A-Za-z0-9]{1,64}$'),
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, stripe_customer_id text NOT NULL,
 status text NOT NULL CHECK (status IN ('incomplete','incomplete_expired','trialing','active','past_due','canceled','unpaid','paused')),
 price_id text NOT NULL CHECK (price_id ~ '^price_[A-Za-z0-9]{1,64}$'),
 current_period_start timestamptz, current_period_end timestamptz,
 cancel_at_period_end boolean NOT NULL,
 stripe_created_at timestamptz NOT NULL,
 retrieved_at timestamptz NOT NULL,                                        -- monotone (§4)
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 FOREIGN KEY (tenant_id,store_id,stripe_customer_id) REFERENCES billing.store_customers(tenant_id,store_id,stripe_customer_id),
 CHECK ((current_period_start IS NULL)=(current_period_end IS NULL)),
 CHECK (current_period_end IS NULL OR current_period_end>current_period_start));
CREATE INDEX subscriptions_store ON billing.subscriptions(tenant_id,store_id);

ALTER TABLE billing.store_customers ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.store_customers FORCE ROW LEVEL SECURITY;
ALTER TABLE billing.subscriptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.subscriptions FORCE ROW LEVEL SECURITY;
REVOKE ALL ON billing.store_customers, billing.subscriptions FROM PUBLIC;
GRANT SELECT, INSERT ON billing.store_customers, billing.subscriptions TO commerce_billing_writer;
GRANT UPDATE(open_checkout_session_id,open_checkout_expires_at) ON billing.store_customers TO commerce_billing_writer;
GRANT UPDATE(status,price_id,current_period_start,current_period_end,cancel_at_period_end,retrieved_at,updated_at)
 ON billing.subscriptions TO commerce_billing_writer;
GRANT SELECT ON billing.store_customers, billing.subscriptions TO commerce_auth;
-- The writer's definers resolve the store themselves (apply_subscription by Stripe customer id, the trigger by
-- the window row), so its policy cannot be GUC-scoped; scope is enforced in each function body. commerce_auth's
-- read policy is GUC-scoped: identity.read_billing* verify the GUCs against resolve_access first.
CREATE POLICY billing_writer_all ON billing.store_customers TO commerce_billing_writer USING(true) WITH CHECK(true);
CREATE POLICY billing_writer_all ON billing.subscriptions TO commerce_billing_writer USING(true) WITH CHECK(true);
CREATE POLICY billing_auth_read ON billing.store_customers FOR SELECT TO commerce_auth
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
CREATE POLICY billing_auth_read ON billing.subscriptions FOR SELECT TO commerce_auth
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);

-- Set-once identity, canceled is terminal (F-B1), retrieved_at never regresses. The DB-level twin of the
-- apply_subscription checks (defense in depth: a future writer cannot resurrect a canceled subscription).
CREATE FUNCTION billing.guard_subscription_update() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 IF NEW.stripe_subscription_id<>OLD.stripe_subscription_id OR NEW.tenant_id<>OLD.tenant_id
  OR NEW.store_id<>OLD.store_id OR NEW.stripe_customer_id<>OLD.stripe_customer_id
  OR NEW.stripe_created_at<>OLD.stripe_created_at THEN
  RAISE EXCEPTION 'billing subscription identity is immutable' USING ERRCODE='23514'; END IF;
 IF OLD.status='canceled' AND NEW.status<>'canceled' THEN
  RAISE EXCEPTION 'billing subscription canceled is terminal' USING ERRCODE='23514'; END IF;
 IF NEW.retrieved_at<OLD.retrieved_at THEN
  RAISE EXCEPTION 'billing subscription retrieved_at is monotone' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
ALTER FUNCTION billing.guard_subscription_update() OWNER TO commerce_billing_writer;
REVOKE ALL ON FUNCTION billing.guard_subscription_update() FROM PUBLIC;
CREATE TRIGGER subscription_guard BEFORE UPDATE ON billing.subscriptions
 FOR EACH ROW EXECUTE FUNCTION billing.guard_subscription_update();

-- ---------------------------------------------------------------------------------------
-- billing.store_standing (BD4): derived, never stored. incomplete / incomplete_expired are filtered out first
-- (F-B1: incomplete lasts 23 h and must never improve standing), then the best status wins.
-- DEFINER because an invoker would see no rows under FORCE RLS and fail open to UNBILLED.
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION billing.store_standing(p_tenant uuid,p_store uuid) RETURNS text
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT CASE WHEN count(*)=0 THEN 'UNBILLED'
  WHEN bool_or(s.status IN ('trialing','active')) THEN 'GOOD'
  WHEN bool_or(s.status='past_due') THEN 'GRACE'
  ELSE 'RESTRICTED' END
 FROM billing.subscriptions s
 WHERE s.tenant_id=p_tenant AND s.store_id=p_store AND s.status NOT IN ('incomplete','incomplete_expired')
$$;
ALTER FUNCTION billing.store_standing(uuid,uuid) OWNER TO commerce_billing_writer;
REVOKE ALL ON FUNCTION billing.store_standing(uuid,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION billing.store_standing(uuid,uuid) TO commerce_auth;

-- ---------------------------------------------------------------------------------------
-- billing.guard_window_open (BD5): the only billing effect outside billing.*. Fires on OPEN transitions only;
-- an OPEN window is never closed by billing. DEFINER because commerce_runtime writes live.claim_windows
-- directly (0060 GRANT UPDATE) and must never get SELECT on billing.*.
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION billing.guard_window_open() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 IF TG_OP='UPDATE' THEN
  IF OLD.state='OPEN' THEN RETURN NEW; END IF;   -- already open: never a new open
 END IF;
 IF billing.store_standing(NEW.tenant_id,NEW.store_id)='RESTRICTED' THEN
  RAISE EXCEPTION 'billing_restricted' USING ERRCODE='PT412';
 END IF;
 RETURN NEW;
END $$;
ALTER FUNCTION billing.guard_window_open() OWNER TO commerce_billing_writer;
REVOKE ALL ON FUNCTION billing.guard_window_open() FROM PUBLIC;
CREATE TRIGGER billing_guard_window_open BEFORE INSERT OR UPDATE OF state ON live.claim_windows
 FOR EACH ROW WHEN (NEW.state='OPEN') EXECUTE FUNCTION billing.guard_window_open();

-- ---------------------------------------------------------------------------------------
-- billing.platform_account_conflict (BD1): the platform billing account must differ from every registered
-- Stripe merchant account, in any environment. Malformed ids count as a conflict (fail closed).
-- ---------------------------------------------------------------------------------------
GRANT USAGE ON SCHEMA integration TO commerce_billing_writer;
GRANT SELECT(provider,account_id) ON integration.merchant_accounts TO commerce_billing_writer;
CREATE POLICY billing_account_conflict_read ON integration.merchant_accounts FOR SELECT TO commerce_billing_writer
 USING(provider='stripe');
CREATE FUNCTION billing.platform_account_conflict(p_account text) RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT p_account IS NULL OR p_account !~ '^acct_[A-Za-z0-9]{1,59}$'
  OR EXISTS(SELECT 1 FROM integration.merchant_accounts a WHERE a.provider='stripe' AND a.account_id=p_account)
$$;
ALTER FUNCTION billing.platform_account_conflict(text) OWNER TO commerce_billing_writer;
REVOKE ALL ON FUNCTION billing.platform_account_conflict(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION billing.platform_account_conflict(text) TO commerce_runtime, commerce_stripe_ingress;

-- ---------------------------------------------------------------------------------------
-- Merchant-invoked writers authenticate through identity.resolve_access and audit through ops.audit_events.
-- ---------------------------------------------------------------------------------------
GRANT USAGE ON SCHEMA identity, ops TO commerce_billing_writer;
GRANT EXECUTE ON FUNCTION identity.resolve_access(bytea,uuid,text) TO commerce_billing_writer;
GRANT INSERT ON ops.audit_events TO commerce_billing_writer;
CREATE POLICY billing_audit_insert ON ops.audit_events FOR INSERT TO commerce_billing_writer
 WITH CHECK(action='billing.customer_pinned'
  AND tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND principal_id=nullif(current_setting('app.principal_id',true),'')::uuid);

-- pin_customer: set-once per (store, env). Same id replay OK, a different id PT409. The Stripe customer id is
-- unique across stores (23505 when another store already owns it).
CREATE FUNCTION billing.pin_customer(p_hash bytea,p_store uuid,p_env text,p_customer text,p_account text) RETURNS text
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE s record; v_rows bigint; v_existing text;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR p_env IS DISTINCT FROM 'SANDBOX'
  OR p_customer IS NULL OR p_customer !~ '^cus_[A-Za-z0-9]{1,64}$'
  OR p_account IS NULL OR p_account !~ '^acct_[A-Za-z0-9]{1,59}$'
  OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid billing customer' USING ERRCODE='PT400'; END IF;
 SELECT * INTO s FROM identity.resolve_access(p_hash,p_store,'billing:manage');
 IF s.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF s.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF s.access_status IS DISTINCT FROM 'ok' THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 IF current_setting('app.tenant_id',true) IS DISTINCT FROM s.tenant_id::text
  OR current_setting('app.store_id',true) IS DISTINCT FROM p_store::text
  OR current_setting('app.principal_id',true) IS DISTINCT FROM s.principal_id::text THEN
  RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 -- BD1: never pin the platform account that is a store's merchant of record.
 IF billing.platform_account_conflict(p_account) THEN
  RAISE EXCEPTION 'billing_account_conflict' USING ERRCODE='PT409'; END IF;
 INSERT INTO billing.store_customers(tenant_id,store_id,environment,stripe_customer_id,platform_account_id)
  VALUES(s.tenant_id,p_store,p_env,p_customer,p_account)
  ON CONFLICT (tenant_id,store_id,environment) DO NOTHING;
 GET DIAGNOSTICS v_rows=ROW_COUNT;
 IF v_rows=1 THEN
  INSERT INTO ops.audit_events(tenant_id,store_id,principal_id,action)
   VALUES(s.tenant_id,p_store,s.principal_id,'billing.customer_pinned');
  RETURN p_customer;
 END IF;
 SELECT c.stripe_customer_id INTO v_existing FROM billing.store_customers c
  WHERE c.tenant_id=s.tenant_id AND c.store_id=p_store AND c.environment=p_env;
 IF v_existing IS DISTINCT FROM p_customer THEN
  RAISE EXCEPTION 'billing customer already pinned' USING ERRCODE='PT409'; END IF;
 RETURN v_existing;
END $$;
ALTER FUNCTION billing.pin_customer(bytea,uuid,text,text,text) OWNER TO commerce_billing_writer;
REVOKE ALL ON FUNCTION billing.pin_customer(bytea,uuid,text,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION billing.pin_customer(bytea,uuid,text,text,text) TO commerce_runtime;

-- record_checkout_session: at most one open Checkout Session per store (§5 step 4). Returns the id of an older
-- session that is still unexpired (the caller expires it at Stripe) or NULL.
CREATE FUNCTION billing.record_checkout_session(p_hash bytea,p_store uuid,p_env text,p_session text,p_expires timestamptz) RETURNS text
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE s record; v_old text; v_old_expires timestamptz;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR p_env IS DISTINCT FROM 'SANDBOX'
  OR p_session IS NULL OR p_session !~ '^cs_[A-Za-z0-9_]{1,255}$'
  OR p_expires IS NULL OR p_expires<=clock_timestamp() OR p_expires>clock_timestamp()+interval '31 minutes'
  OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid checkout session' USING ERRCODE='PT400'; END IF;
 SELECT * INTO s FROM identity.resolve_access(p_hash,p_store,'billing:manage');
 IF s.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF s.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF s.access_status IS DISTINCT FROM 'ok' THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 IF current_setting('app.tenant_id',true) IS DISTINCT FROM s.tenant_id::text
  OR current_setting('app.store_id',true) IS DISTINCT FROM p_store::text
  OR current_setting('app.principal_id',true) IS DISTINCT FROM s.principal_id::text THEN
  RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 -- The row lock serializes two tabs: the second waits, sees the first session and reports it for expiry.
 SELECT c.open_checkout_session_id,c.open_checkout_expires_at INTO v_old,v_old_expires
  FROM billing.store_customers c
  WHERE c.tenant_id=s.tenant_id AND c.store_id=p_store AND c.environment=p_env FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'no billing customer' USING ERRCODE='PT404'; END IF;
 UPDATE billing.store_customers c SET open_checkout_session_id=p_session,open_checkout_expires_at=p_expires
  WHERE c.tenant_id=s.tenant_id AND c.store_id=p_store AND c.environment=p_env;
 IF v_old IS NOT NULL AND v_old<>p_session AND v_old_expires>clock_timestamp() THEN RETURN v_old; END IF;
 RETURN NULL;
END $$;
ALTER FUNCTION billing.record_checkout_session(bytea,uuid,text,text,timestamptz) OWNER TO commerce_billing_writer;
REVOKE ALL ON FUNCTION billing.record_checkout_session(bytea,uuid,text,text,timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION billing.record_checkout_session(bytea,uuid,text,text,timestamptz) TO commerce_runtime;

-- apply_subscription: the single mirror writer (BD3). Executed by the platform webhook login only (C-4) and,
-- through refresh_subscription, by the merchant-scoped refresh. Outcomes: applied | stale | duplicate |
-- mismatch | unknown_customer; PT400 for livemode, a future retrieved_at or an unusable value.
CREATE FUNCTION billing.apply_subscription(p_env text,p_customer text,p_subscription text,p_status text,p_price text,
 p_period_start timestamptz,p_period_end timestamptz,p_cancel_at_period_end boolean,p_created timestamptz,
 p_retrieved_at timestamptz,p_meta_store uuid,p_livemode boolean) RETURNS text
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_tenant uuid; v_store uuid; v_old billing.subscriptions%ROWTYPE; v_found boolean;
BEGIN
 IF p_livemode IS DISTINCT FROM false OR p_env IS DISTINCT FROM 'SANDBOX'          -- BD8
  OR p_customer IS NULL OR p_subscription IS NULL OR p_status IS NULL OR p_price IS NULL
  OR p_cancel_at_period_end IS NULL OR p_created IS NULL OR p_retrieved_at IS NULL
  OR (p_period_start IS NULL)<>(p_period_end IS NULL)
  OR p_retrieved_at>clock_timestamp()+interval '5 seconds' THEN
  RAISE EXCEPTION 'invalid subscription' USING ERRCODE='PT400'; END IF;
 -- The store comes ONLY from the pinned customer; the row lock serializes concurrent applies of one store.
 SELECT c.tenant_id,c.store_id INTO v_tenant,v_store FROM billing.store_customers c
  WHERE c.environment=p_env AND c.stripe_customer_id=p_customer FOR UPDATE;
 IF NOT FOUND THEN RETURN 'unknown_customer'; END IF;
 -- A subscription not created through our Checkout (Dashboard, another store's metadata) is never mirrored.
 IF p_meta_store IS NULL OR p_meta_store<>v_store THEN RETURN 'mismatch'; END IF;
 SELECT * INTO v_old FROM billing.subscriptions s WHERE s.stripe_subscription_id=p_subscription FOR UPDATE;
 v_found:=FOUND;
 IF v_found THEN
  IF v_old.tenant_id<>v_tenant OR v_old.store_id<>v_store OR v_old.stripe_customer_id<>p_customer THEN
   RETURN 'mismatch'; END IF;
  -- Out-of-order delivery cannot regress state; canceled is terminal (F-B1).
  IF v_old.retrieved_at>=p_retrieved_at OR (v_old.status='canceled' AND p_status<>'canceled') THEN
   RETURN 'stale'; END IF;
  UPDATE billing.subscriptions s SET status=p_status,price_id=p_price,current_period_start=p_period_start,
   current_period_end=p_period_end,cancel_at_period_end=p_cancel_at_period_end,retrieved_at=p_retrieved_at,
   updated_at=clock_timestamp()
   WHERE s.stripe_subscription_id=p_subscription;
 ELSE
  INSERT INTO billing.subscriptions(stripe_subscription_id,tenant_id,store_id,stripe_customer_id,status,price_id,
   current_period_start,current_period_end,cancel_at_period_end,stripe_created_at,retrieved_at)
  VALUES(p_subscription,v_tenant,v_store,p_customer,p_status,p_price,p_period_start,p_period_end,
   p_cancel_at_period_end,p_created,p_retrieved_at);
 END IF;
 -- BD2: still mirrored, but an operator alert (no auto-cancel) when two non-terminal subscriptions coexist.
 IF p_status NOT IN ('canceled','incomplete_expired') AND EXISTS(SELECT 1 FROM billing.subscriptions o
   WHERE o.tenant_id=v_tenant AND o.store_id=v_store AND o.stripe_subscription_id<>p_subscription
    AND o.status NOT IN ('canceled','incomplete_expired')) THEN
  RETURN 'duplicate'; END IF;
 RETURN 'applied';
END $$;
ALTER FUNCTION billing.apply_subscription(text,text,text,text,text,timestamptz,timestamptz,boolean,timestamptz,timestamptz,uuid,boolean) OWNER TO commerce_billing_writer;
REVOKE ALL ON FUNCTION billing.apply_subscription(text,text,text,text,text,timestamptz,timestamptz,boolean,timestamptz,timestamptz,uuid,boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION billing.apply_subscription(text,text,text,text,text,timestamptz,timestamptz,boolean,timestamptz,timestamptz,uuid,boolean) TO commerce_stripe_ingress;

-- refresh_subscription: store-scoped entry for refresh-on-read and the checkout pre-check (§5). The resolved
-- customer must belong to p_store (PT404 otherwise, indistinguishable from an unknown customer), then the
-- apply_subscription body runs as this definer's owner.
CREATE FUNCTION billing.refresh_subscription(p_hash bytea,p_store uuid,p_env text,p_customer text,p_subscription text,
 p_status text,p_price text,p_period_start timestamptz,p_period_end timestamptz,p_cancel_at_period_end boolean,
 p_created timestamptz,p_retrieved_at timestamptz,p_meta_store uuid,p_livemode boolean) RETURNS text
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE s record; v_store uuid;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL
  OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid subscription refresh' USING ERRCODE='PT400'; END IF;
 SELECT * INTO s FROM identity.resolve_access(p_hash,p_store,'billing:manage');
 IF s.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF s.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF s.access_status IS DISTINCT FROM 'ok' THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 IF current_setting('app.tenant_id',true) IS DISTINCT FROM s.tenant_id::text
  OR current_setting('app.store_id',true) IS DISTINCT FROM p_store::text
  OR current_setting('app.principal_id',true) IS DISTINCT FROM s.principal_id::text THEN
  RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 SELECT c.store_id INTO v_store FROM billing.store_customers c
  WHERE c.tenant_id=s.tenant_id AND c.environment=p_env AND c.stripe_customer_id=p_customer;
 IF v_store IS DISTINCT FROM p_store THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 RETURN billing.apply_subscription(p_env,p_customer,p_subscription,p_status,p_price,p_period_start,p_period_end,
  p_cancel_at_period_end,p_created,p_retrieved_at,p_meta_store,p_livemode);
END $$;
ALTER FUNCTION billing.refresh_subscription(bytea,uuid,text,text,text,text,text,timestamptz,timestamptz,boolean,timestamptz,timestamptz,uuid,boolean) OWNER TO commerce_billing_writer;
REVOKE ALL ON FUNCTION billing.refresh_subscription(bytea,uuid,text,text,text,text,text,timestamptz,timestamptz,boolean,timestamptz,timestamptz,uuid,boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION billing.refresh_subscription(bytea,uuid,text,text,text,text,text,timestamptz,timestamptz,boolean,timestamptz,timestamptz,uuid,boolean) TO commerce_runtime;

-- ---------------------------------------------------------------------------------------
-- Merchant reads (owner commerce_auth, the 0027/0063 projection pattern): identity.read_billing (billing:manage)
-- and identity.read_billing_standing (store:read, the banner every member sees).
-- ---------------------------------------------------------------------------------------
GRANT USAGE ON SCHEMA live, integration TO commerce_auth;
-- BD6 usage counts: exactly the columns each count needs, each under a GUC-scoped policy.
GRANT SELECT(received_at) ON payments.facts TO commerce_auth;        -- existing policy merchant_order_projection
GRANT SELECT(tenant_id,store_id,opened_at) ON live.claim_window_intervals TO commerce_auth;
CREATE POLICY billing_usage_read ON live.claim_window_intervals FOR SELECT TO commerce_auth
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
GRANT SELECT(tenant_id,store_id,action,state,updated_at) ON integration.operations TO commerce_auth;
CREATE POLICY billing_usage_read ON integration.operations FOR SELECT TO commerce_auth
 USING(action='meta.private_reply'
  AND tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);

CREATE FUNCTION identity.read_billing(p_hash bytea,p_store uuid) RETURNS jsonb
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE s record; v_standing text; v_customer text; v_subs jsonb; v_pending boolean;
 v_start timestamptz; v_end timestamptz;
 v_paid bigint; v_windows bigint; v_replies bigint; v_members bigint;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL
  OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid billing read' USING ERRCODE='PT400'; END IF;
 SELECT * INTO s FROM identity.resolve_access(p_hash,p_store,'billing:manage');
 IF s.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF s.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF s.access_status IS DISTINCT FROM 'ok' THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 IF current_setting('app.tenant_id',true) IS DISTINCT FROM s.tenant_id::text
  OR current_setting('app.store_id',true) IS DISTINCT FROM p_store::text
  OR current_setting('app.principal_id',true) IS DISTINCT FROM s.principal_id::text THEN
  RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 v_standing:=billing.store_standing(s.tenant_id,p_store);
 SELECT c.stripe_customer_id INTO v_customer FROM billing.store_customers c
  WHERE c.tenant_id=s.tenant_id AND c.store_id=p_store AND c.environment='SANDBOX';
 SELECT coalesce(jsonb_agg(jsonb_build_object(
   'status',x.status,'price_id',x.price_id,
   'current_period_start',to_char(x.current_period_start AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'current_period_end',to_char(x.current_period_end AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'cancel_at_period_end',x.cancel_at_period_end,
   'retrieved_at',to_char(x.retrieved_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'))
   ORDER BY x.stripe_created_at,x.stripe_subscription_id),'[]'::jsonb),
  coalesce(bool_or(x.status='incomplete'),false)
  INTO v_subs,v_pending
  FROM billing.subscriptions x WHERE x.tenant_id=s.tenant_id AND x.store_id=p_store;
 -- BD6 period: the current billing period of the live subscription; UNBILLED or no period -> calendar month
 -- in Asia/Taipei (Q11).
 SELECT x.current_period_start,x.current_period_end INTO v_start,v_end FROM billing.subscriptions x
  WHERE x.tenant_id=s.tenant_id AND x.store_id=p_store AND x.status IN ('trialing','active','past_due')
   AND x.current_period_start IS NOT NULL
  ORDER BY x.current_period_end DESC,x.stripe_subscription_id LIMIT 1;
 IF v_start IS NULL THEN
  v_start:=date_trunc('month',clock_timestamp() AT TIME ZONE 'Asia/Taipei') AT TIME ZONE 'Asia/Taipei';
  v_end:=(date_trunc('month',clock_timestamp() AT TIME ZONE 'Asia/Taipei')+interval '1 month') AT TIME ZONE 'Asia/Taipei';
 END IF;
 SELECT count(*) INTO v_paid FROM payments.facts f
  WHERE f.tenant_id=s.tenant_id AND f.store_id=p_store AND f.kind='CAPTURED' AND f.received_at>=v_start AND f.received_at<v_end;
 SELECT count(*) INTO v_windows FROM live.claim_window_intervals i
  WHERE i.tenant_id=s.tenant_id AND i.store_id=p_store AND i.opened_at>=v_start AND i.opened_at<v_end;
 SELECT count(*) INTO v_replies FROM integration.operations o
  WHERE o.tenant_id=s.tenant_id AND o.store_id=p_store AND o.action='meta.private_reply' AND o.state='SUCCEEDED'
   AND o.updated_at>=v_start AND o.updated_at<v_end;
 SELECT count(DISTINCT g.principal_id) INTO v_members FROM identity.store_grants g
  JOIN identity.memberships m ON m.tenant_id=g.tenant_id AND m.principal_id=g.principal_id AND m.active
  WHERE g.tenant_id=s.tenant_id AND g.store_id=p_store AND g.permission='store:read';
 RETURN jsonb_build_object('standing',v_standing,'payment_pending',v_pending,
  'customer_pinned',v_customer IS NOT NULL,'stripe_customer_id',v_customer,'subscriptions',v_subs,
  'usage',jsonb_build_object(
   'period_start',to_char(v_start AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'period_end',to_char(v_end AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'paid_orders',v_paid,'claim_windows_opened',v_windows,'private_replies_sent',v_replies,'members',v_members));
END $$;
ALTER FUNCTION identity.read_billing(bytea,uuid) OWNER TO commerce_auth;
REVOKE ALL ON FUNCTION identity.read_billing(bytea,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION identity.read_billing(bytea,uuid) TO commerce_runtime;

CREATE FUNCTION identity.read_billing_standing(p_hash bytea,p_store uuid) RETURNS text
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE s record;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL
  OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid billing read' USING ERRCODE='PT400'; END IF;
 SELECT * INTO s FROM identity.resolve_access(p_hash,p_store,'store:read');
 IF s.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF s.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF s.access_status IS DISTINCT FROM 'ok' THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 IF current_setting('app.tenant_id',true) IS DISTINCT FROM s.tenant_id::text
  OR current_setting('app.store_id',true) IS DISTINCT FROM p_store::text
  OR current_setting('app.principal_id',true) IS DISTINCT FROM s.principal_id::text THEN
  RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 RETURN billing.store_standing(s.tenant_id,p_store);
END $$;
ALTER FUNCTION identity.read_billing_standing(bytea,uuid) OWNER TO commerce_auth;
REVOKE ALL ON FUNCTION identity.read_billing_standing(bytea,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION identity.read_billing_standing(bytea,uuid) TO commerce_runtime;

-- ---------------------------------------------------------------------------------------
-- identity.create_initial_store, sixth version (C-5): the 0065 body byte for byte except the fresh-store grant
-- array (+ customers:read, customers:privacy, billing:manage). No INSERT/UPDATE of identity.store_grants for
-- existing principals: no backfill, replay never restores a removed grant, other members gain nothing.
-- ---------------------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION identity.create_initial_store(p_token bytea,p_key text,p_hash bytea,p_tenant_name text,p_store_name text,p_warehouse_name text,p_currency text)
RETURNS TABLE(tenant_id uuid,store_id uuid,warehouse_id uuid)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_principal uuid; v_tenant uuid; v_store uuid; v_warehouse uuid; v_key text; v_hash bytea;
BEGIN
    IF p_key IS NULL OR p_key !~ '^[A-Za-z0-9_.:-]{8,128}$' OR p_hash IS NULL OR octet_length(p_hash)<>32
       OR p_tenant_name IS NULL OR length(p_tenant_name) NOT BETWEEN 1 AND 120
       OR p_store_name IS NULL OR length(p_store_name) NOT BETWEEN 1 AND 120
       OR p_warehouse_name IS NULL OR length(p_warehouse_name) NOT BETWEEN 1 AND 120
       OR p_currency IS NULL OR p_currency !~ '^[A-Z]{3}$' THEN RAISE EXCEPTION 'invalid store request' USING ERRCODE='PT400'; END IF;
    SELECT s.principal_id INTO v_principal FROM identity.sessions s WHERE s.token_hash=p_token AND s.audience='merchant';
    IF NOT FOUND THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
    PERFORM 1 FROM identity.principals p WHERE p.id=v_principal AND p.active FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
    PERFORM 1 FROM identity.sessions s WHERE s.token_hash=p_token AND s.principal_id=v_principal AND s.audience='merchant'
      AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp() FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
    SELECT i.idempotency_key,i.request_hash,i.tenant_id,i.store_id,i.warehouse_id INTO v_key,v_hash,v_tenant,v_store,v_warehouse
      FROM identity.initial_stores i WHERE i.principal_id=v_principal;
    IF FOUND THEN
        IF v_key<>p_key OR v_hash<>p_hash THEN RAISE EXCEPTION 'initial store conflict' USING ERRCODE='PT409'; END IF;
        PERFORM 1 FROM identity.memberships m JOIN control.tenants t ON t.id=m.tenant_id AND t.active
          JOIN control.stores s ON s.tenant_id=t.id AND s.id=v_store AND s.active
          WHERE m.tenant_id=v_tenant AND m.principal_id=v_principal AND m.active;
        IF NOT FOUND THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
        RETURN QUERY SELECT v_tenant,v_store,v_warehouse; RETURN;
    END IF;
    INSERT INTO control.tenants(id,name) VALUES(gen_random_uuid(),p_tenant_name) RETURNING id INTO v_tenant;
    INSERT INTO control.stores(tenant_id,id,name,currency) VALUES(v_tenant,gen_random_uuid(),p_store_name,p_currency) RETURNING id INTO v_store;
    INSERT INTO identity.memberships(tenant_id,principal_id) VALUES(v_tenant,v_principal);
    INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission)
      SELECT v_tenant,v_store,v_principal,p FROM unnest(ARRAY['store:read','audit:read','audit:write','catalog:read','catalog:write','inventory:read','inventory:write','inventory:reserve','pricing:read','pricing:write','integration:read','integration:manage','orders:read','live:read','live:manage','payments:refund','fulfillment:write','orders:export','integration:execute','customers:read','customers:privacy','billing:manage']) p;
    INSERT INTO inventory.warehouses(tenant_id,store_id,name) VALUES(v_tenant,v_store,p_warehouse_name) RETURNING id INTO v_warehouse;
    INSERT INTO identity.initial_stores(principal_id,idempotency_key,request_hash,tenant_id,store_id,warehouse_id)
      VALUES(v_principal,p_key,p_hash,v_tenant,v_store,v_warehouse);
    INSERT INTO ops.audit_events(tenant_id,store_id,principal_id,action) VALUES(v_tenant,v_store,v_principal,'merchant.store_created');
    RETURN QUERY SELECT v_tenant,v_store,v_warehouse;
END $$;
ALTER FUNCTION identity.create_initial_store(bytea,text,bytea,text,text,text,text) OWNER TO commerce_identity_writer;
REVOKE ALL ON FUNCTION identity.create_initial_store(bytea,text,bytea,text,text,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION identity.create_initial_store(bytea,text,bytea,text,text,text,text) TO commerce_identity;
COMMENT ON FUNCTION identity.create_initial_store(bytea,text,bytea,text,text,text,text) IS
 'internal/identity onboarding only; EXECUTE commerce_identity. Since 0079 (sixth version, C-5) the creator receives the 0065 set plus customers:read, customers:privacy and billing:manage on the new store; no backfill of existing principals (scripts/ops/grant-r2-permissions.sql is the owner-approved operator path), replay never restores removed grants.';

-- ---------------------------------------------------------------------------------------
-- COMMENT ON (PROCESS §5): every table, column, function, policy and trigger this file adds.
-- ---------------------------------------------------------------------------------------
COMMENT ON TABLE billing.store_customers IS
 'internal/billing: the platform Stripe customer pinned to one store per environment (BD1/BD8), plus the single open Checkout Session (§5 step 4). Written by billing.pin_customer / record_checkout_session (commerce_billing_writer), read by commerce_auth under a GUC-scoped policy. Never holds a merchant PSP account, a secret or an invoice.';
COMMENT ON COLUMN billing.store_customers.tenant_id IS 'owning tenant; part of the store key';
COMMENT ON COLUMN billing.store_customers.store_id IS 'billing unit = store (BD2, Q3)';
COMMENT ON COLUMN billing.store_customers.environment IS 'SANDBOX only in v1 (BD8); LIVE is an amendment after owner approval';
COMMENT ON COLUMN billing.store_customers.stripe_customer_id IS 'platform-account Stripe Customer id; unique across stores; set once by pin_customer';
COMMENT ON COLUMN billing.store_customers.platform_account_id IS 'platform Stripe account id read from GET /v1/account at pin time; must never equal a registered merchant PSP account (BD1)';
COMMENT ON COLUMN billing.store_customers.open_checkout_session_id IS 'the one unexpired Checkout Session of this store, or NULL; bearer-URL is never stored (I11)';
COMMENT ON COLUMN billing.store_customers.open_checkout_expires_at IS 'expiry of open_checkout_session_id (<= 31 min ahead when stored)';
COMMENT ON COLUMN billing.store_customers.created_at IS 'pin time';
COMMENT ON TABLE billing.subscriptions IS
 'internal/billing: mirror of the last RETRIEVED Stripe Subscription per id (BD3). Written only by billing.apply_subscription (monotone retrieved_at, canceled terminal); standing is derived from it (billing.store_standing), never stored. Never an invoice, event or usage row.';
COMMENT ON COLUMN billing.subscriptions.stripe_subscription_id IS 'Stripe Subscription id (sub_...)';
COMMENT ON COLUMN billing.subscriptions.tenant_id IS 'resolved from the pinned customer, never from Stripe metadata';
COMMENT ON COLUMN billing.subscriptions.store_id IS 'resolved from the pinned customer; must equal metadata.lc_store or the row is not mirrored';
COMMENT ON COLUMN billing.subscriptions.stripe_customer_id IS 'the pinned Stripe Customer this subscription belongs to';
COMMENT ON COLUMN billing.subscriptions.status IS 'Stripe status verbatim (F-B1); BD4 maps it to standing';
COMMENT ON COLUMN billing.subscriptions.price_id IS 'items.data[0].price.id (F-B10); a subscription with more than one item is not mirrored (billing_multi_item)';
COMMENT ON COLUMN billing.subscriptions.current_period_start IS 'items.data[0].current_period_start (F-B10) or NULL';
COMMENT ON COLUMN billing.subscriptions.current_period_end IS 'items.data[0].current_period_end (F-B10) or NULL';
COMMENT ON COLUMN billing.subscriptions.cancel_at_period_end IS 'Stripe cancel_at_period_end flag';
COMMENT ON COLUMN billing.subscriptions.stripe_created_at IS 'Stripe created timestamp; immutable';
COMMENT ON COLUMN billing.subscriptions.retrieved_at IS 'server time of the Stripe GET that produced this row; monotone, out-of-order applies are stale';
COMMENT ON COLUMN billing.subscriptions.updated_at IS 'last local write';
COMMENT ON POLICY billing_writer_all ON billing.store_customers IS 'commerce_billing_writer definers resolve the store themselves; scope is enforced in each function body';
COMMENT ON POLICY billing_writer_all ON billing.subscriptions IS 'commerce_billing_writer definers resolve the store themselves; scope is enforced in each function body';
COMMENT ON POLICY billing_auth_read ON billing.store_customers IS 'commerce_auth reads only the GUC-scoped store that identity.read_billing verified against resolve_access';
COMMENT ON POLICY billing_auth_read ON billing.subscriptions IS 'commerce_auth reads only the GUC-scoped store that identity.read_billing verified against resolve_access';
COMMENT ON POLICY billing_account_conflict_read ON integration.merchant_accounts IS 'billing.platform_account_conflict reads Stripe accounts only (provider, account_id columns), BD1';
COMMENT ON POLICY billing_audit_insert ON ops.audit_events IS 'commerce_billing_writer may insert only billing.customer_pinned, scoped to the GUCs pin_customer verified against resolve_access';
COMMENT ON POLICY billing_usage_read ON live.claim_window_intervals IS 'identity.read_billing counts opened windows (BD6) for the GUC-scoped store only';
COMMENT ON POLICY billing_usage_read ON integration.operations IS 'identity.read_billing counts SUCCEEDED meta.private_reply operations (BD6) for the GUC-scoped store only; no other action is readable';
COMMENT ON FUNCTION billing.guard_subscription_update() IS 'trigger function on billing.subscriptions: set-once identity, canceled terminal, monotone retrieved_at; owner commerce_billing_writer, no caller EXECUTE';
COMMENT ON TRIGGER subscription_guard ON billing.subscriptions IS 'set-once identity, canceled terminal, monotone retrieved_at (F-B1)';
COMMENT ON FUNCTION billing.store_standing(uuid,uuid) IS
 'BD4 derived standing GOOD|GRACE|RESTRICTED|UNBILLED; STABLE DEFINER owner commerce_billing_writer, EXECUTE commerce_auth only (R3 commerce_ads_writer in meta-ads 0080, C-8). Twin of internal/billing.StandingOf (CB01). Never stored, never called by buyer/payment/refund/fulfilment code (CB07).';
COMMENT ON FUNCTION billing.guard_window_open() IS
 'trigger function of billing_guard_window_open on live.claim_windows: refuses only NEW opens (INSERT as OPEN, or OLD.state<>OPEN) under RESTRICTED with PT412 (-> HTTP 402 billing_restricted). The only billing effect outside billing.*; owner commerce_billing_writer, no caller EXECUTE.';
COMMENT ON TRIGGER billing_guard_window_open ON live.claim_windows IS 'BD5: RESTRICTED blocks only new claim-window opens; an OPEN window is never closed by billing';
COMMENT ON FUNCTION billing.platform_account_conflict(text) IS
 'BD1: true when the account id equals any provider=stripe integration.merchant_accounts.account_id (any environment) or is malformed. STABLE DEFINER owner commerce_billing_writer; EXECUTE commerce_runtime (startup check, pin) and commerce_stripe_ingress (C-4).';
COMMENT ON FUNCTION billing.pin_customer(bytea,uuid,text,text,text) IS
 'internal/billing: billing:manage + GUC check, BD1 conflict -> PT409, set-once per (store, env); same id replays, another id PT409; audit billing.customer_pinned. EXECUTE commerce_runtime.';
COMMENT ON FUNCTION billing.record_checkout_session(bytea,uuid,text,text,timestamptz) IS
 'internal/billing: billing:manage; locks the store_customers row, stores the new open session and returns an older unexpired session id for the caller to expire at Stripe, else NULL (§5 step 4). EXECUTE commerce_runtime.';
COMMENT ON FUNCTION billing.apply_subscription(text,text,text,text,text,timestamptz,timestamptz,boolean,timestamptz,timestamptz,uuid,boolean) IS
 'internal/billing webhook: the single mirror writer. EXECUTE commerce_stripe_ingress only (C-4); commerce_runtime never holds it. Store from the pinned customer only; NULL/other lc_store metadata -> mismatch; monotone retrieved_at -> stale; two non-terminal subscriptions -> duplicate (ops alert, no auto-cancel); livemode or future retrieved_at -> PT400.';
COMMENT ON FUNCTION billing.refresh_subscription(bytea,uuid,text,text,text,text,text,timestamptz,timestamptz,boolean,timestamptz,timestamptz,uuid,boolean) IS
 'internal/billing refresh-on-read and checkout pre-check: billing:manage + GUC check, the customer must belong to p_store (PT404), then apply_subscription. EXECUTE commerce_runtime.';
COMMENT ON FUNCTION identity.read_billing(bytea,uuid) IS
 'internal/billing merchant read (billing:manage): standing, payment_pending, pinned Stripe customer id (Go never returns it), every subscription row, BD6 usage counts for the current period (calendar month Asia/Taipei when UNBILLED). Owner commerce_auth; EXECUTE commerce_runtime.';
COMMENT ON FUNCTION identity.read_billing_standing(bytea,uuid) IS
 'internal/billing merchant banner read (store:read): standing text only. Owner commerce_auth; EXECUTE commerce_runtime.';
