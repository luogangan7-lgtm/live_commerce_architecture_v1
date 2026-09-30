-- 0075 Meta ads insights (T15; contracts/meta-ads-v1.md §4.2 + §6.2/§6.3 ingestion, D8, D9).
--
-- Owns: ads.insight_reads (one marker per read operation) and ads.insights_daily (one row per campaign and
-- day), the ingestion definer ads.put_insights_day, the pending-read lister and the merchant report definer.
--
-- Non-goals: no Graph call (read operations run in cmd/ads-worker through the dispatcher), no planning (ads
-- definers of 0074 plan the reads), no budget change, no re-activation. Rows with final=true never change.
--
-- Depends on: 0074 (schema ads, commerce_ads_writer, ads.campaign_drafts, ads.auth), 0008 (operations),
-- 0018/0062 (payments.facts, payments.refund_facts for the orders block, granted in 0074).
--
-- Callers: ads.put_insights_day and ads.pending_insight_reads: the advance sweeper (commerce_worker);
-- ads.report: the merchant report route (commerce_runtime, ads:read).

CREATE TABLE ads.insight_reads (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, operation_id uuid PRIMARY KEY,
 draft_id uuid NOT NULL, day date NOT NULL, ingested_at timestamptz,
 FOREIGN KEY(tenant_id,store_id,operation_id) REFERENCES integration.operations(tenant_id,store_id,id),
 FOREIGN KEY(tenant_id,store_id,draft_id) REFERENCES ads.campaign_drafts(tenant_id,store_id,id));
CREATE INDEX insight_reads_pending ON ads.insight_reads(operation_id) WHERE ingested_at IS NULL;
CREATE TABLE ads.insights_daily (
 tenant_id uuid NOT NULL, store_id uuid NOT NULL, draft_id uuid NOT NULL, campaign_remote_id text NOT NULL CHECK(campaign_remote_id ~ '^[0-9]{1,40}$'),
 day date NOT NULL,
 account_timezone text NOT NULL CHECK(account_timezone ~ '^[A-Za-z0-9_./+-]{1,40}$'), currency text NOT NULL CHECK(currency IN ('TWD','USD','HKD')),
 spend_minor bigint NOT NULL CHECK(spend_minor >= 0), impressions bigint NOT NULL CHECK(impressions>=0),
 clicks bigint NOT NULL CHECK(clicks>=0), meta_purchases bigint CHECK(meta_purchases>=0), meta_purchase_value_minor bigint CHECK(meta_purchase_value_minor>=0),
 effective_status text NOT NULL CHECK(effective_status ~ '^[A-Za-z0-9_./+-]{1,40}$'),
 source_operation_id uuid NOT NULL REFERENCES ads.insight_reads(operation_id),
 fetched_at timestamptz NOT NULL, final boolean NOT NULL DEFAULT false,
 PRIMARY KEY(tenant_id,store_id,campaign_remote_id,day),
 FOREIGN KEY(tenant_id,store_id,draft_id) REFERENCES ads.campaign_drafts(tenant_id,store_id,id));
CREATE INDEX insights_daily_draft ON ads.insights_daily(draft_id,day DESC);

ALTER TABLE ads.insight_reads ENABLE ROW LEVEL SECURITY;   ALTER TABLE ads.insight_reads FORCE ROW LEVEL SECURITY;
ALTER TABLE ads.insights_daily ENABLE ROW LEVEL SECURITY;  ALTER TABLE ads.insights_daily FORCE ROW LEVEL SECURITY;
REVOKE ALL ON ads.insight_reads, ads.insights_daily FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE(ingested_at) ON ads.insight_reads TO commerce_ads_writer;
GRANT SELECT, INSERT, UPDATE(account_timezone,currency,spend_minor,impressions,clicks,meta_purchases,meta_purchase_value_minor,
 effective_status,source_operation_id,fetched_at,final) ON ads.insights_daily TO commerce_ads_writer;
CREATE POLICY ads_writer_all ON ads.insight_reads FOR ALL TO commerce_ads_writer USING (true) WITH CHECK (true);
CREATE POLICY ads_writer_all ON ads.insights_daily FOR ALL TO commerce_ads_writer USING (true) WITH CHECK (true);

-- Rows with final=true are immutable (F13: Meta insights do not change after 28 days).
CREATE FUNCTION ads.guard_insights_final() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF OLD.final THEN RAISE EXCEPTION 'final insights row is immutable' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
REVOKE ALL ON FUNCTION ads.guard_insights_final() FROM PUBLIC;
ALTER FUNCTION ads.guard_insights_final() OWNER TO commerce_ads_writer;
CREATE TRIGGER insights_daily_final BEFORE UPDATE ON ads.insights_daily
 FOR EACH ROW EXECUTE FUNCTION ads.guard_insights_final();
COMMENT ON FUNCTION ads.guard_insights_final() IS
 'internal/ads trigger function of ads.insights_daily only; no caller EXECUTE. A row with final=true (day older than 28 days, F13) never changes.';

CREATE FUNCTION ads.pending_insight_reads(p_limit integer) RETURNS SETOF uuid
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 IF p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 500 THEN RAISE EXCEPTION 'invalid ads sweep' USING ERRCODE='22023'; END IF;
 -- Every SUCCEEDED read is ingested regardless of the draft's derived status (contract 6.3).
 RETURN QUERY SELECT ir.operation_id FROM ads.insight_reads ir JOIN integration.operations o ON o.id=ir.operation_id
  WHERE ir.ingested_at IS NULL AND o.state='SUCCEEDED' ORDER BY o.updated_at,ir.operation_id LIMIT p_limit;
END $$;
REVOKE ALL ON FUNCTION ads.pending_insight_reads(integer) FROM PUBLIC;
ALTER FUNCTION ads.pending_insight_reads(integer) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.pending_insight_reads(integer) TO commerce_worker;
COMMENT ON FUNCTION ads.pending_insight_reads(integer) IS
 'ads owner; only caller the ads_publish_advance_v1 job (commerce_worker). SUCCEEDED read_insights operations whose result is not ingested yet, oldest first.';

-- Parses the operation's provider_reference itself (contract 4.2): grammar v1;es=..;sp=..;im=..;cl=..;pu=..;pv=..;cur=..;tz=..
-- (external constant: contract meta-ads-v1 §3 read results, adapter internal/integrations/meta_ads).
CREATE FUNCTION ads.put_insights_day(p_operation uuid) RETURNS boolean
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE ir ads.insight_reads; o record; v_ref text; v_es text; v_sp text; v_im text; v_cl text; v_pu text; v_pv text; v_cur text; v_tz text;
 v_campaign text; v_spend bigint; v_pv_minor bigint; v_final boolean; v_today date;
BEGIN
 SELECT * INTO ir FROM ads.insight_reads x WHERE x.operation_id=p_operation FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'invalid ads ingest' USING ERRCODE='22023'; END IF;
 IF ir.ingested_at IS NOT NULL THEN RETURN false; END IF;      -- idempotent
 SELECT x.state,x.provider_reference,x.updated_at,x.request INTO o FROM integration.operations x
  WHERE x.id=p_operation AND x.action='meta.ads.read_insights' AND x.provider='meta_ads';
 IF NOT FOUND THEN RAISE EXCEPTION 'invalid ads ingest' USING ERRCODE='22023'; END IF;
 IF o.state<>'SUCCEEDED' THEN RETURN false; END IF;
 v_ref:=o.provider_reference;
 IF v_ref !~ '^v1(;[a-z]{2,3}=[A-Za-z0-9_./+-]{1,40}){1,9}$' THEN RAISE EXCEPTION 'invalid ads ingest' USING ERRCODE='22023'; END IF;
 v_es:=substring(v_ref from '(?:^|;)es=([^;]+)'); v_sp:=substring(v_ref from '(?:^|;)sp=([^;]+)');
 v_im:=substring(v_ref from '(?:^|;)im=([^;]+)'); v_cl:=substring(v_ref from '(?:^|;)cl=([^;]+)');
 v_pu:=substring(v_ref from '(?:^|;)pu=([^;]+)'); v_pv:=substring(v_ref from '(?:^|;)pv=([^;]+)');
 v_cur:=substring(v_ref from '(?:^|;)cur=([^;]+)'); v_tz:=substring(v_ref from '(?:^|;)tz=([^;]+)');
 v_campaign:=o.request->>'campaign_id';
 -- I05: exact decimal to minor units (hundredths for TWD/USD/HKD), never rounded; anything else is refused.
 IF v_es IS NULL OR v_sp IS NULL OR v_im IS NULL OR v_cl IS NULL OR v_cur IS NULL OR v_tz IS NULL
  OR v_sp !~ '^[0-9]{1,15}(\.[0-9]{1,2})?$' OR v_im !~ '^[0-9]{1,15}$' OR v_cl !~ '^[0-9]{1,15}$'
  OR (v_pu IS NOT NULL AND v_pu<>'na' AND v_pu !~ '^[0-9]{1,15}$')
  OR (v_pv IS NOT NULL AND v_pv<>'na' AND v_pv !~ '^[0-9]{1,15}(\.[0-9]{1,2})?$')
  OR v_cur NOT IN ('TWD','USD','HKD') OR v_campaign IS NULL OR v_campaign !~ '^[0-9]{1,40}$' THEN
  RAISE EXCEPTION 'invalid ads ingest' USING ERRCODE='22023'; END IF;
 v_spend:=split_part(v_sp,'.',1)::bigint*100+rpad(split_part(v_sp,'.',2),2,'0')::bigint;
 v_pv_minor:=CASE WHEN v_pv IS NULL OR v_pv='na' THEN NULL ELSE split_part(v_pv,'.',1)::bigint*100+rpad(split_part(v_pv,'.',2),2,'0')::bigint END;
 BEGIN
  v_today:=(clock_timestamp() AT TIME ZONE v_tz)::date;
 EXCEPTION WHEN invalid_parameter_value THEN v_today:=(clock_timestamp() AT TIME ZONE 'UTC')::date;   -- unknown zone name: UTC (never finalizes early)
 END;
 v_final:=ir.day<=v_today-28;
 INSERT INTO ads.insights_daily(tenant_id,store_id,draft_id,campaign_remote_id,day,account_timezone,currency,spend_minor,impressions,
  clicks,meta_purchases,meta_purchase_value_minor,effective_status,source_operation_id,fetched_at,final)
 VALUES(ir.tenant_id,ir.store_id,ir.draft_id,v_campaign,ir.day,v_tz,v_cur,v_spend,v_im::bigint,v_cl::bigint,
  CASE WHEN v_pu IS NULL OR v_pu='na' THEN NULL ELSE v_pu::bigint END,v_pv_minor,v_es,ir.operation_id,o.updated_at,v_final)
 ON CONFLICT (tenant_id,store_id,campaign_remote_id,day) DO UPDATE SET account_timezone=EXCLUDED.account_timezone,
  currency=EXCLUDED.currency,spend_minor=EXCLUDED.spend_minor,impressions=EXCLUDED.impressions,clicks=EXCLUDED.clicks,
  meta_purchases=EXCLUDED.meta_purchases,meta_purchase_value_minor=EXCLUDED.meta_purchase_value_minor,
  effective_status=EXCLUDED.effective_status,source_operation_id=EXCLUDED.source_operation_id,fetched_at=EXCLUDED.fetched_at,final=EXCLUDED.final
  WHERE EXCLUDED.fetched_at>=ads.insights_daily.fetched_at;       -- an older read never overwrites a newer one
 UPDATE ads.insight_reads x SET ingested_at=clock_timestamp() WHERE x.operation_id=ir.operation_id;
 RETURN true;
END $$;
REVOKE ALL ON FUNCTION ads.put_insights_day(uuid) FROM PUBLIC;
ALTER FUNCTION ads.put_insights_day(uuid) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.put_insights_day(uuid) TO commerce_worker;
COMMENT ON FUNCTION ads.put_insights_day(uuid) IS
 'ads owner; only caller the ads_publish_advance_v1 job (commerce_worker). Ingests one SUCCEEDED read_insights operation: parses its provider_reference (v1;es;sp;im;cl;pu;pv;cur;tz) with exact decimal to minor conversion (I05, never rounded), upserts ads.insights_daily (older reads never overwrite newer; final rows are immutable, days older than 28 store-local days become final) and marks the read ingested. Idempotent.';

CREATE FUNCTION ads.report(p_hash bytea,p_store uuid,p_from date,p_to date) RETURNS jsonb
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE a record; v_cur text; v_cap bigint; v_ref bigint; v_orders jsonb; v_del record; v_rep record; v_final date;
BEGIN
 SELECT * INTO a FROM ads.auth(p_hash,p_store,ARRAY['ads:read']);
 IF p_from IS NULL OR p_to IS NULL OR p_to<p_from OR p_to-p_from>91 THEN PERFORM ads.deny('invalid_request'); END IF;
 SELECT coalesce((SELECT s.allowance_currency FROM ads.store_settings s WHERE s.tenant_id=a.out_tenant AND s.store_id=p_store),'TWD') INTO v_cur;
 -- Block 1 (D8): CAPTURED card payments minus SUCCEEDED refunds received in the window, store-local days
 -- (Asia/Taipei; no store timezone column exists). I05: one currency, minor units. Pay-at-pickup orders have no payment facts.
 SELECT coalesce(sum(f.amount_minor),0)::bigint INTO v_cap FROM payments.facts f
  WHERE f.tenant_id=a.out_tenant AND f.store_id=p_store AND f.kind='CAPTURED' AND f.currency=v_cur
   AND (f.received_at AT TIME ZONE 'Asia/Taipei')::date BETWEEN p_from AND p_to;
 SELECT coalesce(sum(f.amount_minor),0)::bigint INTO v_ref FROM payments.refund_facts f
  WHERE f.tenant_id=a.out_tenant AND f.store_id=p_store AND f.kind='SUCCEEDED' AND f.currency=v_cur
   AND (f.received_at AT TIME ZONE 'Asia/Taipei')::date BETWEEN p_from AND p_to;
 v_orders:=jsonb_build_object('captured_minor',v_cap,'refunded_minor',v_ref,'net_minor',v_cap-v_ref,'currency',v_cur,
  'note','card_payments_only','fetched_at',ads.ts(clock_timestamp()));
 -- Blocks 2 and 3: Meta-reported delivery and purchases, kept apart from block 1 (arch 15.5), never blended.
 SELECT coalesce(sum(i.spend_minor),0)::bigint AS spend,coalesce(sum(i.impressions),0)::bigint AS imp,coalesce(sum(i.clicks),0)::bigint AS clk,
  max(i.fetched_at) AS fetched,
  CASE WHEN count(DISTINCT i.account_timezone)=1 THEN min(i.account_timezone) WHEN count(*)=0 THEN NULL ELSE 'mixed' END AS tz
  INTO v_del FROM ads.insights_daily i
  WHERE i.tenant_id=a.out_tenant AND i.store_id=p_store AND i.currency=v_cur AND i.day BETWEEN p_from AND p_to;
 SELECT sum(i.meta_purchases)::bigint AS purchases,sum(i.meta_purchase_value_minor)::bigint AS value,max(i.fetched_at) AS fetched
  INTO v_rep FROM ads.insights_daily i
  WHERE i.tenant_id=a.out_tenant AND i.store_id=p_store AND i.currency=v_cur AND i.day BETWEEN p_from AND p_to;
 SELECT max(i.day) INTO v_final FROM ads.insights_daily i WHERE i.tenant_id=a.out_tenant AND i.store_id=p_store AND i.final AND i.day<=p_to;
 RETURN jsonb_build_object('window',jsonb_build_object('from',to_char(p_from,'YYYY-MM-DD'),'to',to_char(p_to,'YYYY-MM-DD')),
  'timezone','Asia/Taipei','orders',v_orders,
  'meta_delivery',jsonb_build_object('spend_minor',v_del.spend,'impressions',v_del.imp,'clicks',v_del.clk,'currency',v_cur,
   'account_timezone',coalesce(v_del.tz,'Asia/Taipei'),'fetched_at',CASE WHEN v_del.fetched IS NULL THEN NULL ELSE to_jsonb(ads.ts(v_del.fetched)) END,
   'final_through',CASE WHEN v_final IS NULL THEN NULL ELSE to_jsonb(to_char(v_final,'YYYY-MM-DD')) END),
  'meta_reported',jsonb_build_object('purchases',coalesce(v_rep.purchases,0),'purchase_value_minor',coalesce(v_rep.value,0),'currency',v_cur,
   'fetched_at',CASE WHEN v_rep.fetched IS NULL THEN NULL ELSE to_jsonb(ads.ts(v_rep.fetched)) END));
END $$;
REVOKE ALL ON FUNCTION ads.report(bytea,uuid,date,date) FROM PUBLIC;
ALTER FUNCTION ads.report(bytea,uuid,date,date) OWNER TO commerce_ads_writer;
GRANT EXECUTE ON FUNCTION ads.report(bytea,uuid,date,date) TO commerce_runtime;
COMMENT ON FUNCTION ads.report(bytea,uuid,date,date) IS
 'ads owner; only caller internal/ads.Service.Report (commerce_runtime, ads:read). Three separate blocks (arch 15.5): local orders net of refunds (CAPTURED minus SUCCEEDED refund facts of the store currency in the window, Asia/Taipei days, card payments only), Meta delivery (spend, impressions, clicks) and Meta-reported purchases; each with its window, timezone and fetched_at. Window <= 92 days.';

COMMENT ON TABLE ads.insight_reads IS
 'internal/ads: one marker per read_insights operation (planned by ads.insights_plan, ingested by ads.put_insights_day). Written only by ads definers.';
COMMENT ON TABLE ads.insights_daily IS
 'internal/ads: Meta-reported delivery per campaign and day (account timezone), minor units of the account currency (I05). Upserted only by ads.put_insights_day; final rows are immutable. Not the local order ledger.';
DO $$
DECLARE r record; c text;
BEGIN
 FOR r IN SELECT * FROM (VALUES
  ('ads.insight_reads','tenant_id,store_id,operation_id,draft_id,day,ingested_at','Scope, the read operation, its draft and the store-local day it reads; ingested_at is set once by ads.put_insights_day.'),
  ('ads.insights_daily','tenant_id,store_id,draft_id,campaign_remote_id,day','Scope, draft and the Meta campaign/day this row reports.'),
  ('ads.insights_daily','account_timezone,currency,spend_minor,impressions,clicks,meta_purchases,meta_purchase_value_minor,effective_status','Meta-reported values for the day: account timezone and currency, spend and purchase value in minor units (exact decimal, I05), counts, and NULL purchases when Meta did not report them.'),
  ('ads.insights_daily','source_operation_id,fetched_at,final','The read operation the values came from, when they were fetched, and whether the day is final (older than 28 days, F13).')
 ) AS v(tbl,cols,why)
 LOOP
  FOREACH c IN ARRAY string_to_array(r.cols,',') LOOP
   IF col_description(r.tbl::regclass,(SELECT a.attnum FROM pg_attribute a WHERE a.attrelid=r.tbl::regclass AND a.attname=c)) IS NULL THEN
    EXECUTE format('COMMENT ON COLUMN %s.%I IS %L',r.tbl,c,r.why);
   END IF;
  END LOOP;
 END LOOP;
 EXECUTE format('COMMENT ON POLICY ads_writer_all ON ads.insight_reads IS %L','T15 ads (migrations/0075): policy of commerce_ads_writer for the ads definers only. Owner package internal/ads; non-goal: no merchant or buyer access.');
 EXECUTE format('COMMENT ON POLICY ads_writer_all ON ads.insights_daily IS %L','T15 ads (migrations/0075): policy of commerce_ads_writer for the ads definers only. Owner package internal/ads; non-goal: no merchant or buyer access.');
END $$;
