-- 0080 Meta CAPI Purchase, public product feed, consent hook and billing/consent grants (T16;
-- contracts/meta-ads-v1.md §4.3 + §4.4 0080 rows + §6.4, FROZEN 2026-09-30; unit ads-capi,
-- docs/delivery/units/ads-capi.md C1-C7).
--
-- Owns: ads.capi_contexts (browser user agent per consenting buyer), ads.capi_events (one CAPI operation per captured
-- attempt, ever), the CAPI planner/Check/user-data/feed/consent-hook definers below, and the grants that let the ads
-- definers of 0074 read billing standing (billing.store_standing) and consent (customers.consent_allows).
--
-- Non-goals: no Graph call or River job from SQL (the Go sweeper inserts the job first, ads.verify_job re-checks it);
-- no copy of consent or billing state; no phone, email or name stored anywhere; no user data outside
-- ads.capi_user_data; no resend (an UNKNOWN CAPI operation is never re-POSTed, F15); no refund or negative events.
--
-- Depends on: 0074/0075 (ads schema, commerce_ads_writer, ads.verify_job, ads.store_settings), 0078 (customers.
-- consent_allows), 0079 (billing.store_standing), 0020 (buyer.resolve_published_store), 0006 (buyer.resolve_scope),
-- 0018/0061 (payments.facts), 0016 (checkout.payment_attempts), 0013 (checkout.orders), 0007 (storefront.quotes),
-- 0002 (catalog, inventory).
--
-- Callers (each function's COMMENT repeats its only caller):
--   ads-worker sweeper (commerce_worker, internal/attribution): ads.plan_capi_candidates/plan_capi_eligible/plan_capi/
--     plan_capi_purge
--   dispatcher pool (commerce_worker, internal/attribution/capiroute): ads.check_capi, ads.capi_user_data
--   buyer runtime (commerce_buyer_runtime): ads.put_capi_context (consent PUT hook, A-3), ads.feed_rows (public feed)
--
-- Gaps in contract §4.4 found while writing this file (EXTRA rows the integrator must rule and add to §4.4 and MA02;
-- listed in output/ads-capi/integrator-hooks.patch header): R-H1 catalog.skus/inventory.balances/storefront.quotes/
-- checkout.* column reads (the schema has no order-line or variant table: lines come from the frozen quote snapshot,
-- ids are catalog.skus ids, availability from inventory.balances); R-H2 control.stores(name) for the feed brand;
-- R-H3 buyer.resolve_scope EXECUTE (put_capi_context, implied by §4.3); the phone column is deliberately NOT granted
-- (CD5: a recipient phone may be sent only when checkout records recipient = buyer, which nothing records yet).

-- ---------------------------------------------------------------------------------------
-- Schema USAGE and function EXECUTE (§4.4 rows "customers/billing" and "buyer"). Both late-bound names must be
-- SECURITY DEFINER: an invoker would see zero rows under FORCE RLS and read UNBILLED / not-consented wrongly.
-- ---------------------------------------------------------------------------------------
DO $$
DECLARE r record; v_n integer:=0;
BEGIN
 FOR r IN SELECT p.oid::regprocedure::text AS sig,p.prosecdef FROM pg_proc p WHERE p.oid IN (
   'customers.consent_allows(uuid,uuid,uuid,text,text)'::regprocedure,'billing.store_standing(uuid,uuid)'::regprocedure) LOOP
  v_n:=v_n+1;
  IF NOT r.prosecdef THEN RAISE EXCEPTION '% must be SECURITY DEFINER (fail-open otherwise)',r.sig; END IF;
 END LOOP;
 IF v_n<>2 THEN RAISE EXCEPTION '0080 needs customers.consent_allows (0078) and billing.store_standing (0079)'; END IF;
END $$;
GRANT USAGE ON SCHEMA customers, billing, buyer, checkout, storefront, inventory TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION customers.consent_allows(uuid,uuid,uuid,text,text) TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION billing.store_standing(uuid,uuid) TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION buyer.resolve_published_store(text) TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION buyer.resolve_scope(bytea,uuid) TO commerce_ads_writer;

-- ---------------------------------------------------------------------------------------
-- Tables (§4.3). FORCE RLS, PUBLIC revoked, no runtime grants.
-- ---------------------------------------------------------------------------------------
CREATE TABLE ads.capi_contexts (                   -- UA for website events (F14); purged after 8 days
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, owner_id uuid NOT NULL,
 user_agent text NOT NULL CHECK(char_length(user_agent) BETWEEN 1 AND 512),
 captured_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,store_id,owner_id),
 FOREIGN KEY(tenant_id,store_id,owner_id) REFERENCES buyer.owners(tenant_id,store_id,id));
CREATE TABLE ads.capi_events (                     -- one per attempt, ever
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, attempt_id uuid NOT NULL,
 operation_id uuid NOT NULL UNIQUE,
 event_id text NOT NULL UNIQUE CHECK(event_id ~ '^lc-purchase-[0-9a-f-]{36}$'),
 planned_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 fact_kind text GENERATED ALWAYS AS ('CAPTURED') STORED,
 PRIMARY KEY(tenant_id,store_id,attempt_id),
 FOREIGN KEY(tenant_id,store_id,attempt_id,fact_kind) REFERENCES payments.facts(tenant_id,store_id,attempt_id,kind),
 FOREIGN KEY(tenant_id,store_id,operation_id) REFERENCES integration.operations(tenant_id,store_id,id));
ALTER TABLE ads.capi_contexts ENABLE ROW LEVEL SECURITY;  ALTER TABLE ads.capi_contexts FORCE ROW LEVEL SECURITY;
ALTER TABLE ads.capi_events ENABLE ROW LEVEL SECURITY;    ALTER TABLE ads.capi_events FORCE ROW LEVEL SECURITY;
REVOKE ALL ON ads.capi_contexts, ads.capi_events FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE(user_agent,captured_at), DELETE ON ads.capi_contexts TO commerce_ads_writer;
GRANT SELECT, INSERT ON ads.capi_events TO commerce_ads_writer;
CREATE POLICY ads_writer_all ON ads.capi_contexts FOR ALL TO commerce_ads_writer USING (true) WITH CHECK (true);
CREATE POLICY ads_writer_all ON ads.capi_events FOR ALL TO commerce_ads_writer USING (true) WITH CHECK (true);

-- ---------------------------------------------------------------------------------------
-- Cross-domain column reads (§4.3 "column-level SELECT + read policy only on the listed columns"). A SELECT grant
-- WITHOUT a policy reads zero rows under FORCE RLS; each grant below names its policy and MA02 asserts each.
-- Definers filter tenant/store themselves (sweepers run without a GUC scope), so the policies limit rows only
-- where the row itself says so (active/published).
-- ---------------------------------------------------------------------------------------
GRANT SELECT(tenant_id,store_id,id,owner_id,order_id) ON checkout.payment_attempts TO commerce_ads_writer;
CREATE POLICY ads_writer_attempt_read ON checkout.payment_attempts FOR SELECT TO commerce_ads_writer USING (true);
GRANT SELECT(tenant_id,store_id,owner_id,id,quote_id) ON checkout.orders TO commerce_ads_writer;
CREATE POLICY ads_writer_order_read ON checkout.orders FOR SELECT TO commerce_ads_writer USING (true);
-- The quote snapshot carries the priced lines (sku_id, quantity) and no destination or PII (the order snapshot does).
GRANT SELECT(tenant_id,store_id,owner_id,id,snapshot) ON storefront.quotes TO commerce_ads_writer;
CREATE POLICY ads_writer_quote_read ON storefront.quotes FOR SELECT TO commerce_ads_writer USING (true);
GRANT SELECT(name,description) ON catalog.products TO commerce_ads_writer;   -- policy ads_writer_product_read (0074, active)
GRANT SELECT(tenant_id,store_id,id,product_id,status,currency,price_minor) ON catalog.skus TO commerce_ads_writer;
CREATE POLICY ads_writer_sku_read ON catalog.skus FOR SELECT TO commerce_ads_writer USING (status='active');
GRANT SELECT(tenant_id,store_id,sku_id,on_hand,reserved,allocated,unavailable) ON inventory.balances TO commerce_ads_writer;
CREATE POLICY ads_writer_balance_read ON inventory.balances FOR SELECT TO commerce_ads_writer USING (true);
GRANT SELECT(name) ON control.stores TO commerce_ads_writer;                 -- policy ads_writer_store_read (0074, active)

-- ---------------------------------------------------------------------------------------
-- Consent hook (A-3, C5): the buyer consent PUT calls this right after an ads_personalization grant, in the same
-- transaction. Upserts only when consent_allows is already true; a withdrawal never reaches it.
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION ads.put_capi_context(p_hash bytea,p_store uuid,p_user_agent text) RETURNS void
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v record;
BEGIN
 -- Go pre-validates (attribution.PutCAPIContext); these refusals are the backstop and abort the caller's transaction.
 IF p_hash IS NULL OR octet_length(p_hash)<>32 OR p_store IS NULL OR p_user_agent IS NULL
  OR char_length(p_user_agent) NOT BETWEEN 1 AND 512 THEN
  RAISE EXCEPTION 'invalid capi context' USING ERRCODE='PT400'; END IF;
 -- buyer.resolve_scope: the same capability check as customers.buyer_set_consent; never a caller-supplied owner.
 SELECT s.tenant_id,s.owner_id INTO v FROM buyer.resolve_scope(p_hash,p_store) s;
 IF NOT FOUND THEN RAISE EXCEPTION 'invalid buyer capability' USING ERRCODE='PT401'; END IF;
 -- customers.consent_allows: CD5, the only consent gate; no copy of consent state is kept here.
 IF customers.consent_allows(v.tenant_id,p_store,v.owner_id,'ads_personalization','meta_ads') THEN
  INSERT INTO ads.capi_contexts(tenant_id,store_id,owner_id,user_agent) VALUES(v.tenant_id,p_store,v.owner_id,p_user_agent)
  ON CONFLICT (tenant_id,store_id,owner_id) DO UPDATE SET user_agent=excluded.user_agent,captured_at=clock_timestamp();
 END IF;
END $$;
REVOKE ALL ON FUNCTION ads.put_capi_context(bytea,uuid,text) FROM PUBLIC;
ALTER FUNCTION ads.put_capi_context(bytea,uuid,text) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.put_capi_context(bytea,uuid,text) TO commerce_buyer_runtime;
COMMENT ON FUNCTION ads.put_capi_context(bytea,uuid,text) IS
 'ads owner; only caller internal/attribution.PutCAPIContext from the buyer consent PUT (commerce_buyer_runtime), same transaction, after an ads_personalization grant (A-3). Resolves the buyer capability (buyer.resolve_scope) and upserts the browser user agent only when customers.consent_allows(ads_personalization, meta_ads) is already true. Stores nothing else; withdrawal or erasure makes the sweeper delete the row.';

-- ---------------------------------------------------------------------------------------
-- CAPI sweeper (§6.4): candidates, per-attempt eligibility, plan, purge. No network; the caller inserts the ads-lane
-- River job first and ads.verify_job re-checks it (same pattern as ads.plan_op).
-- ---------------------------------------------------------------------------------------
-- One test for "may this captured attempt still be planned now" (used by candidates, the caller's pre-check and plan).
CREATE FUNCTION ads.plan_capi_eligible(p_attempt uuid) RETURNS boolean
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE f record; a record; s ads.store_settings; b record;
BEGIN
 SELECT x.tenant_id,x.store_id,x.received_at,x.environment,x.execution_profile INTO f
  FROM payments.facts x WHERE x.attempt_id=p_attempt AND x.kind='CAPTURED';
 IF NOT FOUND THEN RETURN false; END IF;
 -- F14: an event older than 7 days rejects the whole request; plan only inside 6 days. PROVIDER_MOCK facts never go to Meta.
 IF f.execution_profile='PROVIDER_MOCK' OR f.received_at<clock_timestamp()-interval '6 days' THEN RETURN false; END IF;
 SELECT * INTO s FROM ads.store_settings x WHERE x.tenant_id=f.tenant_id AND x.store_id=f.store_id;
 IF NOT FOUND OR NOT s.capi_enabled OR s.capi_dataset_binding IS NULL OR s.capi_enabled_by IS NULL THEN RETURN false; END IF;
 -- AD8/AD9: the fact's environment must equal the store's ads environment; SANDBOX always carries a test event code.
 IF f.environment<>s.environment OR (s.environment='SANDBOX' AND s.capi_test_event_code IS NULL) THEN RETURN false; END IF;
 SELECT x.provider,x.enabled INTO b FROM integration.bindings x
  WHERE x.tenant_id=f.tenant_id AND x.store_id=f.store_id AND x.id=s.capi_dataset_binding;
 IF NOT FOUND OR b.provider<>'meta_dataset' OR NOT b.enabled THEN RETURN false; END IF;
 IF NOT identity.principal_holds(f.tenant_id,f.store_id,s.capi_enabled_by,ARRAY['ads:manage']) THEN RETURN false; END IF;
 IF EXISTS(SELECT 1 FROM ads.capi_events e WHERE e.tenant_id=f.tenant_id AND e.store_id=f.store_id AND e.attempt_id=p_attempt) THEN
  RETURN false; END IF;
 SELECT x.owner_id INTO a FROM checkout.payment_attempts x WHERE x.tenant_id=f.tenant_id AND x.store_id=f.store_id AND x.id=p_attempt;
 IF NOT FOUND THEN RETURN false; END IF;
 -- customers.consent_allows: CD5/AD8, checked in the planning transaction; a browser context captured <= 7 days ago must exist.
 IF NOT customers.consent_allows(f.tenant_id,f.store_id,a.owner_id,'ads_personalization','meta_ads') THEN RETURN false; END IF;
 IF NOT EXISTS(SELECT 1 FROM ads.capi_contexts c WHERE c.tenant_id=f.tenant_id AND c.store_id=f.store_id AND c.owner_id=a.owner_id
   AND c.captured_at>=clock_timestamp()-interval '7 days') THEN RETURN false; END IF;
 -- event_source_url needs a live storefront origin.
 RETURN EXISTS(SELECT 1 FROM control.storefront_domains d WHERE d.tenant_id=f.tenant_id AND d.store_id=f.store_id AND d.state='ACTIVE');
END $$;
REVOKE ALL ON FUNCTION ads.plan_capi_eligible(uuid) FROM PUBLIC;
ALTER FUNCTION ads.plan_capi_eligible(uuid) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.plan_capi_eligible(uuid) TO commerce_worker;
COMMENT ON FUNCTION ads.plan_capi_eligible(uuid) IS
 'ads owner; callers ads.plan_capi_candidates, ads.plan_capi and the capi_purchase_sweep_v1 job (commerce_worker). True iff the attempt has a CAPTURED fact within 6 days that is not PROVIDER_MOCK, its store has CAPI enabled on a current enabled meta_dataset binding whose enabling principal still holds ads:manage, fact environment = store environment (SANDBOX needs a test event code), no capi_events row yet, the buyer owner passes customers.consent_allows(ads_personalization, meta_ads) and has a browser context <= 7 days old, and the store has an ACTIVE storefront domain. Reads only.';

CREATE FUNCTION ads.plan_capi_candidates(p_limit integer) RETURNS SETOF uuid
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 IF p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 500 THEN RAISE EXCEPTION 'invalid ads sweep' USING ERRCODE='22023'; END IF;
 -- ponytail: scans the last 6 days of CAPTURED facts every run; add an index on payments.facts(received_at) or a
 -- planned-cursor when daily captures exceed ~10k.
 RETURN QUERY SELECT f.attempt_id FROM payments.facts f
  WHERE f.kind='CAPTURED' AND f.execution_profile<>'PROVIDER_MOCK' AND f.received_at>=clock_timestamp()-interval '6 days'
   AND NOT EXISTS(SELECT 1 FROM ads.capi_events e WHERE e.tenant_id=f.tenant_id AND e.store_id=f.store_id AND e.attempt_id=f.attempt_id)
   AND ads.plan_capi_eligible(f.attempt_id)
  ORDER BY f.received_at,f.attempt_id LIMIT p_limit;
END $$;
REVOKE ALL ON FUNCTION ads.plan_capi_candidates(integer) FROM PUBLIC;
ALTER FUNCTION ads.plan_capi_candidates(integer) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.plan_capi_candidates(integer) TO commerce_worker;
COMMENT ON FUNCTION ads.plan_capi_candidates(integer) IS
 'ads owner; only caller the capi_purchase_sweep_v1 job (commerce_worker). At most p_limit (1..500) attempt ids for which ads.plan_capi_eligible is true, oldest capture first. Reads only.';

CREATE FUNCTION ads.plan_capi(p_attempt uuid,p_op uuid,p_job bigint) RETURNS void
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE f record; s ads.store_settings; b record; v_request jsonb;
BEGIN
 IF p_attempt IS NULL OR p_op IS NULL OR p_job IS NULL OR p_job<=0 OR current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'invalid ads plan' USING ERRCODE='22023'; END IF;
 -- Serialize with ads.set_capi (ads.lock_settings takes FOR UPDATE): a disable committed first wins, else it waits for us.
 SELECT x.tenant_id,x.store_id INTO f FROM payments.facts x WHERE x.attempt_id=p_attempt AND x.kind='CAPTURED';
 IF NOT FOUND THEN RAISE EXCEPTION 'invalid ads plan' USING ERRCODE='22023'; END IF;
 PERFORM 1 FROM ads.store_settings x WHERE x.tenant_id=f.tenant_id AND x.store_id=f.store_id FOR SHARE;
 IF NOT ads.plan_capi_eligible(p_attempt) THEN RAISE EXCEPTION 'invalid ads plan' USING ERRCODE='22023'; END IF;
 PERFORM ads.verify_job(p_job,p_op,3);
 SELECT * INTO s FROM ads.store_settings x WHERE x.tenant_id=f.tenant_id AND x.store_id=f.store_id;
 SELECT x.id,x.external_asset_id,x.semantic_version INTO b FROM integration.bindings x
  WHERE x.tenant_id=f.tenant_id AND x.store_id=f.store_id AND x.id=s.capi_dataset_binding;
 SELECT x.received_at,x.environment INTO f FROM payments.facts x WHERE x.attempt_id=p_attempt AND x.kind='CAPTURED';
 -- C4 frozen request: internal ids + event time only (no token, no PII). test_event_code iff SANDBOX (AD9).
 v_request:=jsonb_build_object('v',1,'attempt_id',p_attempt,'event_id','lc-purchase-'||p_attempt::text,
  'event_time',floor(extract(epoch FROM f.received_at))::bigint) -- contract: the fact's received_at in unix seconds, floored (a bigint cast rounds)
  ||CASE WHEN s.environment='SANDBOX' THEN jsonb_build_object('test_event_code',s.capi_test_event_code) ELSE '{}'::jsonb END;
 INSERT INTO integration.operations(tenant_id,store_id,id,principal_id,binding_id,binding_version,provider,external_asset_id,
  purpose,action,semantic_key,request_hash,request,job_id)
 VALUES(s.tenant_id,s.store_id,p_op,s.capi_enabled_by,b.id,b.semantic_version,'meta_dataset',b.external_asset_id,'marketing',
  'meta.capi.purchase','ads:capi:'||p_attempt::text,sha256(convert_to(v_request::text,'UTF8')),v_request,p_job);
 INSERT INTO integration.operation_events(tenant_id,store_id,operation_id,generation,state,mode,reason_code)
  VALUES(s.tenant_id,s.store_id,p_op,0,'READY','','operation_planned');
 INSERT INTO ads.capi_events(tenant_id,store_id,attempt_id,operation_id,event_id)
  VALUES(s.tenant_id,s.store_id,p_attempt,p_op,'lc-purchase-'||p_attempt::text);
END $$;
REVOKE ALL ON FUNCTION ads.plan_capi(uuid,uuid,bigint) FROM PUBLIC;
ALTER FUNCTION ads.plan_capi(uuid,uuid,bigint) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.plan_capi(uuid,uuid,bigint) TO commerce_worker;
COMMENT ON FUNCTION ads.plan_capi(uuid,uuid,bigint) IS
 'ads owner; only caller the capi_purchase_sweep_v1 job (commerce_worker) after inserting the priority-3 ads-lane River job in the same transaction. Re-checks ads.plan_capi_eligible under a FOR SHARE lock on the store settings, verifies the job (ads.verify_job), then inserts the READY meta.capi.purchase operation (actor MERCHANT, principal = capi_enabled_by, semantic key ads:capi:<attempt>, request per C4), its event and the ads.capi_events row (one per attempt, ever). Any failed precondition is an invariant breach (22023); no token, no PII, no network.';

CREATE FUNCTION ads.plan_capi_purge() RETURNS integer
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_n integer;
BEGIN
 -- customers.consent_allows: CD5; withdrawal or erasure turns it false and the context goes with it.
 DELETE FROM ads.capi_contexts c WHERE c.ctid IN (SELECT x.ctid FROM ads.capi_contexts x
  WHERE x.captured_at<clock_timestamp()-interval '8 days'
   OR NOT customers.consent_allows(x.tenant_id,x.store_id,x.owner_id,'ads_personalization','meta_ads') LIMIT 1000);
 GET DIAGNOSTICS v_n=ROW_COUNT;
 RETURN v_n;
END $$;
REVOKE ALL ON FUNCTION ads.plan_capi_purge() FROM PUBLIC;
ALTER FUNCTION ads.plan_capi_purge() OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.plan_capi_purge() TO commerce_worker;
COMMENT ON FUNCTION ads.plan_capi_purge() IS
 'ads owner; only caller the capi_purchase_sweep_v1 job (commerce_worker). Deletes up to 1000 ads.capi_contexts rows older than 8 days or whose owner no longer passes customers.consent_allows(ads_personalization, meta_ads); returns the count.';

-- ---------------------------------------------------------------------------------------
-- Dispatcher Check (§6.1 CAPI bullet, C7): PG only, no network, no secret; '' = allowed, else the BLOCKED_POLICY code.
-- The Go route returns nil in reconcile mode and never calls this there.
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION ads.check_capi(p_operation uuid) RETURNS text
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE o record; e record; a record; s ads.store_settings; f record; b record; v_time bigint;
BEGIN
 SELECT x.tenant_id,x.store_id,x.binding_id,x.request INTO o FROM integration.operations x
  WHERE x.id=p_operation AND x.provider='meta_dataset' AND x.action='meta.capi.purchase' AND x.actor_kind='MERCHANT';
 IF NOT FOUND THEN RETURN 'unknown_action'; END IF;
 IF coalesce(o.request->>'attempt_id','') !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  OR o.request->>'event_id' IS DISTINCT FROM 'lc-purchase-'||(o.request->>'attempt_id')
  OR coalesce(o.request->>'event_time','') !~ '^[0-9]{1,12}$' THEN RETURN 'invalid_request'; END IF;
 SELECT c.attempt_id INTO e FROM ads.capi_events c WHERE c.tenant_id=o.tenant_id AND c.store_id=o.store_id
  AND c.operation_id=p_operation AND c.attempt_id=(o.request->>'attempt_id')::uuid;
 IF NOT FOUND THEN RETURN 'invalid_request'; END IF;
 SELECT x.owner_id INTO a FROM checkout.payment_attempts x WHERE x.tenant_id=o.tenant_id AND x.store_id=o.store_id AND x.id=e.attempt_id;
 IF NOT FOUND THEN RETURN 'invalid_request'; END IF;
 -- customers.consent_allows: CD5/AD8/G09, re-checked at dispatch: a withdrawal after planning ends here, zero HTTP.
 IF NOT customers.consent_allows(o.tenant_id,o.store_id,a.owner_id,'ads_personalization','meta_ads') THEN RETURN 'consent_withdrawn'; END IF;
 SELECT * INTO s FROM ads.store_settings x WHERE x.tenant_id=o.tenant_id AND x.store_id=o.store_id;
 IF NOT FOUND OR NOT s.capi_enabled THEN RETURN 'capi_disabled'; END IF;
 SELECT x.provider,x.enabled INTO b FROM integration.bindings x
  WHERE x.tenant_id=o.tenant_id AND x.store_id=o.store_id AND x.id=o.binding_id;
 IF s.capi_dataset_binding IS DISTINCT FROM o.binding_id OR NOT FOUND OR b.provider<>'meta_dataset' OR NOT b.enabled THEN
  RETURN 'dataset_binding_changed'; END IF;
 IF s.capi_enabled_by IS NULL OR NOT identity.principal_holds(o.tenant_id,o.store_id,s.capi_enabled_by,ARRAY['ads:manage']) THEN
  RETURN 'capi_principal_revoked'; END IF;
 SELECT x.environment INTO f FROM payments.facts x WHERE x.tenant_id=o.tenant_id AND x.store_id=o.store_id
  AND x.attempt_id=e.attempt_id AND x.kind='CAPTURED';
 IF NOT FOUND OR f.environment<>s.environment THEN RETURN 'environment_mismatch'; END IF;
 v_time:=(o.request->>'event_time')::bigint;
 IF v_time<extract(epoch FROM clock_timestamp()-interval '6 days')::bigint THEN RETURN 'event_too_old'; END IF;
 RETURN '';
END $$;
REVOKE ALL ON FUNCTION ads.check_capi(uuid) FROM PUBLIC;
ALTER FUNCTION ads.check_capi(uuid) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.check_capi(uuid) TO commerce_worker;
COMMENT ON FUNCTION ads.check_capi(uuid) IS
 'ads owner; only caller internal/attribution/capiroute Check (dispatcher, dispatch mode, commerce_worker). For meta.capi.purchase ops: consent_withdrawn (customers.consent_allows), capi_disabled, dataset_binding_changed, capi_principal_revoked (capi_enabled_by no longer holds ads:manage), environment_mismatch (fact vs store), event_too_old (> 6 days). Billing never blocks CAPI (BD5). Returns the BLOCKED_POLICY code or empty; reads only PG.';

-- ---------------------------------------------------------------------------------------
-- Lease-fenced user data (§4.3, C2). The only place user data is read for CAPI: same lease fence as
-- integration.load_meta_ads_token, dispatch mode only (CAPI has no secret path in reconcile mode, so a reconcile
-- claim is refused). Called by the route's LoadSecret in the SAME transaction as the token loader, whose FOR SHARE
-- lock on the operation row is still held (this owner has no UPDATE privilege on operations to lock it itself).
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION ads.capi_user_data(p_operation uuid,p_generation bigint,p_lease_token bytea)
RETURNS TABLE(ph_e164 text,owner_id uuid,contents jsonb,value_minor bigint,currency text,event_source_url text,user_agent text)
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
#variable_conflict use_column
DECLARE o record; v_attempt uuid; f record; a record; q record; v_contents jsonb; v_ua text; v_origin text;
BEGIN
 IF p_operation IS NULL OR p_generation IS NULL OR p_generation<1 OR p_lease_token IS NULL OR octet_length(p_lease_token)<>32 THEN
  RAISE EXCEPTION 'invalid CAPI user data load' USING ERRCODE='22023'; END IF;
 SELECT x.tenant_id,x.store_id,x.state,x.lease_mode,x.generation,x.lease_until,x.lease_token_hash,x.request INTO o
  FROM integration.operations x WHERE x.id=p_operation AND x.provider='meta_dataset' AND x.action='meta.capi.purchase'
   AND x.actor_kind='MERCHANT';
 IF NOT FOUND THEN RAISE EXCEPTION 'CAPI user data unavailable' USING ERRCODE='P0002'; END IF;
 IF o.state<>'DISPATCHING' OR o.lease_mode<>'dispatch' OR o.generation<>p_generation OR o.lease_until IS NULL
  OR o.lease_until<=clock_timestamp() OR o.lease_token_hash IS DISTINCT FROM sha256(p_lease_token) THEN
  RAISE EXCEPTION 'CAPI user data lease conflict' USING ERRCODE='40001'; END IF;
 IF coalesce(o.request->>'attempt_id','') !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' THEN RETURN; END IF;
 v_attempt:=(o.request->>'attempt_id')::uuid;
 PERFORM 1 FROM ads.capi_events c WHERE c.tenant_id=o.tenant_id AND c.store_id=o.store_id AND c.operation_id=p_operation
  AND c.attempt_id=v_attempt;
 IF NOT FOUND THEN RETURN; END IF;
 SELECT x.amount_minor,x.currency INTO f FROM payments.facts x WHERE x.tenant_id=o.tenant_id AND x.store_id=o.store_id
  AND x.attempt_id=v_attempt AND x.kind='CAPTURED';
 IF NOT FOUND THEN RETURN; END IF;
 SELECT x.owner_id,x.order_id INTO a FROM checkout.payment_attempts x
  WHERE x.tenant_id=o.tenant_id AND x.store_id=o.store_id AND x.id=v_attempt;
 IF NOT FOUND THEN RETURN; END IF;
 SELECT z.snapshot INTO q FROM checkout.orders r JOIN storefront.quotes z ON z.tenant_id=r.tenant_id AND z.store_id=r.store_id
  AND z.owner_id=r.owner_id AND z.id=r.quote_id
  WHERE r.tenant_id=o.tenant_id AND r.store_id=o.store_id AND r.owner_id=a.owner_id AND r.id=a.order_id;
 IF NOT FOUND OR jsonb_typeof(q.snapshot->'lines')<>'array' THEN RETURN; END IF;
 -- AD10: contents[].id = catalog.skus id, the same id the feed publishes.
 SELECT jsonb_agg(jsonb_build_object('id',l.sku_id::text,'quantity',l.quantity) ORDER BY l.sku_id) INTO v_contents
  FROM jsonb_to_recordset(q.snapshot->'lines') AS l(sku_id uuid,quantity bigint);
 IF v_contents IS NULL OR jsonb_array_length(v_contents)>50 THEN RETURN; END IF;
 SELECT c.user_agent INTO v_ua FROM ads.capi_contexts c WHERE c.tenant_id=o.tenant_id AND c.store_id=o.store_id AND c.owner_id=a.owner_id;
 SELECT d.origin INTO v_origin FROM control.storefront_domains d WHERE d.tenant_id=o.tenant_id AND d.store_id=o.store_id
  AND d.state='ACTIVE' ORDER BY d.id LIMIT 1;
 IF v_ua IS NULL OR v_origin IS NULL THEN RETURN; END IF;   -- zero rows -> the route denies (no HTTP)
 IF o.lease_until<=clock_timestamp() THEN RAISE EXCEPTION 'CAPI user data lease conflict' USING ERRCODE='40001'; END IF;
 -- CD5: ph_e164 is NULL. A recipient phone (storefront.destination_snapshots.phone) may be sent only when checkout records
 -- recipient = buyer; nothing records that, so the column is not granted and ph is omitted (attribution.HashPhone ready).
 RETURN QUERY SELECT NULL::text,a.owner_id,v_contents,f.amount_minor,f.currency,v_origin||'/orders',v_ua;
END $$;
REVOKE ALL ON FUNCTION ads.capi_user_data(uuid,bigint,bytea) FROM PUBLIC;
ALTER FUNCTION ads.capi_user_data(uuid,bigint,bytea) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.capi_user_data(uuid,bigint,bytea) TO commerce_worker;
COMMENT ON FUNCTION ads.capi_user_data(uuid,bigint,bytea) IS
 'ads owner; only caller internal/attribution/capiroute LoadSecret (commerce_worker), in the same transaction as integration.load_meta_ads_token. Lease fence: operation is a meta.capi.purchase MERCHANT op in (DISPATCHING, dispatch), same generation, unexpired lease, matching token hash (40001/P0002 otherwise; reconcile claims are refused). Returns the buyer owner id, the quote line contents (sku id + quantity), the captured amount and currency (I05: minor units), the storefront origin + /orders and the stored browser user agent; zero rows when any input is gone. ph_e164 is always NULL (CD5). The caller hashes in memory and never stores or logs the result.';

-- ---------------------------------------------------------------------------------------
-- Public product feed (§7, AD10, C6). The store is resolved from the verified origin inside the definer.
-- ---------------------------------------------------------------------------------------
CREATE FUNCTION ads.feed_rows(p_origin text)
RETURNS TABLE(id text,title text,description text,availability text,price_minor bigint,currency text,link text,brand text)
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
#variable_conflict use_column
DECLARE v_store uuid; v_tenant uuid; v_name text; v_currency text;
BEGIN
 -- buyer.resolve_published_store: 0020, the only origin -> store mapping; an invalid origin raises PT400 there.
 SELECT r.store_id INTO v_store FROM buyer.resolve_published_store(p_origin) r;
 IF NOT FOUND THEN RAISE EXCEPTION 'storefront not published' USING ERRCODE='PT404'; END IF;
 SELECT s.tenant_id,s.name,s.currency INTO v_tenant,v_name,v_currency FROM control.stores s WHERE s.id=v_store;
 IF NOT FOUND THEN RAISE EXCEPTION 'storefront not published' USING ERRCODE='PT404'; END IF;
 -- ponytail: 20000 SKUs per feed (one Meta feed file is far larger, but a pilot store is not); paginate when a store nears it.
 RETURN QUERY SELECT k.id::text,p.name,CASE WHEN p.description<>'' THEN p.description ELSE p.name END,
   CASE WHEN coalesce(b.available,0)>0 THEN 'in stock' ELSE 'out of stock' END,k.price_minor,k.currency,
   p_origin||'/products/'||p.id::text,v_name
  FROM catalog.skus k JOIN catalog.products p ON p.tenant_id=k.tenant_id AND p.store_id=k.store_id AND p.id=k.product_id
  LEFT JOIN LATERAL (SELECT sum(x.on_hand-x.reserved-x.allocated-x.unavailable) AS available FROM inventory.balances x
    WHERE x.tenant_id=k.tenant_id AND x.store_id=k.store_id AND x.sku_id=k.id) b ON true
  WHERE k.tenant_id=v_tenant AND k.store_id=v_store AND k.status='active' AND p.status='active' AND k.currency=v_currency
  ORDER BY k.id LIMIT 20000;
END $$;
REVOKE ALL ON FUNCTION ads.feed_rows(text) FROM PUBLIC;
ALTER FUNCTION ads.feed_rows(text) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.feed_rows(text) TO commerce_buyer_runtime;
COMMENT ON FUNCTION ads.feed_rows(text) IS
 'ads owner; only caller internal/attribution.FeedHandler (commerce_buyer_runtime). Resolves the store from the verified storefront origin via buyer.resolve_published_store (PT400 invalid origin, PT404 not published) and returns only active SKUs of active products in the store currency: id = catalog.skus id, availability from inventory.balances (on_hand - reserved - allocated - unavailable > 0), price in minor units, link = origin + /products/<product id>, brand = store name. No store parameter, no PII.';

-- ---------------------------------------------------------------------------------------
-- Documentation (PROCESS §5).
-- ---------------------------------------------------------------------------------------
COMMENT ON TABLE ads.capi_contexts IS
 'internal/attribution: browser user agent of a buyer who granted ads_personalization (CAPI client_user_data, F14). Written only by ads.put_capi_context (consent hook); deleted by ads.plan_capi_purge after 8 days or when consent is withdrawn or erased; read by the CAPI definers. Non-goal: no IP, no cookie, no other buyer data; register the retention class with customers-billing CD7 (blocks production mount).';
COMMENT ON TABLE ads.capi_events IS
 'internal/attribution: one row per captured payment attempt that was planned for a CAPI Purchase, ever (event_id lc-purchase-<attempt>, AD8). Written only by ads.plan_capi; an UNKNOWN operation is never resent (F15). Non-goal: no user data, no hashes.';
COMMENT ON COLUMN ads.capi_contexts.tenant_id IS 'Scope (FK buyer.owners); written only by ads.put_capi_context.';
COMMENT ON COLUMN ads.capi_contexts.store_id IS 'Scope (FK buyer.owners); written only by ads.put_capi_context.';
COMMENT ON COLUMN ads.capi_contexts.owner_id IS 'The anonymous buyer owner whose consent gated this row (FK buyer.owners).';
COMMENT ON COLUMN ads.capi_contexts.user_agent IS 'Buyer browser User-Agent at consent time, 1..512 chars, control characters stripped by Go; sent as client_user_agent, purged after 8 days.';
COMMENT ON COLUMN ads.capi_contexts.captured_at IS 'When the context was (re)captured; the sweeper requires it within 7 days and purges at 8.';
COMMENT ON COLUMN ads.capi_events.tenant_id IS 'Scope (FK payments.facts); written only by ads.plan_capi.';
COMMENT ON COLUMN ads.capi_events.store_id IS 'Scope (FK payments.facts); written only by ads.plan_capi.';
COMMENT ON COLUMN ads.capi_events.attempt_id IS 'The captured payment attempt; primary key, so one CAPI Purchase per attempt, ever.';
COMMENT ON COLUMN ads.capi_events.operation_id IS 'The integration.operations row (meta.capi.purchase) that sends it; unique.';
COMMENT ON COLUMN ads.capi_events.event_id IS 'Stable CAPI event_id lc-purchase-<attempt uuid> (F15 dedup with a future pixel eventID).';
COMMENT ON COLUMN ads.capi_events.planned_at IS 'When the sweeper planned the operation.';
COMMENT ON COLUMN ads.capi_events.fact_kind IS 'Always CAPTURED; the generated column makes the FK to payments.facts a CAPTURED-only reference.';
COMMENT ON POLICY ads_writer_all ON ads.capi_contexts IS
 'T16 CAPI (migrations/0080): commerce_ads_writer only, for the ads.* definers of internal/attribution; removing it makes them read zero rows, so MA02 asserts it exists. Non-goal: no buyer or merchant access.';
COMMENT ON POLICY ads_writer_all ON ads.capi_events IS
 'T16 CAPI (migrations/0080): commerce_ads_writer only, for ads.plan_capi and the CAPI Check/user-data definers; removing it makes the sweeper plan duplicates, so MA02 asserts it exists. Non-goal: no buyer or merchant access.';
DO $$
DECLARE r record;
BEGIN
 FOR r IN SELECT * FROM (VALUES
  ('ads_writer_attempt_read','checkout.payment_attempts'),('ads_writer_order_read','checkout.orders'),
  ('ads_writer_quote_read','storefront.quotes'),('ads_writer_sku_read','catalog.skus'),
  ('ads_writer_balance_read','inventory.balances')) AS v(pol,tbl)
 LOOP
  EXECUTE format('COMMENT ON POLICY %I ON %s IS %L',r.pol,r.tbl,
   'T16 CAPI (migrations/0080): column-level read of commerce_ads_writer for ads.plan_capi*, ads.check_capi, ads.capi_user_data and ads.feed_rows only (owner package internal/attribution); removing it makes them read zero rows, so MA02 asserts it exists. Non-goal: no merchant or buyer access, no PII column (phone, address, name) is granted.');
 END LOOP;
END $$;
