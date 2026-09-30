-- grant-r2-permissions.sql (customers-billing-v1 C-5 / §9, brief billing-core B11).
--
-- WHAT: gives ONE existing store creator the three R2 permissions customers:read, customers:privacy and
-- billing:manage on ONE store. Migration 0079 changes identity.create_initial_store for NEW stores only and
-- never backfills; a store created before 0079 (an R1 staging store, the pilot store) needs this script.
--
-- SAFETY (never run automatically; not a migration; needs the owner's approval in chat first):
--   * one transaction; every failed check aborts it and nothing is written;
--   * aborts unless the principal is an active member of the store's tenant AND already holds the FULL
--     creator grant set of 0065 (19 permissions) on that store, i.e. really is the store creator, so it can
--     never hand billing or privacy rights to an ordinary member;
--   * inserts each missing grant only, ON CONFLICT DO NOTHING (idempotent), and writes ONE audit row
--     `store.permissions_granted` only when something was inserted;
--   * touches no other principal, store, membership or grant and never revokes anything.
--
-- HOW (migration-owner connection; the values are ids, not secrets):
--   psql "$MIGRATION_DATABASE_URL" -v ON_ERROR_STOP=1 \
--        -v store_id=<store uuid> -v principal_id=<principal uuid> -f scripts/ops/grant-r2-permissions.sql
-- Prints one line: granted=<0..3>.

\set ON_ERROR_STOP on
\if :{?store_id}
\else
  -- psql's \quit ignores its argument (exit 0), so a usage error raises instead: ON_ERROR_STOP then exits 3.
  DO $$ BEGIN RAISE EXCEPTION 'usage: -v store_id=<uuid> -v principal_id=<uuid>'; END $$;
\endif
\if :{?principal_id}
\else
  -- psql's \quit ignores its argument (exit 0), so a usage error raises instead: ON_ERROR_STOP then exits 3.
  DO $$ BEGIN RAISE EXCEPTION 'usage: -v store_id=<uuid> -v principal_id=<uuid>'; END $$;
\endif

BEGIN;
-- psql substitutes :'var' only in top-level statements, so the DO block reads them back as settings.
SELECT set_config('lc.grant_store',:'store_id',true), set_config('lc.grant_principal',:'principal_id',true);

DO $$
DECLARE
  v_store uuid := current_setting('lc.grant_store')::uuid;
  v_principal uuid := current_setting('lc.grant_principal')::uuid;
  v_tenant uuid; v_missing text[]; v_new text[]; v_added integer;
  -- The 0065 creator set: 0027 (16) + 0063/0065 (live:read, live:manage, payments:refund, fulfillment:write,
  -- orders:export, integration:execute). Keep in step with identity.create_initial_store.
  v_creator constant text[] := ARRAY['store:read','audit:read','audit:write','catalog:read','catalog:write',
   'inventory:read','inventory:write','inventory:reserve','pricing:read','pricing:write','integration:read',
   'integration:manage','orders:read','live:read','live:manage','payments:refund','fulfillment:write',
   'orders:export','integration:execute'];
  v_grant constant text[] := ARRAY['customers:read','customers:privacy','billing:manage'];
BEGIN
  SELECT s.tenant_id INTO v_tenant FROM control.stores s WHERE s.id=v_store AND s.active;
  IF v_tenant IS NULL THEN RAISE EXCEPTION 'grant-r2-permissions: store not found or inactive'; END IF;
  PERFORM 1 FROM identity.principals p JOIN identity.memberships m ON m.principal_id=p.id
   WHERE p.id=v_principal AND p.active AND m.tenant_id=v_tenant AND m.active;
  IF NOT FOUND THEN RAISE EXCEPTION 'grant-r2-permissions: principal is not an active member of the store tenant'; END IF;
  SELECT array_agg(c) INTO v_missing FROM unnest(v_creator) c
   WHERE NOT EXISTS(SELECT 1 FROM identity.store_grants g WHERE g.tenant_id=v_tenant AND g.store_id=v_store
     AND g.principal_id=v_principal AND g.permission=c);
  IF v_missing IS NOT NULL THEN
    RAISE EXCEPTION 'grant-r2-permissions: principal lacks the store-creator grant set (missing: %)',v_missing;
  END IF;
  WITH ins AS (
    INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission)
    SELECT v_tenant,v_store,v_principal,p FROM unnest(v_grant) p
    ON CONFLICT (tenant_id,store_id,principal_id,permission) DO NOTHING
    RETURNING permission)
  SELECT coalesce(array_agg(permission),'{}'::text[]) INTO v_new FROM ins;
  v_added := coalesce(array_length(v_new,1),0);
  IF v_added>0 THEN
    INSERT INTO ops.audit_events(tenant_id,store_id,principal_id,action)
      VALUES(v_tenant,v_store,v_principal,'store.permissions_granted');
  END IF;
  PERFORM set_config('lc.grant_added',v_added::text,true);
END $$;
SELECT 'granted=' || current_setting('lc.grant_added') AS result;
COMMIT;
