-- Post-River 0015: main-river guards of the Meta ads lane (contracts/meta-ads-v1.md §6, unit ads-core D4/A-5).
-- Installed after upstream River migrations, under the same migration lock as 0001-0014.
--
-- Owns: the BEFORE guard that admits queue `ads` on river.river_job to exactly the external_operation_v1 jobs
-- of ads/CAPI operations (queue ads, priority 1 = pause lane or 3, exact args) from the merchant runtime
-- (publish/pause/end) and the worker (sweepers), plus the four periodic kinds from the worker; the commit-time
-- reciprocal check that an ads operation's job is on queue ads and an ads-lane job belongs to an ads operation;
-- and the river.river_job read commerce_ads_writer needs to verify its planned job (plan_claim_reply pattern).
-- Non-goals: no new worker, queue configuration, job kind or payload (cmd/ads-worker registers them); existing
-- routes (default queue, claims intake, payments, meta) are untouched; no change to river_meta/payment/expiry.
-- Depends on: 0074 (commerce_ads_writer, ads schema), 0008 (integration.operations), 0064 (commerce_claims_intake),
-- post_river/0014 (guard_external_operation_job_link, which keeps requiring the operation to exist), upstream river.river_job.
-- Callers: none directly; triggers fire on INSERT/UPDATE of river.river_job.
--
-- Existing river.river_job triggers verified 2026-09-30 (grep -n river_job migrations/post_river/*.sql):
-- claims_intake_job_family (0014, claims-intake login only), external_operation_job_commit (0014, any external_operation_v1
-- insert: operation exists), meta_job_legacy (0004: kind meta_inbox_v1 / queue meta_inbox only), legacy_family_exclusion
-- (0005: payment/expiry kinds and queues only). None mentions queue `ads`, so none rejects or reroutes these jobs.

-- Table-level SELECT because ads.verify_job reads xmin (system columns need table SELECT), as 0014 does for the
-- integration writer. Read-only; the ads definers never write River.
GRANT USAGE ON SCHEMA river TO commerce_ads_writer;
GRANT SELECT ON river.river_job TO commerce_ads_writer;

CREATE FUNCTION integration.guard_ads_job() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_periodic constant text[]:=ARRAY['ads_publish_advance_v1','ads_insights_plan_v1','ads_oauth_purge_v1','capi_purchase_sweep_v1'];
BEGIN
 IF NEW.queue IS DISTINCT FROM 'ads' AND NOT (NEW.kind=ANY(v_periodic))
  AND NOT (TG_OP='UPDATE' AND (OLD.queue='ads' OR OLD.kind=ANY(v_periodic))) THEN
  RETURN NEW;                       -- not an ads job: the default lane and every other family are unchanged
 END IF;
 IF TG_OP='UPDATE' THEN
  -- River's own state transitions (fetch, complete, snooze, retry) are allowed; identity never changes.
  IF NEW.kind IS DISTINCT FROM OLD.kind OR NEW.args IS DISTINCT FROM OLD.args OR NEW.queue IS DISTINCT FROM OLD.queue
   OR NEW.unique_key IS DISTINCT FROM OLD.unique_key THEN
   RAISE EXCEPTION 'immutable ads job identity' USING ERRCODE='22023';
  END IF;
  RETURN NEW;
 END IF;
 -- The migration owner and fixtures are superusers (pg_has_role is true for them); every application login is judged.
 IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname=session_user AND rolsuper) THEN RETURN NEW; END IF;
 IF NEW.queue<>'ads' OR NEW.unique_key IS NOT NULL OR NEW.args IS NULL OR jsonb_typeof(NEW.args)<>'object' THEN
  RAISE EXCEPTION 'invalid ads job' USING ERRCODE='22023';
 END IF;
 IF NEW.kind='external_operation_v1' THEN
  IF NEW.priority NOT IN (1,3) OR NEW.args->>'operation_id' IS NULL
   OR NEW.args->>'operation_id' !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
   OR NEW.args IS DISTINCT FROM jsonb_build_object('operation_id',NEW.args->>'operation_id','version',1)
   OR NOT (pg_has_role(session_user,'commerce_runtime','MEMBER') OR pg_has_role(session_user,'commerce_worker','MEMBER')) THEN
   RAISE EXCEPTION 'invalid ads job' USING ERRCODE='22023';
  END IF;
 ELSIF NEW.kind=ANY(v_periodic) THEN
  IF NEW.priority NOT BETWEEN 1 AND 4 OR NEW.args<>'{}'::jsonb OR NOT pg_has_role(session_user,'commerce_worker','MEMBER') THEN
   RAISE EXCEPTION 'invalid ads job' USING ERRCODE='22023';
  END IF;
 ELSE
  RAISE EXCEPTION 'invalid ads job' USING ERRCODE='22023';   -- only the four periodic kinds and external_operation_v1 ride queue ads
 END IF;
 RETURN NEW;
END $$;
ALTER FUNCTION integration.guard_ads_job() OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION integration.guard_ads_job() FROM PUBLIC;
COMMENT ON FUNCTION integration.guard_ads_job() IS
 'integration owner; trigger function of river.river_job only (BEFORE INSERT OR UPDATE), no caller EXECUTE. Confines queue ads to external_operation_v1 jobs of exact args at priority 1 or 3 (merchant runtime and worker logins) and the four periodic kinds (worker login, args {}); never lets kind/args/queue/unique_key change; other queues and kinds are untouched.';
CREATE TRIGGER ads_job_family BEFORE INSERT OR UPDATE ON river.river_job
 FOR EACH ROW EXECUTE FUNCTION integration.guard_ads_job();

-- Reciprocal commit check: an ads/CAPI operation's job must ride queue ads (else cmd/claims-worker would claim it into
-- errRouteMissing), and an ads-lane operation job must belong to an ads operation whose job_id is this job.
CREATE FUNCTION integration.guard_ads_job_link() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_op integration.operations%ROWTYPE; v_ads boolean;
BEGIN
 SELECT o.* INTO v_op FROM integration.operations o WHERE o.id=(NEW.args->>'operation_id')::uuid;
 v_ads:=FOUND AND v_op.provider IN ('meta_ads','meta_dataset');
 -- Re-read the final row: a deferred trigger sees NEW as inserted, but queue could have been rewritten in the transaction.
 IF v_ads AND (NEW.queue<>'ads' OR v_op.job_id<>NEW.id) THEN
  RAISE EXCEPTION 'ads operation job outside the ads lane' USING ERRCODE='22023';
 END IF;
 IF NOT v_ads AND NEW.queue='ads' THEN
  RAISE EXCEPTION 'ads lane job without an ads operation' USING ERRCODE='22023';
 END IF;
 RETURN NULL;
END $$;
ALTER FUNCTION integration.guard_ads_job_link() OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION integration.guard_ads_job_link() FROM PUBLIC;
COMMENT ON FUNCTION integration.guard_ads_job_link() IS
 'integration owner; deferred constraint-trigger function of river.river_job only, no caller EXECUTE. At COMMIT an external_operation_v1 job of a meta_ads/meta_dataset operation must be on queue ads with operation.job_id = the job, and an ads-lane job must belong to such an operation; reads operations through worker_operation_read.';
CREATE CONSTRAINT TRIGGER ads_job_commit AFTER INSERT ON river.river_job
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.kind='external_operation_v1')
 EXECUTE FUNCTION integration.guard_ads_job_link();
