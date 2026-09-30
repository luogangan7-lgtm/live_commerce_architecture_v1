-- Post-River 0014: main-river guards and grants of the Meta claims intake worker
-- (contracts/meta-claims-intake-v1.md §4.3 river rows and §5.4). Installed after upstream River
-- migrations, under the same migration lock as 0001-0013.
--
-- Owns: the BEFORE guard that confines the commerce_claims_intake login on river.river_job and
-- the commit-time reciprocal check that every external_operation_v1 job has its operation.
-- Non-goals: no new job kind, queue or worker; river_meta (Meta consumer lane) is untouched.
-- Depends on: 0064 (commerce_claims_intake), 0008 (integration.operations), upstream river.river_job.
-- Callers: none directly; triggers fire on INSERT/UPDATE of river.river_job.

GRANT USAGE ON SCHEMA river TO commerce_claims_intake;
-- River InsertTx always emits ON CONFLICT (unique_key) DO UPDATE SET kind = EXCLUDED.kind, and
-- PostgreSQL requires UPDATE(kind) for that statement even without a conflict (same as
-- commerce_runtime in migrate.go and meta_ingress in post-River 0003). The guard below rejects
-- any change of kind/args/queue/unique_key by this login.
GRANT SELECT, INSERT, UPDATE(kind) ON river.river_job TO commerce_claims_intake;
GRANT USAGE ON SEQUENCE river.river_job_id_seq TO commerce_claims_intake;
-- Table-level SELECT because plan_claim_reply reads xmin (system columns need table SELECT).
GRANT SELECT ON river.river_job TO commerce_integration_writer;

CREATE FUNCTION integration.guard_claims_intake_job() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 -- pg_has_role is true for every role to a superuser: the migration owner is not the intake login.
 IF NOT pg_has_role(session_user,'commerce_claims_intake','MEMBER')
  OR EXISTS(SELECT 1 FROM pg_roles WHERE rolname=session_user AND rolsuper) THEN
  RETURN NEW;
 END IF;
 IF TG_OP='INSERT' THEN
  IF NEW.kind<>'external_operation_v1' OR NEW.queue<>'default' OR NEW.unique_key IS NOT NULL
   OR NEW.args IS NULL OR jsonb_typeof(NEW.args)<>'object'
   OR NEW.args->>'operation_id' IS NULL
   OR NEW.args->>'operation_id' !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
   OR NEW.args IS DISTINCT FROM jsonb_build_object('operation_id',NEW.args->>'operation_id','version',1) THEN
   RAISE EXCEPTION 'invalid claims intake job' USING ERRCODE='22023';
  END IF;
 ELSIF NEW.kind IS DISTINCT FROM OLD.kind OR NEW.args IS DISTINCT FROM OLD.args
  OR NEW.queue IS DISTINCT FROM OLD.queue OR NEW.unique_key IS DISTINCT FROM OLD.unique_key THEN
  RAISE EXCEPTION 'immutable claims intake job identity' USING ERRCODE='22023';
 END IF;
 RETURN NEW;
END $$;
ALTER FUNCTION integration.guard_claims_intake_job() OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION integration.guard_claims_intake_job() FROM PUBLIC;
COMMENT ON FUNCTION integration.guard_claims_intake_job() IS
 'integration owner; trigger function of river.river_job only (BEFORE INSERT OR UPDATE), no caller EXECUTE. Confines the commerce_claims_intake login to inserting external_operation_v1 jobs of exact args and never changing kind/args/queue/unique_key.';
CREATE TRIGGER claims_intake_job_family BEFORE INSERT OR UPDATE ON river.river_job
 FOR EACH ROW EXECUTE FUNCTION integration.guard_claims_intake_job();

-- Every external_operation_v1 job must have its operation at COMMIT. The commerce_claims_intake login is held to
-- the contract's exact link (operation id AND job_id = this job). Every other login (the merchant Plan inserts
-- both in one transaction; the dispatcher gate TestT06DispatcherBusyDuplicateSnoozes deliberately inserts a
-- DUPLICATE job of an existing operation, which the dispatcher snoozes without effect) only needs the operation
-- to exist: an orphan job is still impossible.
CREATE FUNCTION integration.guard_external_operation_job_link() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE v_exact boolean;
BEGIN
 v_exact:=pg_has_role(session_user,'commerce_claims_intake','MEMBER')
  AND NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=session_user AND rolsuper);
 IF NOT EXISTS(SELECT 1 FROM integration.operations o WHERE o.id=(NEW.args->>'operation_id')::uuid
   AND (NOT v_exact OR o.job_id=NEW.id)) THEN
  RAISE EXCEPTION 'unlinked external operation job' USING ERRCODE='22023';
 END IF;
 RETURN NULL;
END $$;
ALTER FUNCTION integration.guard_external_operation_job_link() OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION integration.guard_external_operation_job_link() FROM PUBLIC;
COMMENT ON FUNCTION integration.guard_external_operation_job_link() IS
 'integration owner; deferred constraint-trigger function of river.river_job only, no caller EXECUTE. At COMMIT an integration.operations row with id=args.operation_id must exist (and, for the commerce_claims_intake login, job_id=the job); reads operations through worker_operation_read.';
CREATE CONSTRAINT TRIGGER external_operation_job_commit AFTER INSERT ON river.river_job
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.kind='external_operation_v1')
 EXECUTE FUNCTION integration.guard_external_operation_job_link();
