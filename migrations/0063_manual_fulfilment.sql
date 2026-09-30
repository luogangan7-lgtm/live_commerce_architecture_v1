-- 0063 manual fulfilment (contracts/manual-fulfilment-v1.md FROZEN incl. integrator rulings;
-- docs/delivery/units/fulfilment-core.md defaults E1-E6; refund-fulfilment-rulings 1, 10, 11).
--
-- Owns: merchant-arranged shipment history (fulfillment.manual_shipment_versions/_heads), the
-- MD6 shipping-eligibility predicate, the merchant order projection successor
-- (identity.read_merchant_orders with refund and shipment fields), shipment history read,
-- unshipped CSV export source and the permission vocabulary `fulfillment:write`/`orders:export`.
--
-- Non-goals: no carrier API, label, tracking poll, provider operation or River job (MD2), no
-- buyer message (MD10), no grant row for any principal (onboarding lives in 0065, MD8), no
-- stock/ledger/reservation/payment write, no refund table or refund state write.
--
-- Depends on: 0062 (payments.stripe_refunds / refund_facts, the commerce_checkout_writer
-- merchant-auth + ops grants of stripe-refund-v1 §4.5 rows 1-3 and the commerce_auth refund
-- column grants: this file fails closed when they are missing), 0027 (merchant projection
-- pattern, commerce_auth grants), 0018 (payment_work_items, review_cases), 0013 (checkout.orders),
-- 0003 identity.resolve_access.
--
-- Callers: internal/merchantorders (RecordShipment, ShipmentHistory, ExportUnshipped, List/Get)
-- and internal/checkout (buyer order detail). commerce_runtime reaches this only through the
-- definers below; commerce_checkout_runtime (buyer) reads two column-limited tables under RLS.

-- ---------------------------------------------------------------------------------------
-- Permission vocabulary. Re-derive the CURRENT constraint (0062 added payments:refund) instead
-- of rebuilding from 0033, so a later migration's value can never be silently dropped.
-- ---------------------------------------------------------------------------------------
DO $$
DECLARE v_def text; v_list text[]; v_perm text;
BEGIN
 IF to_regclass('payments.stripe_refunds') IS NULL OR to_regclass('payments.refund_facts') IS NULL THEN
  RAISE EXCEPTION '0063 requires 0062 (payments.stripe_refunds, payments.refund_facts)';
 END IF;
 SELECT pg_get_constraintdef(c.oid) INTO STRICT v_def FROM pg_constraint c
  WHERE c.conrelid='identity.store_grants'::regclass AND c.conname='store_grants_permission_check';
 SELECT array_agg(t.m[1] ORDER BY t.ord) INTO v_list
  FROM regexp_matches(v_def,'''([a-z_]+:[a-z_]+)''::text','g') WITH ORDINALITY AS t(m,ord);
 IF v_list IS NULL OR NOT ('orders:read'=ANY(v_list) AND 'live:manage'=ANY(v_list) AND 'payments:refund'=ANY(v_list)) THEN
  RAISE EXCEPTION 'unexpected store_grants_permission_check: %',v_def;
 END IF;
 FOREACH v_perm IN ARRAY ARRAY['fulfillment:write','orders:export'] LOOP
  IF NOT v_perm=ANY(v_list) THEN v_list:=v_list||v_perm; END IF;
 END LOOP;
 ALTER TABLE identity.store_grants DROP CONSTRAINT store_grants_permission_check;
 EXECUTE format('ALTER TABLE identity.store_grants ADD CONSTRAINT store_grants_permission_check CHECK (permission IN (%s))',
  (SELECT string_agg(quote_literal(p),',' ORDER BY ord) FROM unnest(v_list) WITH ORDINALITY AS u(p,ord)));
END $$;

ALTER TABLE checkout.orders DROP CONSTRAINT orders_fulfillment_state_check;
ALTER TABLE checkout.orders ADD CONSTRAINT orders_fulfillment_state_check
 CHECK(fulfillment_state IN ('MANUAL_UNASSIGNED','CANCELLED','PAID_ALLOCATION_FAILED','MERCHANT_SHIPPED'));

-- ---------------------------------------------------------------------------------------
-- Append-only versions + head (the service_heads pattern, MD4).
-- ---------------------------------------------------------------------------------------
CREATE TABLE fulfillment.manual_shipment_versions (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, owner_id uuid NOT NULL, order_id uuid NOT NULL,
 version bigint NOT NULL CHECK(version>0),
 status text NOT NULL CHECK(status IN ('SHIPPED','VOIDED')),
 carrier_code text NOT NULL CHECK(carrier_code IN ('seven_eleven_cvs','familymart_cvs','hilife_cvs','okmart_cvs',
  'sf_express','chunghwa_post','other')),
 carrier_name text CHECK(carrier_name IS NULL OR (length(carrier_name) BETWEEN 1 AND 80 AND carrier_name !~ '[[:cntrl:]]')),
 tracking_number text NOT NULL CHECK(tracking_number ~ '^[A-Za-z0-9][A-Za-z0-9 -]{0,63}$' AND tracking_number !~ ' $'),
 tracking_url text CHECK(tracking_url IS NULL OR (octet_length(tracking_url)<=512 AND tracking_url ~ '^https://([a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?\.)+[a-zA-Z]([a-zA-Z0-9-]*[a-zA-Z0-9])?([/?][!-~]*)?$')),
 note text CHECK(note IS NULL OR (length(note)<=200 AND note !~ '[[:cntrl:]]')),
 void_reason text CHECK(void_reason IN ('wrong_order','wrong_tracking','not_dispatched','other')),
 principal_id uuid NOT NULL, recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,store_id,order_id,version),
 CHECK((status='VOIDED')=(void_reason IS NOT NULL)),
 CHECK(carrier_code<>'other' OR carrier_name IS NOT NULL),
 FOREIGN KEY(tenant_id,store_id,owner_id,order_id) REFERENCES checkout.orders(tenant_id,store_id,owner_id,id),
 FOREIGN KEY(tenant_id,principal_id) REFERENCES identity.memberships(tenant_id,principal_id)
);
CREATE TABLE fulfillment.manual_shipment_heads (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, owner_id uuid NOT NULL, order_id uuid NOT NULL,
 current_version bigint NOT NULL CHECK(current_version>0),
 updated_at timestamptz NOT NULL,
 PRIMARY KEY(tenant_id,store_id,order_id),
 FOREIGN KEY(tenant_id,store_id,owner_id,order_id) REFERENCES checkout.orders(tenant_id,store_id,owner_id,id),
 FOREIGN KEY(tenant_id,store_id,order_id,current_version)
  REFERENCES fulfillment.manual_shipment_versions(tenant_id,store_id,order_id,version) DEFERRABLE INITIALLY DEFERRED
);
-- Unshipped export/list scan: oldest paid first (M-5); the MD6 predicate then filters the few survivors.
CREATE INDEX orders_unshipped ON checkout.orders(tenant_id,store_id,created_at,id)
 WHERE commercial_state='CONFIRMED' AND fulfillment_state='MANUAL_UNASSIGNED';

ALTER TABLE fulfillment.manual_shipment_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE fulfillment.manual_shipment_versions FORCE ROW LEVEL SECURITY;
ALTER TABLE fulfillment.manual_shipment_heads ENABLE ROW LEVEL SECURITY;
ALTER TABLE fulfillment.manual_shipment_heads FORCE ROW LEVEL SECURITY;
REVOKE ALL ON fulfillment.manual_shipment_versions, fulfillment.manual_shipment_heads FROM PUBLIC;

-- Writer: SELECT is unscoped (definers filter tenant/store explicitly and the deferred state guard must
-- see rows whatever the GUCs), every write is scoped to the definer's authenticated GUCs.
GRANT SELECT,INSERT ON fulfillment.manual_shipment_versions TO commerce_checkout_writer;
GRANT SELECT,INSERT ON fulfillment.manual_shipment_heads TO commerce_checkout_writer;
GRANT UPDATE(current_version,updated_at) ON fulfillment.manual_shipment_heads TO commerce_checkout_writer;
CREATE POLICY manual_shipment_writer_select ON fulfillment.manual_shipment_versions FOR SELECT TO commerce_checkout_writer USING(true);
CREATE POLICY manual_shipment_writer_insert ON fulfillment.manual_shipment_versions FOR INSERT TO commerce_checkout_writer
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND principal_id=nullif(current_setting('app.principal_id',true),'')::uuid);
CREATE POLICY manual_shipment_head_writer_select ON fulfillment.manual_shipment_heads FOR SELECT TO commerce_checkout_writer USING(true);
CREATE POLICY manual_shipment_head_writer_insert ON fulfillment.manual_shipment_heads FOR INSERT TO commerce_checkout_writer
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
CREATE POLICY manual_shipment_head_writer_update ON fulfillment.manual_shipment_heads FOR UPDATE TO commerce_checkout_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);

-- Merchant read definers (owner commerce_auth): every column, unscoped SELECT policy (0027 pattern).
GRANT SELECT ON fulfillment.manual_shipment_versions, fulfillment.manual_shipment_heads TO commerce_auth;
CREATE POLICY manual_shipment_auth_read ON fulfillment.manual_shipment_versions FOR SELECT TO commerce_auth USING(true);
CREATE POLICY manual_shipment_head_auth_read ON fulfillment.manual_shipment_heads FOR SELECT TO commerce_auth USING(true);

-- Buyer (commerce_checkout_runtime): never note, void_reason or principal_id (E3 adds the head columns).
GRANT SELECT(tenant_id,store_id,owner_id,order_id,version,status,carrier_code,carrier_name,tracking_number,tracking_url,recorded_at)
 ON fulfillment.manual_shipment_versions TO commerce_checkout_runtime;
GRANT SELECT(tenant_id,store_id,owner_id,order_id,current_version) ON fulfillment.manual_shipment_heads TO commerce_checkout_runtime;
CREATE POLICY manual_shipment_buyer_read ON fulfillment.manual_shipment_versions FOR SELECT TO commerce_checkout_runtime
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND owner_id=nullif(current_setting('app.buyer_id',true),'')::uuid);
CREATE POLICY manual_shipment_head_buyer_read ON fulfillment.manual_shipment_heads FOR SELECT TO commerce_checkout_runtime
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND owner_id=nullif(current_setting('app.buyer_id',true),'')::uuid);

-- ---------------------------------------------------------------------------------------
-- Triggers: append-only versions, monotone heads, deferred head <=> order-state agreement.
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION fulfillment.guard_manual_shipment_immutable() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 RAISE EXCEPTION 'manual shipment history is append-only' USING ERRCODE='42501';
END $$;
-- Trigger functions need no EXECUTE to fire. PUBLIC EXECUTE would also trip the Stripe registrar/ingress
-- pool validator (platform.validateStripeAuthority rejects any reachable PUBLIC-executable application function).
REVOKE ALL ON FUNCTION fulfillment.guard_manual_shipment_immutable() FROM PUBLIC;
CREATE TRIGGER manual_shipment_versions_append_only BEFORE UPDATE OR DELETE ON fulfillment.manual_shipment_versions
 FOR EACH ROW EXECUTE FUNCTION fulfillment.guard_manual_shipment_immutable();
CREATE TRIGGER manual_shipment_versions_no_truncate BEFORE TRUNCATE ON fulfillment.manual_shipment_versions
 FOR EACH STATEMENT EXECUTE FUNCTION fulfillment.guard_manual_shipment_immutable();
CREATE TRIGGER manual_shipment_heads_no_delete BEFORE DELETE ON fulfillment.manual_shipment_heads
 FOR EACH ROW EXECUTE FUNCTION fulfillment.guard_manual_shipment_immutable();
CREATE TRIGGER manual_shipment_heads_no_truncate BEFORE TRUNCATE ON fulfillment.manual_shipment_heads
 FOR EACH STATEMENT EXECUTE FUNCTION fulfillment.guard_manual_shipment_immutable();

CREATE FUNCTION fulfillment.guard_manual_shipment_head() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id OR NEW.store_id IS DISTINCT FROM OLD.store_id
  OR NEW.owner_id IS DISTINCT FROM OLD.owner_id OR NEW.order_id IS DISTINCT FROM OLD.order_id
  OR NEW.current_version<>OLD.current_version+1 OR NEW.updated_at<OLD.updated_at THEN
  RAISE EXCEPTION 'manual shipment head must advance by exactly one version' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
REVOKE ALL ON FUNCTION fulfillment.guard_manual_shipment_head() FROM PUBLIC;
CREATE TRIGGER manual_shipment_heads_monotone BEFORE UPDATE ON fulfillment.manual_shipment_heads
 FOR EACH ROW EXECUTE FUNCTION fulfillment.guard_manual_shipment_head();

-- Head SHIPPED <=> order MERCHANT_SHIPPED, head = newest version, MERCHANT_SHIPPED only on CONFIRMED.
-- DEFINER (owner commerce_checkout_writer) so the check sees the rows whoever wrote either table.
CREATE FUNCTION fulfillment.guard_manual_shipment_state() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_tenant uuid; v_store uuid; v_order uuid; v_state text; v_commercial text;
 v_current bigint; v_status text; v_max bigint;
BEGIN
 v_tenant:=NEW.tenant_id; v_store:=NEW.store_id;
 IF TG_TABLE_NAME='orders' THEN v_order:=NEW.id; ELSE v_order:=NEW.order_id; END IF;
 SELECT o.fulfillment_state,o.commercial_state INTO v_state,v_commercial FROM checkout.orders o
  WHERE o.tenant_id=v_tenant AND o.store_id=v_store AND o.id=v_order;
 IF NOT FOUND THEN RETURN NULL; END IF;
 SELECT h.current_version INTO v_current FROM fulfillment.manual_shipment_heads h
  WHERE h.tenant_id=v_tenant AND h.store_id=v_store AND h.order_id=v_order;
 IF NOT FOUND THEN
  IF v_state='MERCHANT_SHIPPED' THEN
   RAISE EXCEPTION 'MERCHANT_SHIPPED order has no shipment head' USING ERRCODE='23514';
  END IF;
  RETURN NULL;
 END IF;
 SELECT v.status INTO v_status FROM fulfillment.manual_shipment_versions v
  WHERE v.tenant_id=v_tenant AND v.store_id=v_store AND v.order_id=v_order AND v.version=v_current;
 SELECT max(v.version) INTO v_max FROM fulfillment.manual_shipment_versions v
  WHERE v.tenant_id=v_tenant AND v.store_id=v_store AND v.order_id=v_order;
 IF v_status IS NULL OR v_max IS DISTINCT FROM v_current OR (v_status='SHIPPED')<>(v_state='MERCHANT_SHIPPED')
  OR (v_state='MERCHANT_SHIPPED' AND v_commercial<>'CONFIRMED') THEN
  RAISE EXCEPTION 'shipment head and order fulfillment state disagree' USING ERRCODE='23514';
 END IF;
 RETURN NULL;
END $$;
ALTER FUNCTION fulfillment.guard_manual_shipment_state() OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION fulfillment.guard_manual_shipment_state() FROM PUBLIC;
CREATE CONSTRAINT TRIGGER manual_shipment_state_orders AFTER UPDATE OF fulfillment_state ON checkout.orders
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
 WHEN (OLD.fulfillment_state='MERCHANT_SHIPPED' OR NEW.fulfillment_state='MERCHANT_SHIPPED')
 EXECUTE FUNCTION fulfillment.guard_manual_shipment_state();
CREATE CONSTRAINT TRIGGER manual_shipment_state_heads AFTER INSERT OR UPDATE ON fulfillment.manual_shipment_heads
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
 EXECUTE FUNCTION fulfillment.guard_manual_shipment_state();

-- ---------------------------------------------------------------------------------------
-- MD6 shipping eligibility, written ONCE. SECURITY INVOKER: record_manual_shipment runs it as
-- commerce_checkout_writer (GUC-scoped policies) and the projection/export as commerce_auth
-- (0027 policies + the column grants below), so it never widens either role.
-- ---------------------------------------------------------------------------------------
GRANT SELECT(reason) ON payments.review_cases TO commerce_auth;   -- E8: existing merchant_order_projection policy

CREATE FUNCTION fulfillment.manual_shipment_eligible(p_tenant uuid,p_store uuid,p_order uuid)
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
   AND o.commercial_state='CONFIRMED' AND o.fulfillment_state='MANUAL_UNASSIGNED'
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
$$;
ALTER FUNCTION fulfillment.manual_shipment_eligible(uuid,uuid,uuid) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION fulfillment.manual_shipment_eligible(uuid,uuid,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION fulfillment.manual_shipment_eligible(uuid,uuid,uuid) TO commerce_checkout_writer,commerce_auth;

-- ---------------------------------------------------------------------------------------
-- Fresh final authorization shared by the two new commerce_auth definers: one VOLATILE statement
-- after every data wait (0027 pattern; a nested STABLE resolve_access would keep the outer snapshot).
-- Returns NULL when access still holds, else the PT error code. Callable only by its owner.
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION identity.merchant_access_denied(p_hash bytea,p_store uuid,p_permissions text[],
 p_tenant uuid,p_principal uuid,p_revision bigint)
RETURNS text LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT CASE WHEN p.id IS NULL THEN 'PT401'
   WHEN st.id IS NULL OR t.id IS NULL OR m.principal_id IS NULL OR gr.permission IS NULL THEN 'PT404'
   WHEN st.tenant_id<>p_tenant OR p.id<>p_principal OR m.authz_revision<>p_revision
     OR EXISTS(SELECT 1 FROM unnest(p_permissions) req WHERE NOT EXISTS(SELECT 1 FROM identity.store_grants g
       WHERE g.tenant_id=st.tenant_id AND g.store_id=st.id AND g.principal_id=p.id AND g.permission=req)) THEN 'PT403'
   ELSE NULL END
 FROM (VALUES(1)) gate(n)
 LEFT JOIN identity.sessions login ON login.token_hash=p_hash AND login.audience='merchant'
   AND login.revoked_at IS NULL AND login.expires_at>clock_timestamp()
 LEFT JOIN identity.principals p ON p.id=login.principal_id AND p.active
 LEFT JOIN control.stores st ON st.id=p_store AND st.active
 LEFT JOIN control.tenants t ON t.id=st.tenant_id AND t.active
 LEFT JOIN identity.memberships m ON m.tenant_id=st.tenant_id AND m.principal_id=p.id AND m.active
 LEFT JOIN identity.store_grants gr ON gr.tenant_id=st.tenant_id AND gr.store_id=st.id
   AND gr.principal_id=p.id AND gr.permission='store:read'
$$;
ALTER FUNCTION identity.merchant_access_denied(bytea,uuid,text[],uuid,uuid,bigint) OWNER TO commerce_auth;
REVOKE ALL ON FUNCTION identity.merchant_access_denied(bytea,uuid,text[],uuid,uuid,bigint) FROM PUBLIC;

-- ---------------------------------------------------------------------------------------
-- Grants outside the tables: commerce_auth export audit insert (E4) and schema USAGE (A2).
-- commerce_checkout_writer's identity/ops grants come from 0062 (stripe-refund-v1 §4.5 rows 1-3).
-- ---------------------------------------------------------------------------------------
GRANT USAGE ON SCHEMA ops TO commerce_auth;
GRANT INSERT ON ops.audit_events TO commerce_auth;
CREATE POLICY auth_export_audit ON ops.audit_events FOR INSERT TO commerce_auth
 WITH CHECK(action='orders.export_unshipped'
  AND tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND principal_id=nullif(current_setting('app.principal_id',true),'')::uuid);

-- ---------------------------------------------------------------------------------------
-- record_manual_shipment: the single command (record / correct / void).
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION fulfillment.record_manual_shipment(p_hash bytea,p_store uuid,p_order uuid,p_key text,
 p_request_hash bytea,p_expected_version bigint,p_status text,p_carrier_code text,p_carrier_name text,
 p_tracking_number text,p_tracking_url text,p_note text,p_void_reason text)
RETURNS jsonb LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE s record; v_final record; v_expiry timestamptz; v_owner uuid; v_fulfillment text;
 v_head bigint; v_head_status text; v_prev record; v_row record; v_now timestamptz; v_action text;
 v_saved bytea; v_response jsonb; v_replay boolean:=false; v_fail_code text; v_fail_msg text;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR p_order IS NULL
  OR p_key IS NULL OR p_key !~ '^[A-Za-z0-9_.:-]{8,128}$'
  OR p_request_hash IS NULL OR octet_length(p_request_hash)<>32
  OR p_expected_version IS NULL OR p_expected_version<0 OR p_expected_version>=9223372036854775807
  OR p_status IS NULL OR p_status NOT IN ('SHIPPED','VOIDED')
  OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid shipment request' USING ERRCODE='PT400'; END IF;
 -- Authorize before any lock; the GUCs below then come from this result, never from the caller.
 SELECT * INTO s FROM identity.resolve_access(p_hash,p_store,'fulfillment:write');
 IF s.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF s.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF s.access_status IS DISTINCT FROM 'ok' THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 PERFORM set_config('app.tenant_id',s.tenant_id::text,true),set_config('app.store_id',p_store::text,true),
  set_config('app.principal_id',s.principal_id::text,true);
 -- State-independent body rules (void-body rule, contract §5.1): no lock and no state involved.
 IF p_status='VOIDED' THEN
  IF p_carrier_code IS NOT NULL OR p_carrier_name IS NOT NULL OR p_tracking_number IS NOT NULL
   OR p_tracking_url IS NOT NULL OR p_note IS NOT NULL OR p_void_reason IS NULL
   OR p_void_reason NOT IN ('wrong_order','wrong_tracking','not_dispatched','other') THEN
   RAISE EXCEPTION 'invalid_void' USING ERRCODE='PT422'; END IF;
 ELSE
  IF p_void_reason IS NOT NULL THEN RAISE EXCEPTION 'invalid_void' USING ERRCODE='PT422'; END IF;
  IF p_carrier_code IS NULL OR p_carrier_code NOT IN ('seven_eleven_cvs','familymart_cvs','hilife_cvs','okmart_cvs',
   'sf_express','chunghwa_post','other') OR (p_carrier_code='other' AND p_carrier_name IS NULL) THEN
   RAISE EXCEPTION 'invalid_carrier' USING ERRCODE='PT422'; END IF;
  IF p_tracking_number IS NULL OR p_tracking_number !~ '^[A-Za-z0-9][A-Za-z0-9 -]{0,63}$' OR p_tracking_number ~ ' $' THEN
   RAISE EXCEPTION 'invalid_tracking' USING ERRCODE='PT422'; END IF;
  IF p_tracking_url IS NOT NULL AND (octet_length(p_tracking_url)>512 OR p_tracking_url !~ '^https://([a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?\.)+[a-zA-Z]([a-zA-Z0-9-]*[a-zA-Z0-9])?([/?][!-~]*)?$') THEN
   RAISE EXCEPTION 'invalid_url' USING ERRCODE='PT422'; END IF;
 END IF;

 -- Lock order (contract §4.1): order -> work/refund reads (no lock: a refund request holds this same
 -- order lock, so held capacity cannot grow under us) -> head.
 SELECT o.owner_id,o.fulfillment_state INTO v_owner,v_fulfillment FROM checkout.orders o
  WHERE o.tenant_id=s.tenant_id AND o.store_id=p_store AND o.id=p_order FOR UPDATE;
 IF NOT FOUND THEN
  v_fail_code:='PT404'; v_fail_msg:='order not found';
 ELSE
  PERFORM set_config('app.buyer_id',v_owner::text,true);
  SELECT c.request_hash,c.response INTO v_saved,v_response FROM ops.command_results c
   WHERE c.tenant_id=s.tenant_id AND c.store_id=p_store
    AND c.operation='fulfillment.manual_shipment.record' AND c.idempotency_key=p_key;
  IF FOUND THEN
   IF v_saved<>p_request_hash THEN v_fail_code:='PT409'; v_fail_msg:='idempotency_conflict';
   ELSE v_replay:=true; END IF;
  ELSE
   SELECT h.current_version INTO v_head FROM fulfillment.manual_shipment_heads h
    WHERE h.tenant_id=s.tenant_id AND h.store_id=p_store AND h.order_id=p_order FOR UPDATE;
   IF v_head IS NOT NULL THEN
    SELECT v.status INTO v_head_status FROM fulfillment.manual_shipment_versions v
     WHERE v.tenant_id=s.tenant_id AND v.store_id=p_store AND v.order_id=p_order AND v.version=v_head;
   END IF;
   IF coalesce(v_head,0)<>p_expected_version THEN
    v_fail_code:='PT409'; v_fail_msg:='version_changed';
   ELSIF p_status='VOIDED' THEN
    IF v_head_status IS DISTINCT FROM 'SHIPPED' OR v_fulfillment<>'MERCHANT_SHIPPED' THEN
     v_fail_code:='PT422'; v_fail_msg:='void_requires_shipped';
    ELSE v_action:='fulfillment.shipment_voided'; END IF;
   ELSIF v_head_status='SHIPPED' AND v_fulfillment='MERCHANT_SHIPPED' THEN
    v_action:='fulfillment.shipment_corrected';   -- MD7: correction needs no MD6 re-check
   ELSIF NOT fulfillment.manual_shipment_eligible(s.tenant_id,p_store,p_order) THEN
    v_fail_code:='PT422'; v_fail_msg:='not_shippable';
   ELSE v_action:='fulfillment.shipment_recorded'; END IF;
  END IF;
 END IF;

 IF v_fail_code IS NULL AND NOT v_replay THEN
  IF p_status='VOIDED' THEN
   -- A void copies the carrier and tracking of the version it voids (history reads without joins).
   SELECT v.carrier_code,v.carrier_name,v.tracking_number,v.tracking_url INTO v_prev
    FROM fulfillment.manual_shipment_versions v
    WHERE v.tenant_id=s.tenant_id AND v.store_id=p_store AND v.order_id=p_order AND v.version=v_head;
   INSERT INTO fulfillment.manual_shipment_versions(tenant_id,store_id,owner_id,order_id,version,status,
    carrier_code,carrier_name,tracking_number,tracking_url,note,void_reason,principal_id)
   VALUES(s.tenant_id,p_store,v_owner,p_order,v_head+1,'VOIDED',v_prev.carrier_code,v_prev.carrier_name,
    v_prev.tracking_number,v_prev.tracking_url,NULL,p_void_reason,s.principal_id)
   RETURNING * INTO v_row;
  ELSE
   INSERT INTO fulfillment.manual_shipment_versions(tenant_id,store_id,owner_id,order_id,version,status,
    carrier_code,carrier_name,tracking_number,tracking_url,note,void_reason,principal_id)
   VALUES(s.tenant_id,p_store,v_owner,p_order,coalesce(v_head,0)+1,'SHIPPED',p_carrier_code,p_carrier_name,
    p_tracking_number,p_tracking_url,p_note,NULL,s.principal_id)
   RETURNING * INTO v_row;
  END IF;
  v_now:=clock_timestamp();
  IF v_head IS NULL THEN
   INSERT INTO fulfillment.manual_shipment_heads(tenant_id,store_id,owner_id,order_id,current_version,updated_at)
   VALUES(s.tenant_id,p_store,v_owner,p_order,v_row.version,v_now);
  ELSE
   UPDATE fulfillment.manual_shipment_heads h SET current_version=v_row.version,updated_at=v_now
    WHERE h.tenant_id=s.tenant_id AND h.store_id=p_store AND h.order_id=p_order;
  END IF;
  -- §11.5 n/a: fulfilment state only; no stock, ledger, reservation or payment write (MD2, RD6).
  UPDATE checkout.orders o SET fulfillment_state=CASE WHEN p_status='SHIPPED' THEN 'MERCHANT_SHIPPED' ELSE 'MANUAL_UNASSIGNED' END,
   updated_at=v_now
   WHERE o.tenant_id=s.tenant_id AND o.store_id=p_store AND o.owner_id=v_owner AND o.id=p_order;
  INSERT INTO ops.audit_events(tenant_id,store_id,principal_id,action) VALUES(s.tenant_id,p_store,s.principal_id,v_action);
  v_response:=jsonb_build_object('version',v_row.version,'status',v_row.status,'carrier_code',v_row.carrier_code,
   'carrier_name',v_row.carrier_name,'tracking_number',v_row.tracking_number,'tracking_url',v_row.tracking_url,
   'recorded_at',to_char(v_row.recorded_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'note',v_row.note,'void_reason',v_row.void_reason,'principal_id',v_row.principal_id);
  INSERT INTO ops.command_results(tenant_id,store_id,operation,idempotency_key,request_hash,response,principal_id)
  VALUES(s.tenant_id,p_store,'fulfillment.manual_shipment.record',p_key,p_request_hash,v_response,s.principal_id);
 END IF;

 -- Final authority after every lock wait and write, also on the refusal paths (no post-revocation
 -- existence oracle). A failure here aborts the transaction, so no row survives.
 SELECT * INTO v_final FROM identity.resolve_access(p_hash,p_store,'fulfillment:write');
 SELECT se.expires_at INTO v_expiry FROM identity.sessions se
  WHERE se.token_hash=p_hash AND se.audience='merchant' AND se.revoked_at IS NULL;
 IF v_final.access_status='unauthorized' OR v_expiry IS NULL OR v_expiry<=clock_timestamp() THEN
  RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF v_final.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF v_final.access_status<>'ok' OR v_final.tenant_id IS DISTINCT FROM s.tenant_id
  OR v_final.principal_id IS DISTINCT FROM s.principal_id
  OR v_final.authz_revision IS DISTINCT FROM s.authz_revision THEN
  RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 IF v_fail_code IS NOT NULL THEN RAISE EXCEPTION '%',v_fail_msg USING ERRCODE=v_fail_code; END IF;
 RETURN v_response;
END $$;
ALTER FUNCTION fulfillment.record_manual_shipment(bytea,uuid,uuid,text,bytea,bigint,text,text,text,text,text,text,text) OWNER TO commerce_checkout_writer;
REVOKE ALL ON FUNCTION fulfillment.record_manual_shipment(bytea,uuid,uuid,text,bytea,bigint,text,text,text,text,text,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION fulfillment.record_manual_shipment(bytea,uuid,uuid,text,bytea,bigint,text,text,text,text,text,text,text) TO commerce_runtime;

-- ---------------------------------------------------------------------------------------
-- Merchant shipment history (orders:read): all versions ascending incl. merchant-only fields.
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION fulfillment.read_manual_shipment_history(p_hash bytea,p_store uuid,p_order uuid)
RETURNS jsonb LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE s record; v_exists boolean; v_items jsonb; v_err text;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR p_order IS NULL
  OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid shipment history read' USING ERRCODE='PT400'; END IF;
 SELECT * INTO s FROM identity.resolve_access(p_hash,p_store,'orders:read');
 IF s.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF s.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF s.access_status IS DISTINCT FROM 'ok' THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 IF current_setting('app.tenant_id',true) IS DISTINCT FROM s.tenant_id::text
  OR current_setting('app.store_id',true) IS DISTINCT FROM p_store::text
  OR current_setting('app.principal_id',true) IS DISTINCT FROM s.principal_id::text THEN
  RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 SELECT EXISTS(SELECT 1 FROM checkout.orders o WHERE o.tenant_id=s.tenant_id AND o.store_id=p_store AND o.id=p_order),
  coalesce((SELECT jsonb_agg(jsonb_build_object('version',v.version,'status',v.status,'carrier_code',v.carrier_code,
   'carrier_name',v.carrier_name,'tracking_number',v.tracking_number,'tracking_url',v.tracking_url,
   'recorded_at',to_char(v.recorded_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
   'note',v.note,'void_reason',v.void_reason,'principal_id',v.principal_id) ORDER BY v.version)
   FROM fulfillment.manual_shipment_versions v
   WHERE v.tenant_id=s.tenant_id AND v.store_id=p_store AND v.order_id=p_order),'[]'::jsonb)
  INTO v_exists,v_items;
 v_err:=identity.merchant_access_denied(p_hash,p_store,ARRAY['orders:read'],s.tenant_id,s.principal_id,s.authz_revision);
 IF v_err IS NOT NULL THEN RAISE EXCEPTION 'shipment history access denied' USING ERRCODE=v_err; END IF;
 IF NOT v_exists THEN RAISE EXCEPTION 'order not found' USING ERRCODE='PT404'; END IF;
 RETURN v_items;
END $$;
ALTER FUNCTION fulfillment.read_manual_shipment_history(bytea,uuid,uuid) OWNER TO commerce_auth;
REVOKE ALL ON FUNCTION fulfillment.read_manual_shipment_history(bytea,uuid,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION fulfillment.read_manual_shipment_history(bytea,uuid,uuid) TO commerce_runtime;

-- ---------------------------------------------------------------------------------------
-- Unshipped export source (orders:export AND orders:read): one snapshot, fresh final auth, audit row.
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION identity.export_unshipped_orders(p_hash bytea,p_store uuid,p_row_limit integer)
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
   'total_minor',o.total_minor,'currency',o.currency) AS j
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
ALTER FUNCTION identity.export_unshipped_orders(bytea,uuid,integer) OWNER TO commerce_auth;
REVOKE ALL ON FUNCTION identity.export_unshipped_orders(bytea,uuid,integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION identity.export_unshipped_orders(bytea,uuid,integer) TO commerce_runtime;

-- ---------------------------------------------------------------------------------------
-- Merchant order projection successor (E1): 0027 body + refund fields (stripe-refund-v1 §7.1)
-- + shipment fields (manual-fulfilment-v1 §4.1). Same signature, owner, ACL and auth rules.
-- ---------------------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION identity.read_merchant_orders(p_hash bytea,p_store uuid,p_order uuid,
 p_limit integer,p_after_created_at timestamptz,p_after_id uuid,p_state text)
RETURNS jsonb LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE s record; v_result jsonb; v_auth_error text;
BEGIN
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL
 OR p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 101
 OR p_state IS NULL OR p_state NOT IN ('all','DRAFT','AWAITING_PAYMENT','CONFIRMED','CANCELLED','shipped','unshipped')
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
   CASE WHEN p_order IS NULL THEN NULL ELSE o.snapshot END AS snapshot
  FROM checkout.orders o WHERE o.tenant_id=s.tenant_id AND o.store_id=p_store
   AND (p_order IS NULL OR o.id=p_order)
   AND (p_state='all'
    OR (p_state IN ('DRAFT','AWAITING_PAYMENT','CONFIRMED','CANCELLED') AND o.commercial_state=p_state)
    OR (p_state='shipped' AND o.fulfillment_state='MERCHANT_SHIPPED')
    -- MD6: the same predicate the record command and the export use.
    OR (p_state='unshipped' AND o.commercial_state='CONFIRMED' AND o.fulfillment_state='MANUAL_UNASSIGNED'
      AND fulfillment.manual_shipment_eligible(o.tenant_id,o.store_id,o.id)))
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
ALTER FUNCTION identity.read_merchant_orders(bytea,uuid,uuid,integer,timestamptz,uuid,text) OWNER TO commerce_auth;
REVOKE ALL ON FUNCTION identity.read_merchant_orders(bytea,uuid,uuid,integer,timestamptz,uuid,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION identity.read_merchant_orders(bytea,uuid,uuid,integer,timestamptz,uuid,text) TO commerce_runtime;

-- ---------------------------------------------------------------------------------------
-- Documentation (PROCESS §5): owning package, allowed roles, non-goals.
-- ---------------------------------------------------------------------------------------
COMMENT ON TABLE fulfillment.manual_shipment_versions IS
 'internal/merchantorders: append-only merchant-arranged shipment versions (SHIPPED|VOIDED); attestation only, never in-transit or delivered evidence (MD3). Written by fulfillment.record_manual_shipment (owner commerce_checkout_writer) only; read by commerce_auth (merchant, all columns) and commerce_checkout_runtime (buyer, no note/void_reason/principal_id). No UPDATE/DELETE for any role.';
COMMENT ON TABLE fulfillment.manual_shipment_heads IS
 'internal/merchantorders: current version pointer per order (CAS target of expected_version). Written by fulfillment.record_manual_shipment only; head SHIPPED <=> checkout.orders.fulfillment_state=MERCHANT_SHIPPED (deferred guard). Buyer role reads only owner/order/current_version.';
COMMENT ON COLUMN fulfillment.manual_shipment_versions.note IS 'Merchant-only free text (<=200 chars); never returned to buyers. Personal-data class: merchant notes.';
COMMENT ON COLUMN fulfillment.manual_shipment_versions.principal_id IS 'Recording merchant principal (audit); merchant-only, returned as UUID only.';
COMMENT ON COLUMN fulfillment.manual_shipment_versions.tracking_url IS 'Merchant-supplied https URL (contract §3.2); never fetched by any server code.';
COMMENT ON COLUMN fulfillment.manual_shipment_versions.tenant_id IS 'Owning tenant; RLS scope key. Set by record_manual_shipment from the authenticated scope, never from the client.';
COMMENT ON COLUMN fulfillment.manual_shipment_versions.store_id IS 'Owning store; RLS scope key. Set by record_manual_shipment from the authenticated scope, never from the client.';
COMMENT ON COLUMN fulfillment.manual_shipment_versions.owner_id IS 'Buyer owner of the order (FK to checkout.orders); lets the buyer role read its own SHIPPED version. Never client-supplied.';
COMMENT ON COLUMN fulfillment.manual_shipment_versions.order_id IS 'Shipped order (FK to checkout.orders); one version chain per order.';
COMMENT ON COLUMN fulfillment.manual_shipment_versions.version IS '1-based version within the order; head.current_version points at the newest (CAS target of expected_version).';
COMMENT ON COLUMN fulfillment.manual_shipment_versions.status IS 'SHIPPED (merchant attests dispatch) or VOIDED (retracts the previous SHIPPED); no in-transit/delivered states (MD3).';
COMMENT ON COLUMN fulfillment.manual_shipment_versions.carrier_code IS 'Closed carrier vocabulary (contract §3.1); other requires carrier_name.';
COMMENT ON COLUMN fulfillment.manual_shipment_versions.carrier_name IS 'Merchant-typed carrier label, NFC-normalized in Go (ruling 20), 1..80 chars, no control characters; only with carrier_code other or as a display override.';
COMMENT ON COLUMN fulfillment.manual_shipment_versions.tracking_number IS 'Merchant-typed tracking number (A-Z, 0-9, space, dash; <=64); shown to the buyer; never validated against a carrier.';
COMMENT ON COLUMN fulfillment.manual_shipment_versions.void_reason IS 'Closed reason, present exactly when status=VOIDED; merchant-only.';
COMMENT ON COLUMN fulfillment.manual_shipment_versions.recorded_at IS 'Server clock_timestamp() of the version insert; never client-supplied.';
COMMENT ON COLUMN fulfillment.manual_shipment_heads.tenant_id IS 'Owning tenant; RLS scope key (same as the versions row).';
COMMENT ON COLUMN fulfillment.manual_shipment_heads.store_id IS 'Owning store; RLS scope key (same as the versions row).';
COMMENT ON COLUMN fulfillment.manual_shipment_heads.owner_id IS 'Buyer owner of the order; buyer role reads its own head through the owner-scope policy (ruling E3).';
COMMENT ON COLUMN fulfillment.manual_shipment_heads.order_id IS 'Order whose current shipment version this row points at; one head per order.';
COMMENT ON COLUMN fulfillment.manual_shipment_heads.current_version IS 'Newest version of the order; advances by exactly one per accepted command (guard_manual_shipment_head).';
COMMENT ON COLUMN fulfillment.manual_shipment_heads.updated_at IS 'Server time of the last head advance; never client-supplied.';
COMMENT ON FUNCTION fulfillment.guard_manual_shipment_immutable() IS 'fulfillment package trigger guard: versions are append-only, heads are never deleted or truncated.';
COMMENT ON FUNCTION fulfillment.guard_manual_shipment_head() IS 'fulfillment package trigger guard: heads advance by exactly one version and never change identity columns.';
COMMENT ON FUNCTION fulfillment.guard_manual_shipment_state() IS 'Deferred constraint trigger body on checkout.orders and manual_shipment_heads: head SHIPPED <=> order MERCHANT_SHIPPED, head is the newest version, MERCHANT_SHIPPED only for CONFIRMED orders. DEFINER (commerce_checkout_writer).';
COMMENT ON FUNCTION fulfillment.manual_shipment_eligible(uuid,uuid,uuid) IS
 'MD6 shipping eligibility, single source for record_manual_shipment, the unshipped list filter and the export. SECURITY INVOKER: runs with the caller definer''s own grants (commerce_checkout_writer, commerce_auth). Non-goal: does not authorize anyone.';
COMMENT ON FUNCTION identity.merchant_access_denied(bytea,uuid,text[],uuid,uuid,bigint) IS
 'Fresh final merchant authorization (one VOLATILE statement) for the commerce_auth definers export_unshipped_orders and read_manual_shipment_history. Owner-only EXECUTE; returns NULL or a PT401/PT403/PT404 code.';
COMMENT ON FUNCTION fulfillment.record_manual_shipment(bytea,uuid,uuid,text,bytea,bigint,text,text,text,text,text,text,text) IS
 'internal/merchantorders.RecordShipment only; EXECUTE commerce_runtime. fulfillment:write, order lock then head CAS, MD6 eligibility, replay via ops.command_results, audit row, no job/operation/provider call. Errors: PT409 version_changed|idempotency_conflict, PT422 not_shippable|invalid_*|void_requires_shipped.';
COMMENT ON FUNCTION fulfillment.read_manual_shipment_history(bytea,uuid,uuid) IS
 'internal/merchantorders.ShipmentHistory only; EXECUTE commerce_runtime. orders:read; all versions ascending including merchant-only fields.';
COMMENT ON FUNCTION identity.export_unshipped_orders(bytea,uuid,integer) IS
 'internal/merchantorders.ExportUnshipped only; EXECUTE commerce_runtime. orders:export AND orders:read; MD6 rows oldest first (limit 1..1001); writes one ops.audit_events row orders.export_unshipped; nothing else is stored.';
COMMENT ON FUNCTION identity.read_merchant_orders(bytea,uuid,uuid,integer,timestamptz,uuid,text) IS
 'internal/merchantorders.List/Get; merchant order projection (0027) with refund amounts (payment_state PARTIALLY_REFUNDED|REFUNDED) and manual shipment; single owner: fulfilment-core. state filters shipped|unshipped.';
COMMENT ON INDEX checkout.orders_unshipped IS 'Export/list scan for MD6 candidates (CONFIRMED + MANUAL_UNASSIGNED), oldest first.';
COMMENT ON POLICY manual_shipment_writer_select ON fulfillment.manual_shipment_versions IS 'commerce_checkout_writer definers filter tenant/store explicitly; unscoped read keeps the deferred state guard independent of GUCs.';
COMMENT ON POLICY auth_export_audit ON ops.audit_events IS 'commerce_auth may insert only the orders.export_unshipped audit row, scoped to the GUCs export_unshipped_orders sets from its authenticated result.';
