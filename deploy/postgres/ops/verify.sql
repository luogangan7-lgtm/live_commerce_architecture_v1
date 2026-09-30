-- File: deploy/postgres/ops/verify.sql
-- Purpose: post-restore checks (deploy-design §15): migration ledger row count (optionally equal
--   to the backup manifest), the readiness gates the DEPLOYED services depend on
--   (integration.payment_queue_ready, checkout.expiry_queue_ready, meta_inbox.runtime_ready),
--   and per-schema table counts. Prints only counts/booleans (no row values) and FAILS
--   (psql exit 3) when a check fails.
--   live.media_plan_ready() is reported but only REQUIRED with -v require_media=1: it pins an
--   md5 of pg_get_constraintdef() that PostgreSQL does not reproduce after pg_dump/pg_restore
--   (nested AND from BETWEEN is flattened on re-parse; VERIFIED_LOCAL 2026-09-28), so every
--   LOGICAL restore reports it false. Physical/PITR restores keep it true. LiveKit media is not
--   deployable in this release (COMMERCE_STUDIO_MEDIA_ENABLED=0 enforced by preflight P06).
--   Fix is REQUIRES_INTEGRATOR (restore-stable gate in migrations/).
-- Runs as/in: psql inside pg-ops as the superuser, against a restored database
--   (restore-dump.sh) or a paused PITR scratch cluster (restore-pitr.sh). Read-only queries,
--   so it also works in a hot-standby/paused recovery session.
-- Reads env / secrets: none. psql variables: :expected_ledger (-1 = do not compare),
--   :require_media (0|1, default 0).
-- Used by: restore-dump.sh, restore-pitr.sh; smoke S29, S31.
-- Depends on: public.lc_schema_migrations and the readiness functions created by migrations/.
-- Status: DESIGN; S29/S31 VERIFIED_LOCAL with scratch images. Change rules: add new readiness
--   gates here when migrations add them.
\set ON_ERROR_STOP 1
\if :{?expected_ledger}
\else
\set expected_ledger -1
\endif
\if :{?require_media}
\else
\set require_media 0
\endif
SELECT count(*) AS ledger_count, coalesce(max(version), '') AS ledger_max FROM public.lc_schema_migrations \gset
SELECT CASE WHEN :ledger_count > 0 AND (:expected_ledger < 0 OR :ledger_count = :expected_ledger)
            THEN 'ok' ELSE 'mismatch' END AS ledger_check \gset
\echo verify.ledger_count=:ledger_count expected=:expected_ledger max=:ledger_max check=:ledger_check
SELECT integration.payment_queue_ready() AS r_payment, checkout.expiry_queue_ready() AS r_expiry,
       meta_inbox.runtime_ready() AS r_meta, live.media_plan_ready() AS r_media \gset
\echo verify.ready payment_queue=:r_payment expiry_queue=:r_expiry meta_runtime=:r_meta media_plan=:r_media require_media=:require_media
SELECT 'verify.schema ' || n.nspname || ' tables=' || count(c.oid)
FROM pg_namespace n
LEFT JOIN pg_class c ON c.relnamespace = n.oid AND c.relkind IN ('r', 'p')
WHERE n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema'
GROUP BY n.nspname ORDER BY n.nspname;
SELECT (:'ledger_check' = 'ok' AND :'r_payment'::boolean AND :'r_expiry'::boolean
        AND :'r_meta'::boolean AND (:'r_media'::boolean OR :'require_media' = '0')) AS all_ok \gset
\if :all_ok
\echo verify.result=PASS
\else
\echo verify.result=FAIL
DO $$ BEGIN RAISE EXCEPTION 'verify.sql: checks failed'; END $$;
\endif
