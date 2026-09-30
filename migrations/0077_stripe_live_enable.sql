-- 0077 Stripe LIVE per-store enablement (contracts/stripe-live-enable-v1.md v1 FROZEN 2026-09-30, §3, plus
-- r2-design-rulings brief ruling B5 = S5). Owner package: payments/stripeadmin (registrar definers),
-- payments (LIVE admission), checkout (buyer view); consumers: cmd/stripe-admin, cmd/payment-worker, cmd/api.
-- Non-goals: no money movement, no key material, no merchant-facing write, no PAYUNi change, no new role, no
-- new queue. Nothing here proves the owner consented: `approval_ref` points at the owner's written approval (LD2).
--
-- Every re-created function keeps its signature, owner, ACL and search_path; only the deltas named next to it
-- change. Lock order on all LIVE registrar paths: account -> binding -> approval -> qualification -> method head.
-- The post-River functions (start/signal/observations/refund) are re-created in post_river/0016_stripe_live.sql.

-- ---------------------------------------------------------------------------------------------------------
-- 3.1 helper: the closed Stripe minimum table next to stripe_amount_ok (0061:4). SL02 asserts they agree.
-- ---------------------------------------------------------------------------------------------------------
CREATE FUNCTION payments.stripe_min_minor(p_currency text) RETURNS bigint
LANGUAGE sql IMMUTABLE SET search_path=pg_catalog AS $$
 SELECT CASE p_currency WHEN 'HKD' THEN 400 WHEN 'USD' THEN 50 WHEN 'SGD' THEN 50 WHEN 'MYR' THEN 200
  -- TWD min 2500: Stripe SANDBOX rejected 100/1200, accepted 2500 (2026-09-29); live minimum UNKNOWN (§13)
  WHEN 'TWD' THEN 2500 ELSE NULL END
$$;
REVOKE ALL ON FUNCTION payments.stripe_min_minor(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION payments.stripe_min_minor(text) TO commerce_payment_registry_writer;
COMMENT ON FUNCTION payments.stripe_min_minor(text) IS 'payments owner; registry writer reads the closed per-currency Stripe minimum used by payments.approve_stripe_live caps; same table as stripe_amount_ok; no conversion, no provider guarantee';

-- ---------------------------------------------------------------------------------------------------------
-- 3.1 widened CHECKs, re-derived from pg_get_constraintdef (0061/0062 pattern). A changed shape stops the run.
-- ---------------------------------------------------------------------------------------------------------
DO $$
DECLARE v_def text;
BEGIN
 SELECT pg_get_constraintdef(c.oid) INTO v_def FROM pg_constraint c
  WHERE c.conrelid='payments.stripe_webhook_endpoints'::regclass AND c.conname='stripe_webhook_endpoints_environment_check';
 IF v_def IS NULL OR v_def<>'CHECK ((environment = ''SANDBOX''::text))' THEN
  RAISE EXCEPTION 'stripe_webhook_endpoints_environment_check has an unexpected shape: %',v_def; END IF;
 ALTER TABLE payments.stripe_webhook_endpoints DROP CONSTRAINT stripe_webhook_endpoints_environment_check;
 ALTER TABLE payments.stripe_webhook_endpoints ADD CONSTRAINT stripe_webhook_endpoints_environment_check
  CHECK(environment IN ('SANDBOX','LIVE'));

 SELECT pg_get_constraintdef(c.oid) INTO v_def FROM pg_constraint c
  WHERE c.conrelid='payments.stripe_webhook_endpoints'::regclass AND c.conname='stripe_webhook_endpoints_execution_profile_check';
 IF v_def IS NULL OR v_def NOT LIKE '%PROVIDER_MOCK%' OR v_def NOT LIKE '%SANDBOX%' OR v_def LIKE '%LIVE%' THEN
  RAISE EXCEPTION 'stripe_webhook_endpoints_execution_profile_check has an unexpected shape: %',v_def; END IF;
 ALTER TABLE payments.stripe_webhook_endpoints DROP CONSTRAINT stripe_webhook_endpoints_execution_profile_check;
 ALTER TABLE payments.stripe_webhook_endpoints ADD CONSTRAINT stripe_webhook_endpoints_execution_profile_check
  CHECK(execution_profile IN ('PROVIDER_MOCK','SANDBOX','LIVE'));
 ALTER TABLE payments.stripe_webhook_endpoints ADD CONSTRAINT stripe_endpoint_env_profile_check
  CHECK((environment='LIVE')=(execution_profile='LIVE'));

 SELECT pg_get_constraintdef(c.oid) INTO v_def FROM pg_constraint c
  WHERE c.conrelid='payments.stripe_refunds'::regclass AND c.conname='stripe_refunds_environment_check';
 IF v_def IS NULL OR v_def<>'CHECK ((environment = ''SANDBOX''::text))' THEN
  RAISE EXCEPTION 'stripe_refunds_environment_check has an unexpected shape: %',v_def; END IF;
 ALTER TABLE payments.stripe_refunds DROP CONSTRAINT stripe_refunds_environment_check;
 ALTER TABLE payments.stripe_refunds ADD CONSTRAINT stripe_refunds_environment_check
  CHECK(environment IN ('SANDBOX','LIVE'));
END $$;
COMMENT ON CONSTRAINT stripe_endpoint_env_profile_check ON payments.stripe_webhook_endpoints IS 'payments owner; a LIVE endpoint exists only with profile LIVE and vice versa (LD1); no mixed-profile ingress';

-- ---------------------------------------------------------------------------------------------------------
-- 3.2 payments.stripe_live_approvals: one small append-only record of the operator-attested owner approval.
-- ---------------------------------------------------------------------------------------------------------
CREATE TABLE payments.stripe_live_approvals (
 id uuid PRIMARY KEY,tenant_id uuid NOT NULL,store_id uuid NOT NULL,connection_id uuid NOT NULL,
 account_id text NOT NULL CHECK(account_id ~ '^acct_[A-Za-z0-9]{1,59}$'),
 environment text GENERATED ALWAYS AS ('LIVE') STORED,
 provider text GENERATED ALWAYS AS ('stripe') STORED,
 currency text NOT NULL CHECK(currency ~ '^[A-Z]{3}$'),
 approved_by uuid NOT NULL,
 approval_ref text NOT NULL CHECK(approval_ref ~ '^[A-Za-z0-9._:-]{8,128}$'),
 approved_at timestamptz NOT NULL,
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 canary_max_minor bigint NOT NULL CHECK(canary_max_minor>0),
 max_minor bigint NOT NULL CHECK(max_minor>=canary_max_minor),
 checklist text[] NOT NULL,
 account_readiness jsonb NOT NULL,
 canary_attempt_id uuid,canary_refund_id uuid,canary_verified_at timestamptz,
 revoked_at timestamptz,revoked_by uuid,
 revoke_ref text CHECK(revoke_ref ~ '^[A-Za-z0-9._:-]{8,128}$'),
 UNIQUE(tenant_id,store_id,id,connection_id),
 FOREIGN KEY(tenant_id,approved_by) REFERENCES identity.memberships(tenant_id,principal_id),
 FOREIGN KEY(tenant_id,revoked_by) REFERENCES identity.memberships(tenant_id,principal_id),
 FOREIGN KEY(tenant_id,store_id,connection_id,provider,environment,account_id)
  REFERENCES integration.merchant_accounts(tenant_id,store_id,id,provider,environment,account_id),
 CHECK(approved_at<=recorded_at),
 CHECK((canary_attempt_id IS NULL)=(canary_refund_id IS NULL) AND (canary_attempt_id IS NULL)=(canary_verified_at IS NULL)),
 CHECK((revoked_at IS NULL)=(revoked_by IS NULL) AND (revoked_at IS NULL)=(revoke_ref IS NULL))
);
CREATE UNIQUE INDEX stripe_live_one_active ON payments.stripe_live_approvals(connection_id) WHERE revoked_at IS NULL;
ALTER TABLE payments.stripe_live_approvals ENABLE ROW LEVEL SECURITY;
ALTER TABLE payments.stripe_live_approvals FORCE ROW LEVEL SECURITY;
REVOKE ALL ON payments.stripe_live_approvals FROM PUBLIC;
GRANT SELECT,INSERT ON payments.stripe_live_approvals TO commerce_payment_registry_writer;
GRANT UPDATE(canary_attempt_id,canary_refund_id,canary_verified_at,revoked_at,revoked_by,revoke_ref)
 ON payments.stripe_live_approvals TO commerce_payment_registry_writer;
-- No DELETE/TRUNCATE grant to any role; no runtime, worker, ingress or checkout privilege (runtime admission
-- reads the qualification, never this table).
CREATE POLICY stripe_live_approval_read ON payments.stripe_live_approvals FOR SELECT
 TO commerce_payment_registry_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
CREATE POLICY stripe_live_approval_insert ON payments.stripe_live_approvals FOR INSERT
 TO commerce_payment_registry_writer
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND approved_by=nullif(current_setting('app.principal_id',true),'')::uuid);
CREATE POLICY stripe_live_approval_update ON payments.stripe_live_approvals FOR UPDATE
 TO commerce_payment_registry_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);

CREATE FUNCTION payments.guard_stripe_live_approval() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 -- Every column is immutable except the canary triple and the revoke triple, each NULL -> value exactly once.
 -- (The generated environment/provider columns are computed after BEFORE triggers and cannot change.)
 IF to_jsonb(NEW)-ARRAY['canary_attempt_id','canary_refund_id','canary_verified_at','revoked_at','revoked_by','revoke_ref',
   'environment','provider']
  IS DISTINCT FROM
  to_jsonb(OLD)-ARRAY['canary_attempt_id','canary_refund_id','canary_verified_at','revoked_at','revoked_by','revoke_ref',
   'environment','provider']
  OR (OLD.canary_verified_at IS NOT NULL AND (NEW.canary_attempt_id,NEW.canary_refund_id,NEW.canary_verified_at)
   IS DISTINCT FROM (OLD.canary_attempt_id,OLD.canary_refund_id,OLD.canary_verified_at))
  OR (OLD.revoked_at IS NOT NULL AND (NEW.revoked_at,NEW.revoked_by,NEW.revoke_ref)
   IS DISTINCT FROM (OLD.revoked_at,OLD.revoked_by,OLD.revoke_ref)) THEN
  RAISE EXCEPTION 'Stripe live approval is immutable' USING ERRCODE='42501'; END IF;
 RETURN NEW;
END $$;
ALTER FUNCTION payments.guard_stripe_live_approval() OWNER TO commerce_payment_registry_writer;
REVOKE ALL ON FUNCTION payments.guard_stripe_live_approval() FROM PUBLIC;
CREATE TRIGGER guard_stripe_live_approval BEFORE UPDATE ON payments.stripe_live_approvals
 FOR EACH ROW EXECUTE FUNCTION payments.guard_stripe_live_approval();

-- ---------------------------------------------------------------------------------------------------------
-- 3.1 account_qualifications: a REAL_LIVE Stripe qualification must name an active approval of the same
-- connection (composite FK); SP19's REAL_LIVE-without-approval insert still fails.
-- ---------------------------------------------------------------------------------------------------------
ALTER TABLE payments.account_qualifications ADD COLUMN live_approval_id uuid;
ALTER TABLE payments.account_qualifications DROP CONSTRAINT stripe_qualification_no_live_check;
ALTER TABLE payments.account_qualifications ADD CONSTRAINT stripe_qualification_live_needs_approval_check
 CHECK(code<>'stripe_checkout' OR proof_class<>'REAL_LIVE' OR live_approval_id IS NOT NULL);
ALTER TABLE payments.account_qualifications ADD CONSTRAINT stripe_qualification_approval_shape_check
 CHECK(live_approval_id IS NULL OR (code='stripe_checkout' AND proof_class='REAL_LIVE'));
ALTER TABLE payments.account_qualifications ADD CONSTRAINT stripe_qualification_live_approval_fk
 FOREIGN KEY(tenant_id,store_id,live_approval_id,connection_id)
 REFERENCES payments.stripe_live_approvals(tenant_id,store_id,id,connection_id);

-- Revoke-only: today no path updates a qualification (only UPDATE(id) row locks, 0061:204 / 0016:113). The only
-- permitted change, for every role, is revoked_at NULL -> now on a LIVE-approved Stripe row.
CREATE FUNCTION payments.account_qualification_revoke_only() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 IF NEW IS NOT DISTINCT FROM OLD THEN RETURN NEW; END IF; -- UPDATE(id) row-lock no-ops change nothing
 IF OLD.code='stripe_checkout' AND OLD.live_approval_id IS NOT NULL
  AND OLD.revoked_at IS NULL AND NEW.revoked_at IS NOT NULL
  AND to_jsonb(NEW)-'revoked_at' IS NOT DISTINCT FROM to_jsonb(OLD)-'revoked_at' THEN
  NEW.revoked_at:=clock_timestamp(); -- the supplied value is ignored
  RETURN NEW;
 END IF;
 RAISE EXCEPTION 'Stripe qualification is revoke-only' USING ERRCODE='PT409';
END $$;
ALTER FUNCTION payments.account_qualification_revoke_only() OWNER TO commerce_payment_registry_writer;
REVOKE ALL ON FUNCTION payments.account_qualification_revoke_only() FROM PUBLIC;
CREATE TRIGGER account_qualification_revoke_only BEFORE UPDATE ON payments.account_qualifications
 FOR EACH ROW EXECUTE FUNCTION payments.account_qualification_revoke_only();

GRANT UPDATE(revoked_at) ON payments.account_qualifications TO commerce_payment_registry_writer;
-- The existing stripe_registry_qualification_lock policy (WITH CHECK(false)) stays for every other row.
CREATE POLICY stripe_registry_qualification_revoke ON payments.account_qualifications
 FOR UPDATE TO commerce_payment_registry_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND code='stripe_checkout' AND live_approval_id IS NOT NULL)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND code='stripe_checkout' AND live_approval_id IS NOT NULL);

-- ---------------------------------------------------------------------------------------------------------
-- 3.3 registry writer read access for the canary check only (scoped by the pinned GUCs, column-limited).
-- ---------------------------------------------------------------------------------------------------------
GRANT USAGE ON SCHEMA checkout TO commerce_payment_registry_writer;
GRANT SELECT(tenant_id,store_id,id,method_code,environment,connection_id,currency,qualification_id,created_at)
 ON checkout.payment_attempts TO commerce_payment_registry_writer;
GRANT SELECT(tenant_id,store_id,attempt_id,kind,amount_minor,currency)
 ON payments.facts TO commerce_payment_registry_writer;
GRANT SELECT(tenant_id,store_id,id,attempt_id,environment)
 ON payments.stripe_refunds TO commerce_payment_registry_writer;
GRANT SELECT(tenant_id,store_id,refund_id,attempt_id,kind,amount_minor,currency)
 ON payments.refund_facts TO commerce_payment_registry_writer;
GRANT SELECT(tenant_id,store_id,attempt_id)
 ON payments.review_cases TO commerce_payment_registry_writer;
GRANT SELECT(tenant_id,store_id,endpoint_id,attempt_id,refund_id,disposition)
 ON payments.stripe_webhook_receipts TO commerce_payment_registry_writer;
CREATE POLICY stripe_registry_attempt_read ON checkout.payment_attempts FOR SELECT
 TO commerce_payment_registry_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
CREATE POLICY stripe_registry_fact_read ON payments.facts FOR SELECT TO commerce_payment_registry_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
CREATE POLICY stripe_registry_refund_read ON payments.stripe_refunds FOR SELECT
 TO commerce_payment_registry_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
CREATE POLICY stripe_registry_refund_fact_read ON payments.refund_facts FOR SELECT
 TO commerce_payment_registry_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
CREATE POLICY stripe_registry_review_read ON payments.review_cases FOR SELECT
 TO commerce_payment_registry_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);
CREATE POLICY stripe_registry_receipt_read ON payments.stripe_webhook_receipts FOR SELECT
 TO commerce_payment_registry_writer
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid);

-- ---------------------------------------------------------------------------------------------------------
-- 3.4 new registrar definers (owner registry writer, EXECUTE registrar only, scope + one audit row each).
-- ---------------------------------------------------------------------------------------------------------
CREATE FUNCTION payments.approve_stripe_live(p_tenant uuid,p_store uuid,p_principal uuid,p_approval uuid,
 p_connection uuid,p_currency text,p_approval_ref text,p_approved_at timestamptz,p_canary_max bigint,
 p_max bigint,p_checklist text[],p_readiness jsonb) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE acct integration.merchant_accounts%ROWTYPE; b integration.bindings%ROWTYPE;
 ap payments.stripe_live_approvals%ROWTYPE; v_sorted text[]; v_key text; v_min bigint; v_now timestamptz;
BEGIN
 -- integration:manage + active membership + active store, and the GUC scope for RLS and the audit row
 PERFORM integration.require_stripe_registrar_scope(p_tenant,p_store,p_principal);
 IF p_approval IS NULL OR p_connection IS NULL OR p_currency IS NULL OR p_currency !~ '^[A-Z]{3}$'
  OR p_approval_ref IS NULL OR p_approval_ref !~ '^[A-Za-z0-9._:-]{8,128}$'
  OR p_approved_at IS NULL OR p_canary_max IS NULL OR p_max IS NULL OR p_checklist IS NULL
  OR p_readiness IS NULL OR jsonb_typeof(p_readiness)<>'object' THEN
  RAISE EXCEPTION 'invalid Stripe live approval input' USING ERRCODE='22023'; END IF;
 -- LD2: the approver is defined by a grant predicate (0065 store-creator set): integration:manage (above) AND payments:refund
 IF NOT EXISTS(SELECT 1 FROM identity.store_grants g WHERE g.tenant_id=p_tenant AND g.store_id=p_store
   AND g.principal_id=p_principal AND g.permission='payments:refund') THEN
  RAISE EXCEPTION 'Stripe live approver lacks payments:refund' USING ERRCODE='42501'; END IF;
 -- lock order: account -> binding -> approval
 SELECT x.* INTO acct FROM integration.merchant_accounts x WHERE x.tenant_id=p_tenant
  AND x.store_id=p_store AND x.id=p_connection AND x.provider='stripe' FOR SHARE;
 IF NOT FOUND OR acct.environment<>'LIVE' THEN
  RAISE EXCEPTION 'Stripe live account unavailable' USING ERRCODE='PT409'; END IF;
 SELECT x.* INTO b FROM integration.bindings x WHERE x.tenant_id=p_tenant AND x.store_id=p_store
  AND x.id=acct.binding_id FOR SHARE;
 IF NOT FOUND OR NOT b.enabled OR b.provider<>'stripe' OR b.external_asset_id<>acct.binding_asset THEN
  RAISE EXCEPTION 'Stripe binding unavailable' USING ERRCODE='PT409'; END IF;
 -- the checklist is the exact §9 code set, stored sorted (byte order, no duplicates)
 SELECT array_agg(c ORDER BY c COLLATE "C") INTO v_sorted FROM unnest(p_checklist) c;
 IF v_sorted IS DISTINCT FROM ARRAY['account_active','canary_private','descriptor','dispute_notice','managed_off','payout_bank','policy_pages','radar_default','rak_live','three_ds','webhook_live']::text[] THEN
  RAISE EXCEPTION 'Stripe live checklist incomplete' USING ERRCODE='22023'; END IF;
 -- replay: identical row -> same id; anything else -> PT409 (checked before time-dependent rules)
 SELECT x.* INTO ap FROM payments.stripe_live_approvals x WHERE x.id=p_approval;
 IF FOUND THEN
  IF ap.tenant_id=p_tenant AND ap.store_id=p_store AND ap.connection_id=p_connection AND ap.currency=p_currency
   AND ap.approved_by=p_principal AND ap.approval_ref=p_approval_ref AND ap.approved_at=p_approved_at
   AND ap.canary_max_minor=p_canary_max AND ap.max_minor=p_max AND ap.checklist=v_sorted
   AND ap.account_readiness=p_readiness THEN
   RETURN ap.id; END IF;
  RAISE EXCEPTION 'Stripe live approval already exists' USING ERRCODE='PT409'; END IF;
 -- LD8: absence is never a pass. Every key must exist and be non-null, then have the right type.
 FOREACH v_key IN ARRAY ARRAY['AVSRule','CVCRule','ChargesEnabled','CurrentlyDueCount','DescriptorLength',
  'DetailsSubmitted','PayoutsEnabled','PrefixLength'] LOOP
  IF NOT p_readiness ? v_key OR jsonb_typeof(p_readiness->v_key)='null' THEN
   RAISE EXCEPTION 'stripe_live_readiness_unknown' USING ERRCODE='22023'; END IF;
 END LOOP;
 IF (SELECT count(*) FROM jsonb_object_keys(p_readiness))<>8 THEN
  RAISE EXCEPTION 'invalid Stripe live readiness shape' USING ERRCODE='22023'; END IF;
 FOREACH v_key IN ARRAY ARRAY['AVSRule','CVCRule','ChargesEnabled','DetailsSubmitted','PayoutsEnabled'] LOOP
  IF jsonb_typeof(p_readiness->v_key)<>'boolean' THEN
   RAISE EXCEPTION 'invalid Stripe live readiness type' USING ERRCODE='22023'; END IF;
 END LOOP;
 FOREACH v_key IN ARRAY ARRAY['CurrentlyDueCount','DescriptorLength','PrefixLength'] LOOP
  IF jsonb_typeof(p_readiness->v_key)<>'number' OR (p_readiness->>v_key) !~ '^(0|[1-9][0-9]{0,5})$' THEN
   RAISE EXCEPTION 'invalid Stripe live readiness type' USING ERRCODE='22023'; END IF;
 END LOOP;
 IF NOT ((p_readiness->>'ChargesEnabled')::boolean AND (p_readiness->>'PayoutsEnabled')::boolean
  AND (p_readiness->>'DetailsSubmitted')::boolean AND (p_readiness->>'CurrentlyDueCount')::integer=0
  AND (p_readiness->>'DescriptorLength')::integer BETWEEN 5 AND 22
  AND ((p_readiness->>'PrefixLength')::integer=0 OR (p_readiness->>'PrefixLength')::integer BETWEEN 2 AND 10)) THEN
  RAISE EXCEPTION 'stripe_live_not_ready' USING ERRCODE='22023'; END IF;
 v_now:=clock_timestamp();
 IF p_approved_at>v_now OR p_approved_at<v_now-interval '30 days' THEN
  RAISE EXCEPTION 'Stripe live approval time out of range' USING ERRCODE='22023'; END IF;
 -- I05 / LQ3: caps come from the closed table; the owner has ruled a per-order max for TWD only
 v_min:=payments.stripe_min_minor(p_currency);
 IF v_min IS NULL THEN
  RAISE EXCEPTION 'Stripe live currency unknown' USING ERRCODE='22023'; END IF;
 IF p_currency<>'TWD' THEN
  RAISE EXCEPTION 'stripe_live_max_unruled' USING ERRCODE='22023'; END IF;
 IF NOT payments.stripe_amount_ok(p_currency,p_canary_max) OR p_canary_max>2*v_min
  OR NOT payments.stripe_amount_ok(p_currency,p_max) OR p_max<p_canary_max OR p_max>2000000 THEN
  RAISE EXCEPTION 'Stripe live caps invalid' USING ERRCODE='22023'; END IF;
 IF EXISTS(SELECT 1 FROM payments.stripe_live_approvals x WHERE x.connection_id=p_connection
  AND x.revoked_at IS NULL) THEN
  RAISE EXCEPTION 'Stripe live approval already active' USING ERRCODE='PT409'; END IF;
 BEGIN
  INSERT INTO payments.stripe_live_approvals(id,tenant_id,store_id,connection_id,account_id,currency,
   approved_by,approval_ref,approved_at,canary_max_minor,max_minor,checklist,account_readiness)
  VALUES(p_approval,p_tenant,p_store,p_connection,acct.account_id,p_currency,p_principal,p_approval_ref,
   p_approved_at,p_canary_max,p_max,v_sorted,p_readiness);
 EXCEPTION WHEN unique_violation THEN
  RAISE EXCEPTION 'Stripe live approval already active' USING ERRCODE='PT409';
 END;
 INSERT INTO ops.audit_events(tenant_id,store_id,principal_id,action)
 VALUES(p_tenant,p_store,p_principal,'stripe.live.approve');
 RETURN p_approval;
END $$;

CREATE FUNCTION payments.record_stripe_live_canary(p_tenant uuid,p_store uuid,p_principal uuid,
 p_approval uuid,p_attempt uuid,p_refund uuid) RETURNS timestamptz
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE ap payments.stripe_live_approvals%ROWTYPE; v_attempt uuid; v_qualification uuid; v_currency text;
 v_captured bigint; v_refunded bigint; v_verified timestamptz;
BEGIN
 PERFORM integration.require_stripe_registrar_scope(p_tenant,p_store,p_principal);
 IF p_approval IS NULL OR p_attempt IS NULL OR p_refund IS NULL THEN
  RAISE EXCEPTION 'invalid Stripe live canary input' USING ERRCODE='22023'; END IF;
 SELECT x.* INTO ap FROM payments.stripe_live_approvals x WHERE x.id=p_approval AND x.tenant_id=p_tenant
  AND x.store_id=p_store FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'Stripe live approval unavailable' USING ERRCODE='PT409'; END IF;
 IF ap.canary_verified_at IS NOT NULL THEN
  IF ap.canary_attempt_id=p_attempt AND ap.canary_refund_id=p_refund THEN RETURN ap.canary_verified_at; END IF;
  RAISE EXCEPTION 'Stripe live canary already recorded' USING ERRCODE='PT409'; END IF;
 IF ap.revoked_at IS NOT NULL THEN
  RAISE EXCEPTION 'Stripe live approval unavailable' USING ERRCODE='PT409'; END IF;
 -- the attempt is a LIVE Stripe attempt of this connection and currency, made after the approval, under a
 -- qualification issued for this very approval
 -- column-limited read: the registry writer holds SELECT on these attempt columns only (not on the whole row)
 SELECT x.id,x.qualification_id,x.currency INTO v_attempt,v_qualification,v_currency
  FROM checkout.payment_attempts x WHERE x.tenant_id=p_tenant AND x.store_id=p_store
  AND x.id=p_attempt AND x.method_code='stripe_checkout' AND x.environment='LIVE'
  AND x.connection_id=ap.connection_id AND x.currency=ap.currency AND x.created_at>ap.recorded_at;
 IF NOT FOUND OR NOT EXISTS(SELECT 1 FROM payments.account_qualifications q WHERE q.tenant_id=p_tenant
  AND q.store_id=p_store AND q.id=v_qualification AND q.live_approval_id=ap.id) THEN
  RAISE EXCEPTION 'Stripe live canary attempt unavailable' USING ERRCODE='PT409'; END IF;
 -- I05: one CAPTURED fact within the canary cap
 SELECT f.amount_minor INTO v_captured FROM payments.facts f WHERE f.tenant_id=p_tenant AND f.store_id=p_store
  AND f.attempt_id=v_attempt AND f.kind='CAPTURED' AND f.currency=v_currency;
 IF v_captured IS NULL OR v_captured>ap.canary_max_minor THEN
  RAISE EXCEPTION 'Stripe live canary capture unavailable' USING ERRCODE='PT409'; END IF;
 -- the refund is a LIVE refund of that attempt with a SUCCEEDED fact, and SUCCEEDED refunds equal the capture
 IF NOT EXISTS(SELECT 1 FROM payments.stripe_refunds r WHERE r.tenant_id=p_tenant AND r.store_id=p_store
   AND r.id=p_refund AND r.attempt_id=v_attempt AND r.environment='LIVE')
  OR NOT EXISTS(SELECT 1 FROM payments.refund_facts f WHERE f.tenant_id=p_tenant AND f.store_id=p_store
   AND f.refund_id=p_refund AND f.attempt_id=v_attempt AND f.kind='SUCCEEDED') THEN
  RAISE EXCEPTION 'Stripe live canary refund unavailable' USING ERRCODE='PT409'; END IF;
 -- SUCCEEDED counts only while the same refund has no FAILED/CANCELED fact (hosted view rule, RD7)
 SELECT coalesce(sum(f.amount_minor),0) INTO v_refunded FROM payments.refund_facts f
  WHERE f.tenant_id=p_tenant AND f.store_id=p_store AND f.attempt_id=v_attempt AND f.kind='SUCCEEDED'
   AND f.currency=v_currency AND NOT EXISTS(SELECT 1 FROM payments.refund_facts g
    WHERE g.tenant_id=f.tenant_id AND g.store_id=f.store_id AND g.refund_id=f.refund_id
     AND g.kind IN ('FAILED','CANCELED'));
 IF v_refunded<>v_captured THEN
  RAISE EXCEPTION 'Stripe live canary refund is not full' USING ERRCODE='PT409'; END IF;
 IF EXISTS(SELECT 1 FROM payments.review_cases c WHERE c.tenant_id=p_tenant AND c.store_id=p_store
  AND c.attempt_id=v_attempt) THEN
  RAISE EXCEPTION 'Stripe live canary attempt has a review case' USING ERRCODE='PT409'; END IF;
 -- webhook evidence on a LIVE endpoint of this connection: one for the checkout, one for the refund
 IF NOT EXISTS(SELECT 1 FROM payments.stripe_webhook_receipts r JOIN payments.stripe_webhook_endpoints e
   ON e.endpoint_id=r.endpoint_id AND e.connection_id=ap.connection_id AND e.environment='LIVE'
   WHERE r.tenant_id=p_tenant AND r.store_id=p_store AND r.attempt_id=v_attempt AND r.refund_id IS NULL
    AND r.disposition='ACCEPTED')
  OR NOT EXISTS(SELECT 1 FROM payments.stripe_webhook_receipts r JOIN payments.stripe_webhook_endpoints e
   ON e.endpoint_id=r.endpoint_id AND e.connection_id=ap.connection_id AND e.environment='LIVE'
   WHERE r.tenant_id=p_tenant AND r.store_id=p_store AND r.refund_id=p_refund AND r.disposition='ACCEPTED') THEN
  RAISE EXCEPTION 'Stripe live canary webhook evidence missing' USING ERRCODE='PT409'; END IF;
 v_verified:=clock_timestamp();
 UPDATE payments.stripe_live_approvals SET canary_attempt_id=p_attempt,canary_refund_id=p_refund,
  canary_verified_at=v_verified WHERE id=p_approval AND tenant_id=p_tenant AND store_id=p_store;
 INSERT INTO ops.audit_events(tenant_id,store_id,principal_id,action)
 VALUES(p_tenant,p_store,p_principal,'stripe.live.canary');
 RETURN v_verified;
END $$;

CREATE FUNCTION payments.revoke_stripe_live(p_tenant uuid,p_store uuid,p_principal uuid,
 p_approval uuid,p_revoke_ref text) RETURNS timestamptz
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE ap payments.stripe_live_approvals%ROWTYPE; v_at timestamptz;
BEGIN
 -- Any principal passing the registrar scope may revoke: the kill switch must not need the owner.
 PERFORM integration.require_stripe_registrar_scope(p_tenant,p_store,p_principal);
 IF p_approval IS NULL OR p_revoke_ref IS NULL OR p_revoke_ref !~ '^[A-Za-z0-9._:-]{8,128}$' THEN
  RAISE EXCEPTION 'invalid Stripe live revoke input' USING ERRCODE='22023'; END IF;
 -- The revoke below must see qualifications committed while the approval lock was awaited: each statement in
 -- READ COMMITTED takes a fresh snapshot, so this function refuses a snapshot-pinning isolation level.
 IF current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'Stripe live revoke needs read committed' USING ERRCODE='PT409'; END IF;
 SELECT x.* INTO ap FROM payments.stripe_live_approvals x WHERE x.id=p_approval AND x.tenant_id=p_tenant
  AND x.store_id=p_store FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'Stripe live approval unavailable' USING ERRCODE='PT409'; END IF;
 IF ap.revoked_at IS NOT NULL THEN
  IF ap.revoked_by=p_principal AND ap.revoke_ref=p_revoke_ref THEN RETURN ap.revoked_at; END IF;
  RAISE EXCEPTION 'Stripe live approval already revoked' USING ERRCODE='PT409'; END IF;
 -- separate statement: every non-revoked qualification of this approval (the trigger stamps revoked_at)
 UPDATE payments.account_qualifications SET revoked_at=clock_timestamp()
  WHERE tenant_id=p_tenant AND store_id=p_store AND live_approval_id=p_approval AND revoked_at IS NULL;
 v_at:=clock_timestamp();
 UPDATE payments.stripe_live_approvals SET revoked_at=v_at,revoked_by=p_principal,revoke_ref=p_revoke_ref
  WHERE id=p_approval AND tenant_id=p_tenant AND store_id=p_store;
 INSERT INTO ops.audit_events(tenant_id,store_id,principal_id,action)
 VALUES(p_tenant,p_store,p_principal,'stripe.live.revoke');
 RETURN v_at;
END $$;

ALTER FUNCTION payments.approve_stripe_live(uuid,uuid,uuid,uuid,uuid,text,text,timestamptz,bigint,bigint,text[],jsonb)
 OWNER TO commerce_payment_registry_writer;
ALTER FUNCTION payments.record_stripe_live_canary(uuid,uuid,uuid,uuid,uuid,uuid)
 OWNER TO commerce_payment_registry_writer;
ALTER FUNCTION payments.revoke_stripe_live(uuid,uuid,uuid,uuid,text) OWNER TO commerce_payment_registry_writer;
REVOKE ALL ON FUNCTION payments.approve_stripe_live(uuid,uuid,uuid,uuid,uuid,text,text,timestamptz,bigint,bigint,text[],jsonb) FROM PUBLIC;
REVOKE ALL ON FUNCTION payments.record_stripe_live_canary(uuid,uuid,uuid,uuid,uuid,uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION payments.revoke_stripe_live(uuid,uuid,uuid,uuid,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION payments.approve_stripe_live(uuid,uuid,uuid,uuid,uuid,text,text,timestamptz,bigint,bigint,text[],jsonb)
 TO commerce_payment_registrar;
GRANT EXECUTE ON FUNCTION payments.record_stripe_live_canary(uuid,uuid,uuid,uuid,uuid,uuid) TO commerce_payment_registrar;
GRANT EXECUTE ON FUNCTION payments.revoke_stripe_live(uuid,uuid,uuid,uuid,text) TO commerce_payment_registrar;


-- ---------------------------------------------------------------------------------------------------------
-- Re-created 0061/0062 functions. Same signature, owner, ACL and search_path; deltas are the marked lines.
-- ---------------------------------------------------------------------------------------------------------

-- delta: environment IN ('SANDBOX','LIVE') (LIVE rows are never touched by SANDBOX callers: the environment is an input
-- only here, and every later function reads it from the registered account).
CREATE OR REPLACE FUNCTION integration.register_stripe_account(p_tenant uuid,p_store uuid,p_principal uuid,
 p_connection uuid,p_binding uuid,p_environment text,p_account text,p_key_id text,
 p_nonce bytea,p_ciphertext bytea) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 PERFORM integration.require_stripe_registrar_scope(p_tenant,p_store,p_principal);
 IF p_connection IS NULL OR p_binding IS NULL OR p_environment NOT IN ('SANDBOX','LIVE')
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

-- delta: environment IN ('SANDBOX','LIVE'); rotation keeps the approval, new starts need a fresh qualification.
CREATE OR REPLACE FUNCTION integration.rotate_stripe_key(p_tenant uuid,p_store uuid,p_principal uuid,
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
 IF NOT FOUND OR acct.credential_version<>p_expected_version OR acct.environment NOT IN ('SANDBOX','LIVE') THEN
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

-- delta: profile LIVE only for a LIVE account; PROVIDER_MOCK/SANDBOX only for a SANDBOX account (no approval needed: the
-- endpoint is registered before live-approve, contract §2 step 2).
CREATE OR REPLACE FUNCTION payments.set_stripe_webhook_endpoint(p_tenant uuid,p_store uuid,p_principal uuid,
 p_connection uuid,p_endpoint uuid,p_profile text,p_expected_version bigint,p_enabled boolean,
 p_key_id text,p_nonce bytea,p_ciphertext bytea) RETURNS bigint
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE acct integration.merchant_accounts%ROWTYPE; ep payments.stripe_webhook_endpoints%ROWTYPE;
 v_next bigint;
BEGIN
 PERFORM integration.require_stripe_registrar_scope(p_tenant,p_store,p_principal);
 IF p_connection IS NULL OR p_endpoint IS NULL OR p_profile NOT IN ('PROVIDER_MOCK','SANDBOX','LIVE')
  OR p_expected_version IS NULL OR p_expected_version<0 OR p_enabled IS NULL
  OR p_key_id !~ '^[A-Za-z0-9_-]{1,40}$'
  OR octet_length(p_nonce)<>12 OR octet_length(p_ciphertext) NOT BETWEEN 17 AND 8192 THEN
  RAISE EXCEPTION 'invalid Stripe endpoint input' USING ERRCODE='22023'; END IF;
 SELECT x.* INTO acct FROM integration.merchant_accounts x WHERE x.tenant_id=p_tenant
  AND x.store_id=p_store AND x.id=p_connection AND x.provider='stripe' FOR SHARE;
 IF NOT FOUND OR acct.environment NOT IN ('SANDBOX','LIVE') OR (acct.environment='LIVE')<>(p_profile='LIVE') THEN
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

-- delta: environment IN ('SANDBOX','LIVE').
CREATE OR REPLACE FUNCTION payments.stripe_endpoint_account(p_tenant uuid,p_store uuid,p_principal uuid,
 p_connection uuid) RETURNS text
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_account text;
BEGIN
 PERFORM integration.require_stripe_registrar_scope(p_tenant,p_store,p_principal);
 SELECT x.account_id INTO v_account FROM integration.merchant_accounts x WHERE x.tenant_id=p_tenant
  AND x.store_id=p_store AND x.id=p_connection AND x.provider='stripe' AND x.environment IN ('SANDBOX','LIVE');
 IF v_account IS NULL THEN
  RAISE EXCEPTION 'Stripe endpoint account unavailable' USING ERRCODE='PT409'; END IF;
 RETURN v_account;
END $$;

-- delta: environment IN ('SANDBOX','LIVE').
CREATE OR REPLACE FUNCTION payments.stripe_registrar_credential(p_tenant uuid,p_store uuid,p_principal uuid,
 p_connection uuid,p_expected_version bigint)
RETURNS TABLE(account_id text,key_id text,nonce bytea,ciphertext bytea)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE acct integration.merchant_accounts%ROWTYPE; c integration.account_credentials%ROWTYPE;
BEGIN
 PERFORM integration.require_stripe_registrar_scope(p_tenant,p_store,p_principal);
 IF p_connection IS NULL OR p_expected_version IS NULL OR p_expected_version<1 THEN
  RAISE EXCEPTION 'invalid Stripe credential read' USING ERRCODE='22023'; END IF;
 SELECT x.* INTO acct FROM integration.merchant_accounts x WHERE x.tenant_id=p_tenant
  AND x.store_id=p_store AND x.id=p_connection AND x.provider='stripe' AND x.environment IN ('SANDBOX','LIVE');
 IF NOT FOUND OR acct.credential_version<>p_expected_version THEN
  RAISE EXCEPTION 'Stripe credential version changed' USING ERRCODE='PT409'; END IF;
 SELECT x.* INTO c FROM integration.account_credentials x WHERE x.tenant_id=p_tenant
  AND x.store_id=p_store AND x.connection_id=p_connection AND x.version=p_expected_version;
 IF NOT FOUND THEN RAISE EXCEPTION 'Stripe credential unavailable' USING ERRCODE='PT409'; END IF;
 RETURN QUERY SELECT acct.account_id,c.key_id,c.nonce,c.ciphertext;
END $$;

-- delta: gains no parameter. Profile LIVE requires a LIVE account, an active approval taken FOR SHARE (revoke's FOR UPDATE
-- waits for us, so a qualification inserted concurrently is either revoked or refused) and cs_live_ probe evidence; the
-- inserted row carries acct.environment, proof REAL_LIVE and live_approval_id. Lock order: account, binding, approval.
CREATE OR REPLACE FUNCTION payments.qualify_stripe_method(p_tenant uuid,p_store uuid,p_principal uuid,
 p_qualification uuid,p_connection uuid,p_expected_version bigint,p_profile text,p_evidence_ref text,
 p_observed_at timestamptz,p_expires_at timestamptz) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE acct integration.merchant_accounts%ROWTYPE; b integration.bindings%ROWTYPE;
 ap payments.stripe_live_approvals%ROWTYPE; v_proof text; v_approval uuid;
BEGIN
 PERFORM integration.require_stripe_registrar_scope(p_tenant,p_store,p_principal);
 IF p_qualification IS NULL OR p_connection IS NULL OR p_expected_version IS NULL
  OR p_expected_version<1 OR p_profile NOT IN ('PROVIDER_MOCK','SANDBOX','LIVE')
  OR p_evidence_ref IS NULL OR length(p_evidence_ref) NOT BETWEEN 1 AND 200
  OR p_evidence_ref ~ '[[:cntrl:]]' OR p_observed_at IS NULL OR p_expires_at IS NULL
  OR p_observed_at>clock_timestamp() OR p_expires_at<=p_observed_at
  OR p_expires_at>p_observed_at+interval '30 days' THEN
  RAISE EXCEPTION 'invalid Stripe qualification input' USING ERRCODE='22023'; END IF;
 IF p_profile='SANDBOX' AND p_evidence_ref !~ '^stripe-probe:[A-Za-z0-9_]{1,255}$' THEN
  RAISE EXCEPTION 'Stripe probe evidence missing' USING ERRCODE='22023'; END IF;
 IF p_profile='LIVE' AND p_evidence_ref !~ '^stripe-probe:cs_live_[A-Za-z0-9_]{1,245}$' THEN
  RAISE EXCEPTION 'Stripe probe evidence missing' USING ERRCODE='22023'; END IF;
 SELECT x.* INTO acct FROM integration.merchant_accounts x WHERE x.tenant_id=p_tenant
  AND x.store_id=p_store AND x.id=p_connection AND x.provider='stripe' FOR SHARE;
 -- (acct.environment='LIVE')=(p_profile='LIVE'): PROVIDER_MOCK and SANDBOX profiles only on SANDBOX accounts
 IF NOT FOUND OR acct.environment NOT IN ('SANDBOX','LIVE')
  OR (acct.environment='LIVE')<>(p_profile='LIVE') OR acct.credential_version<>p_expected_version THEN
  RAISE EXCEPTION 'Stripe account unavailable' USING ERRCODE='PT409'; END IF;
 SELECT x.* INTO b FROM integration.bindings x WHERE x.tenant_id=p_tenant AND x.store_id=p_store
  AND x.id=acct.binding_id FOR SHARE;
 IF NOT FOUND OR NOT b.enabled OR b.provider<>'stripe'
  OR b.external_asset_id<>acct.binding_asset THEN
  RAISE EXCEPTION 'Stripe binding unavailable' USING ERRCODE='PT409'; END IF;
 IF p_profile='LIVE' THEN
  SELECT x.* INTO ap FROM payments.stripe_live_approvals x WHERE x.tenant_id=p_tenant AND x.store_id=p_store
   AND x.connection_id=p_connection AND x.revoked_at IS NULL FOR SHARE;
  IF NOT FOUND THEN RAISE EXCEPTION 'Stripe live approval unavailable' USING ERRCODE='PT409'; END IF;
  v_approval:=ap.id;
 END IF;
 v_proof:=CASE p_profile WHEN 'PROVIDER_MOCK' THEN 'PROVIDER_MOCK' WHEN 'SANDBOX' THEN 'REAL_SANDBOX'
  ELSE 'REAL_LIVE' END;
 INSERT INTO payments.account_qualifications(id,tenant_id,store_id,connection_id,credential_version,
  environment,code,proof_class,evidence_ref,observed_at,expires_at,live_approval_id)
 VALUES(p_qualification,p_tenant,p_store,p_connection,acct.credential_version,acct.environment,
  'stripe_checkout',v_proof,p_evidence_ref,p_observed_at,p_expires_at,v_approval);
 INSERT INTO ops.audit_events(tenant_id,store_id,principal_id,action)
 VALUES(p_tenant,p_store,p_principal,'stripe.method_qualified');
 RETURN p_qualification;
END $$;

-- delta: p_enabled=false skips qualification validity, approval and cap checks (kill switch works after revoke, expiry
-- and rotate) but still needs scope, market, account/binding, the head CAS and a qualification of this connection;
-- REAL_LIVE is no longer rejected; q.environment must equal acct.environment; a LIVE account with p_enabled=true needs
-- REAL_LIVE, an active approval (FOR SHARE, same one the qualification names), the approval currency and the cap
-- (canary cap until canary_verified_at, then max_minor). Lock order: account, binding, approval, qualification, head.
CREATE OR REPLACE FUNCTION payments.set_stripe_method(p_tenant uuid,p_store uuid,p_principal uuid,
 p_market uuid,p_country text,p_connection uuid,p_qualification uuid,p_expected_version bigint,
 p_enabled boolean,p_visible boolean,p_sort integer,p_min bigint,p_max bigint,
 p_name_hans text,p_name_hant text,p_name_en text) RETURNS bigint
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE market pricing.markets%ROWTYPE; acct integration.merchant_accounts%ROWTYPE;
 b integration.bindings%ROWTYPE; q payments.account_qualifications%ROWTYPE;
 ap payments.stripe_live_approvals%ROWTYPE;
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
 IF NOT FOUND OR acct.environment NOT IN ('SANDBOX','LIVE') THEN
  RAISE EXCEPTION 'Stripe account unavailable' USING ERRCODE='PT409'; END IF;
 SELECT x.* INTO b FROM integration.bindings x WHERE x.tenant_id=p_tenant
  AND x.store_id=p_store AND x.id=acct.binding_id FOR SHARE;
 IF NOT FOUND OR NOT b.enabled OR b.provider<>'stripe'
  OR b.external_asset_id<>acct.binding_asset THEN
  RAISE EXCEPTION 'Stripe binding unavailable' USING ERRCODE='PT409'; END IF;
 -- LD5: only enabling a LIVE method needs the active approval; the approval is locked before the qualification
 IF acct.environment='LIVE' AND p_enabled THEN
  SELECT x.* INTO ap FROM payments.stripe_live_approvals x WHERE x.tenant_id=p_tenant AND x.store_id=p_store
   AND x.connection_id=p_connection AND x.revoked_at IS NULL FOR SHARE;
  IF NOT FOUND OR market.currency<>ap.currency THEN
   RAISE EXCEPTION 'Stripe live approval unavailable' USING ERRCODE='PT409'; END IF;
 END IF;
 SELECT x.* INTO q FROM payments.account_qualifications x WHERE x.id=p_qualification
  AND x.tenant_id=p_tenant AND x.store_id=p_store FOR SHARE;
 v_now:=clock_timestamp();
 IF NOT FOUND OR q.connection_id<>p_connection OR q.code<>'stripe_checkout' THEN
  RAISE EXCEPTION 'Stripe qualification unavailable' USING ERRCODE='PT409'; END IF;
 IF p_enabled THEN
  IF q.credential_version<>acct.credential_version OR q.environment<>acct.environment
   OR q.revoked_at IS NOT NULL OR q.observed_at>v_now OR q.expires_at<=v_now THEN
   RAISE EXCEPTION 'Stripe qualification unavailable' USING ERRCODE='PT409'; END IF;
  IF acct.environment='LIVE' THEN
   IF q.proof_class<>'REAL_LIVE' OR q.live_approval_id IS DISTINCT FROM ap.id
    OR p_max>(CASE WHEN ap.canary_verified_at IS NULL THEN ap.canary_max_minor ELSE ap.max_minor END) THEN
    RAISE EXCEPTION 'Stripe live method not admitted' USING ERRCODE='PT409'; END IF;
  END IF;
 END IF;
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
 VALUES(p_tenant,p_store,p_market,p_country,'stripe_checkout',v_next,'stripe',acct.environment,
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


-- delta: Livemode must equal to_jsonb(a.environment='LIVE') (was IS DISTINCT FROM 'false').
CREATE OR REPLACE FUNCTION payments.apply_stripe_refund(p_attempt uuid,p_report_hash bytea)
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
  OR r->'Livemode' IS DISTINCT FROM to_jsonb(a.environment='LIVE')) THEN
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

-- delta: the no-attempt Stripe method branch admits LIVE. a.environment is NULL there (no attempt yet), so the environment
-- comes from the profile: LIVE -> LIVE, else SANDBOX; proof_class follows the profile (REAL_LIVE for LIVE).
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
 IF a.id IS NULL AND p_stripe_digest IS NOT NULL AND p_profile IN ('PROVIDER_MOCK','SANDBOX','LIVE') THEN
  SELECT jsonb_build_object('code',m.code,'version',m.version,'name_hans',m.name_hans,
   'name_hant',m.name_hant,'name_en',m.name_en) INTO v_stripe_method
  FROM payments.method_heads h
  JOIN payments.method_versions m ON m.tenant_id=h.tenant_id AND m.store_id=h.store_id
   AND m.market_id=h.market_id AND m.country=h.country AND m.code=h.code AND m.version=h.current_version
  JOIN integration.merchant_accounts acct ON acct.tenant_id=m.tenant_id AND acct.store_id=m.store_id
   AND acct.id=m.connection_id AND acct.provider='stripe'
   AND acct.environment=CASE WHEN p_profile='LIVE' THEN 'LIVE' ELSE 'SANDBOX' END
  JOIN integration.bindings b ON b.tenant_id=acct.tenant_id AND b.store_id=acct.store_id
   AND b.id=acct.binding_id AND b.provider='stripe' AND b.external_asset_id=acct.binding_asset
   AND b.semantic_version=m.binding_version AND b.enabled
  JOIN payments.account_qualifications q ON q.tenant_id=m.tenant_id AND q.store_id=m.store_id
   AND q.id=m.qualification_id AND q.connection_id=acct.id AND q.credential_version=acct.credential_version
   AND q.environment=acct.environment AND q.code='stripe_checkout'
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
   AND q.proof_class=CASE p_profile WHEN 'PROVIDER_MOCK' THEN 'PROVIDER_MOCK' WHEN 'SANDBOX' THEN 'REAL_SANDBOX'
    ELSE 'REAL_LIVE' END;
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

-- ---------------------------------------------------------------------------------------------------------
-- S5 (brief ruling B5): the merchant refund environment guard needs the attempt's environment, which commerce_runtime
-- cannot read. Same fresh-final-auth prelude as identity.read_merchant_refunds, permission payments:refund; returns
-- the environment of the order's attempt (same order and attempt selection as payments.request_stripe_refund, without
-- its locks) or NULL when the order has no attempt; a missing order is PT404 exactly as request_stripe_refund answers
-- it (keeps the existing 404 for another store's order id). No write, no buyer access, no other column.
-- ---------------------------------------------------------------------------------------------------------
CREATE FUNCTION identity.merchant_refund_environment(p_token_hash bytea,p_store uuid,p_order uuid)
RETURNS text LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE s record; v_env text; v_auth_error text; v_found boolean;
BEGIN
 IF p_token_hash IS NULL OR octet_length(p_token_hash)<>32 OR p_store IS NULL OR p_order IS NULL
  OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid refund environment read' USING ERRCODE='PT400'; END IF;
 SELECT * INTO s FROM identity.resolve_access(p_token_hash,p_store,'payments:refund');
 IF s.access_status='unauthorized' THEN RAISE EXCEPTION 'unauthorized' USING ERRCODE='PT401'; END IF;
 IF s.access_status='not_found' THEN RAISE EXCEPTION 'not found' USING ERRCODE='PT404'; END IF;
 IF s.access_status IS DISTINCT FROM 'ok' THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 IF current_setting('app.tenant_id',true) IS DISTINCT FROM s.tenant_id::text
  OR current_setting('app.store_id',true) IS DISTINCT FROM p_store::text
  OR current_setting('app.principal_id',true) IS DISTINCT FROM s.principal_id::text THEN
  RAISE EXCEPTION 'forbidden' USING ERRCODE='PT403'; END IF;
 SELECT a.environment INTO v_env FROM checkout.orders o LEFT JOIN checkout.payment_attempts a
  ON a.tenant_id=o.tenant_id AND a.store_id=o.store_id AND a.owner_id=o.owner_id AND a.order_id=o.id
  WHERE o.tenant_id=s.tenant_id AND o.store_id=p_store AND o.id=p_order;
 v_found:=FOUND; -- an order without an attempt is v_env NULL; a missing order is PT404 below (as the request definer)
 -- Fresh VOLATILE fence after the data read, before the answer leaves (0027/0062 pattern).
 SELECT CASE WHEN p.id IS NULL THEN 'PT401'
   WHEN st.id IS NULL OR t.id IS NULL OR m.principal_id IS NULL OR gr.permission IS NULL THEN 'PT404'
   WHEN og.permission IS NULL OR st.tenant_id<>s.tenant_id OR p.id<>s.principal_id
     OR m.authz_revision<>s.authz_revision THEN 'PT403' ELSE NULL END INTO v_auth_error
 FROM (VALUES(1)) gate(n)
 LEFT JOIN identity.sessions login ON login.token_hash=p_token_hash AND login.audience='merchant'
   AND login.revoked_at IS NULL AND login.expires_at>clock_timestamp()
 LEFT JOIN identity.principals p ON p.id=login.principal_id AND p.active
 LEFT JOIN control.stores st ON st.id=p_store AND st.active
 LEFT JOIN control.tenants t ON t.id=st.tenant_id AND t.active
 LEFT JOIN identity.memberships m ON m.tenant_id=st.tenant_id AND m.principal_id=p.id AND m.active
 LEFT JOIN identity.store_grants gr ON gr.tenant_id=st.tenant_id AND gr.store_id=st.id
   AND gr.principal_id=p.id AND gr.permission='store:read'
 LEFT JOIN identity.store_grants og ON og.tenant_id=st.tenant_id AND og.store_id=st.id
   AND og.principal_id=p.id AND og.permission='payments:refund';
 IF v_auth_error IS NOT NULL THEN RAISE EXCEPTION 'refund environment access denied' USING ERRCODE=v_auth_error; END IF;
 IF NOT v_found THEN RAISE EXCEPTION 'order not found' USING ERRCODE='PT404'; END IF;
 RETURN v_env;
END $$;
ALTER FUNCTION identity.merchant_refund_environment(bytea,uuid,uuid) OWNER TO commerce_auth;
REVOKE ALL ON FUNCTION identity.merchant_refund_environment(bytea,uuid,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION identity.merchant_refund_environment(bytea,uuid,uuid) TO commerce_runtime;


-- ---------------------------------------------------------------------------------------------------------
-- COMMENT ON every new or changed object: owning package, allowed roles, non-goals (PROCESS §5).
-- ---------------------------------------------------------------------------------------------------------
COMMENT ON TABLE payments.stripe_live_approvals IS 'payments owner (payments/stripeadmin); registry writer inserts one operator-attested owner approval per LIVE store connection and sets the canary and revoke triples once; no runtime, worker, ingress or merchant access; no money movement; NOT proof of owner consent (approval_ref points at the owner message, LD2)';
DO $$ DECLARE v_col record; BEGIN
 FOR v_col IN SELECT column_name FROM information_schema.columns
  WHERE table_schema='payments' AND table_name='stripe_live_approvals' LOOP
  EXECUTE format('COMMENT ON COLUMN payments.stripe_live_approvals.%I IS %L',v_col.column_name,
   'payments owner; registry-writer-only approval fact; immutable except the canary and revoke triples (set once); never read by runtime admission, which reads the qualification');
 END LOOP;
END $$;
COMMENT ON COLUMN payments.account_qualifications.live_approval_id IS 'payments owner; set only on REAL_LIVE Stripe qualifications by payments.qualify_stripe_method; names the active approval of the same connection (composite FK); never a client value';
COMMENT ON CONSTRAINT stripe_qualification_live_needs_approval_check ON payments.account_qualifications IS 'payments owner; a REAL_LIVE Stripe qualification must carry live_approval_id (replaces stripe_qualification_no_live_check); SP19 REAL_LIVE-without-approval stays refused';
COMMENT ON CONSTRAINT stripe_qualification_approval_shape_check ON payments.account_qualifications IS 'payments owner; live_approval_id exists only on REAL_LIVE Stripe qualifications';
COMMENT ON CONSTRAINT stripe_qualification_live_approval_fk ON payments.account_qualifications IS 'payments owner; a LIVE qualification references an approval of the same store and connection (composite FK); revocation is the trigger-guarded revoked_at';
COMMENT ON INDEX payments.stripe_live_one_active IS 'payments owner; at most one non-revoked LIVE approval per Stripe connection (§3.2)';
COMMENT ON TRIGGER guard_stripe_live_approval ON payments.stripe_live_approvals IS 'payments owner; every column immutable except the canary triple and the revoke triple, each NULL to value once';
COMMENT ON FUNCTION payments.guard_stripe_live_approval() IS 'payments owner; registry writer definer enforcing approval immutability; no network or key authority';
COMMENT ON TRIGGER account_qualification_revoke_only ON payments.account_qualifications IS 'payments owner; the only permitted UPDATE for any role is revoked_at NULL to now on a LIVE-approved Stripe qualification (clock_timestamp stamped by the trigger)';
COMMENT ON FUNCTION payments.account_qualification_revoke_only() IS 'payments owner; registry writer definer; rejects every qualification UPDATE except the LIVE revoke (PT409); id row-lock no-ops pass; no network authority';
COMMENT ON POLICY stripe_live_approval_read ON payments.stripe_live_approvals IS 'payments owner; registry writer reads approvals of the pinned tenant/store only';
COMMENT ON POLICY stripe_live_approval_insert ON payments.stripe_live_approvals IS 'payments owner; registry writer inserts an approval only under the pinned tenant/store and approving principal';
COMMENT ON POLICY stripe_live_approval_update ON payments.stripe_live_approvals IS 'payments owner; registry writer locks or sets canary/revoke columns of the pinned tenant/store only (column grants and the guard trigger bound the change)';
COMMENT ON POLICY stripe_registry_qualification_revoke ON payments.account_qualifications IS 'payments owner; registry writer may revoke LIVE-approved Stripe qualifications of the pinned scope; the revoke-only trigger forbids every other change';
COMMENT ON POLICY stripe_registry_attempt_read ON checkout.payment_attempts IS 'payments owner; registry writer reads scoped attempt identity columns for the LIVE canary check only';
COMMENT ON POLICY stripe_registry_fact_read ON payments.facts IS 'payments owner; registry writer reads scoped fact kind/amount for the LIVE canary check only';
COMMENT ON POLICY stripe_registry_refund_read ON payments.stripe_refunds IS 'payments owner; registry writer reads scoped refund identity/environment for the LIVE canary check only';
COMMENT ON POLICY stripe_registry_refund_fact_read ON payments.refund_facts IS 'payments owner; registry writer reads scoped refund fact kind/amount for the LIVE canary check only';
COMMENT ON POLICY stripe_registry_review_read ON payments.review_cases IS 'payments owner; registry writer checks the scoped attempt has no review case for the LIVE canary only';
COMMENT ON POLICY stripe_registry_receipt_read ON payments.stripe_webhook_receipts IS 'payments owner; registry writer checks scoped accepted webhook receipts for the LIVE canary only; no body, signature or event payload columns';
COMMENT ON FUNCTION payments.approve_stripe_live(uuid,uuid,uuid,uuid,uuid,text,text,timestamptz,bigint,bigint,text[],jsonb) IS 'payments owner (payments/stripeadmin); operator registrar records the operator-attested owner approval of one LIVE store (grant predicate, Stripe readiness, caps, checklist); no key access, no money movement; replay identical or PT409';
COMMENT ON FUNCTION payments.record_stripe_live_canary(uuid,uuid,uuid,uuid,uuid,uuid) IS 'payments owner (payments/stripeadmin); operator registrar verifies in SQL that the owner canary was a captured LIVE payment, fully refunded, with webhook receipts, and sets the canary triple once; no money movement';
COMMENT ON FUNCTION payments.revoke_stripe_live(uuid,uuid,uuid,uuid,text) IS 'payments owner (payments/stripeadmin); operator registrar kill switch: revokes the approval and its LIVE qualifications (start then PT409); no owner needed; in-flight reconcile and refunds unaffected';
COMMENT ON FUNCTION identity.merchant_refund_environment(bytea,uuid,uuid) IS 'identity owner (commerce_auth); merchant runtime reads the environment of one order attempt under payments:refund with fresh final authorization so the refund route can refuse another environment; no write, no buyer access';
COMMENT ON FUNCTION integration.register_stripe_account(uuid,uuid,uuid,uuid,uuid,text,text,text,bytea,bytea) IS 'integration owner; operator registrar atomically binds one SANDBOX or LIVE Stripe account and encrypted API credential to one store (LIVE needs rk_live_, enforced by the Go registrar); no plaintext or pooled funds';
COMMENT ON FUNCTION integration.rotate_stripe_key(uuid,uuid,uuid,uuid,bigint,text,bytea,bytea) IS 'integration owner; operator registrar appends exact next encrypted Stripe API key version and advances head (SANDBOX or LIVE); the LIVE approval is kept, new starts need a re-qualification; historical attempts remain frozen';
COMMENT ON FUNCTION payments.set_stripe_webhook_endpoint(uuid,uuid,uuid,uuid,uuid,text,bigint,boolean,text,bytea,bytea) IS 'payments owner; operator registrar creates or rotates one endpoint signing envelope by CAS; profile LIVE only for a LIVE account and vice versa; no API key, rebind or approval needed';
COMMENT ON FUNCTION payments.stripe_endpoint_account(uuid,uuid,uuid,uuid) IS 'payments owner; operator registrar reads the registered SANDBOX or LIVE account id of one in-scope Stripe connection to seal webhook AAD; no key material, no writes';
COMMENT ON FUNCTION payments.stripe_registrar_credential(uuid,uuid,uuid,uuid,bigint) IS 'payments owner; operator registrar reads the sealed API credential envelope at the connection head version plus its registered account so qualify and live-approve use the stored key; ciphertext only, no writes, no worker or runtime access';
COMMENT ON FUNCTION payments.qualify_stripe_method(uuid,uuid,uuid,uuid,uuid,bigint,text,text,timestamptz,timestamptz) IS 'payments owner; operator registrar records bounded SANDBOX, mock or LIVE probe evidence for the expected Stripe credential version; a LIVE qualification needs an active approval (locked FOR SHARE) and cs_live_ probe evidence';
COMMENT ON FUNCTION payments.set_stripe_method(uuid,uuid,uuid,uuid,text,uuid,uuid,bigint,boolean,boolean,integer,bigint,bigint,text,text,text) IS 'payments owner; operator registrar appends a Stripe method revision with exact scoped account/qualification and amount bounds; enabling on LIVE needs an active approval, REAL_LIVE qualification and the canary or per-order cap; disabling always works (kill switch); merchant SetMethod remains PAYUNi only';
COMMENT ON FUNCTION payments.apply_stripe_refund(uuid,bytea) IS 'payments owner; private Stripe refund observation to append-only refund facts and reviews; Livemode must equal the attempt environment; never stock, order, fulfilment or work-item state (RD6); no network or webhook authority';
COMMENT ON FUNCTION checkout.hosted_payment_view_v2(bytea,uuid,uuid,text,bytea,bytea) IS 'checkout owner; hosted runtime projects PAYUNi and Stripe (SANDBOX or LIVE profile) methods, frozen payment facts and buyer-safe refund totals; never authorizes capture, refund or stock release';
