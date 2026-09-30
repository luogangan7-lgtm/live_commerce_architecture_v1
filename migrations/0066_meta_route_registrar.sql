-- 0066 Meta route registrar (R1 ruling F2): the operator path that lets a deployed store receive Meta
-- comments. meta_inbox.activate_route (0028, commerce_meta_registrar) needs an enabled integration.bindings
-- row with provider facebook|instagram for the Page/IG asset, and until now only tests (as the migration
-- owner) could insert one. This adds exactly one registrar definer that creates or reuses that binding;
-- route activation itself stays 0028's frozen activate_route/disable_route. Caller: cmd/meta-admin route
-- (metareply.RegisterRoute) under the lc_meta_registrar login, never a service.
-- Independent of 0064 at apply time (plpgsql resolves identity.principal_holds at call time), so the
-- MCI02 upgrade gate that applies every migration except 0064 still applies this file.

CREATE FUNCTION integration.register_meta_binding(p_tenant uuid,p_store uuid,p_principal uuid,p_provider text,p_asset text)
RETURNS TABLE(binding_id uuid,binding_version bigint) LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE b record;
BEGIN
 IF p_tenant IS NULL OR p_store IS NULL OR p_principal IS NULL OR p_provider IS NULL OR p_provider NOT IN ('facebook','instagram')
  OR p_asset IS NULL OR p_asset !~ '^[0-9]{1,40}$' THEN
  RAISE EXCEPTION 'invalid Meta binding input' USING ERRCODE='22023';
 END IF;
 -- Same owner validation as register_meta_page_token (contract meta-claims-intake-v1 §7): active principal,
 -- membership, tenant, store and an integration:manage grant on the store.
 IF NOT identity.principal_holds(p_tenant,p_store,p_principal,ARRAY['integration:manage']) THEN
  RAISE EXCEPTION 'Meta registrar scope unavailable' USING ERRCODE='42501';
 END IF;
 PERFORM set_config('app.tenant_id',p_tenant::text,true);
 PERFORM set_config('app.store_id',p_store::text,true);
 PERFORM set_config('app.principal_id',p_principal::text,true);
 -- One binding per store/provider/asset: serialize, then reuse the enabled one (re-running is idempotent).
 PERFORM pg_advisory_xact_lock(hashtextextended(jsonb_build_array('meta-binding',p_tenant,p_store,p_provider,p_asset)::text,0));
 SELECT x.id,x.semantic_version INTO b FROM integration.bindings x
  WHERE x.tenant_id=p_tenant AND x.store_id=p_store AND x.provider=p_provider AND x.external_asset_id=p_asset AND x.enabled
  ORDER BY x.created_at LIMIT 1;
 IF FOUND THEN
  RETURN QUERY SELECT b.id,b.semantic_version;
  RETURN;
 END IF;
 INSERT INTO integration.bindings(tenant_id,store_id,principal_id,provider,external_asset_id)
  VALUES(p_tenant,p_store,p_principal,p_provider,p_asset) RETURNING id,semantic_version INTO b;
 INSERT INTO ops.audit_events(tenant_id,store_id,principal_id,action) VALUES(p_tenant,p_store,p_principal,'meta.binding_registered');
 RETURN QUERY SELECT b.id,b.semantic_version;
END $$;
ALTER FUNCTION integration.register_meta_binding(uuid,uuid,uuid,text,text) OWNER TO commerce_integration_writer;
REVOKE ALL ON FUNCTION integration.register_meta_binding(uuid,uuid,uuid,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION integration.register_meta_binding(uuid,uuid,uuid,text,text) TO commerce_meta_registrar;
COMMENT ON FUNCTION integration.register_meta_binding(uuid,uuid,uuid,text,text) IS
 'integration owner; only caller cmd/meta-admin route (commerce_meta_registrar, R1 ruling F2). Returns the enabled facebook/instagram binding of the store for the Page/IG asset, creating it (audited meta.binding_registered) when none exists; requires an active principal holding integration:manage (42501). Never activates a route (0028 activate_route does), never touches credentials.';

-- The definer owner inserts only Meta bindings of the scope the definer pinned (GUCs set above).
GRANT INSERT(tenant_id,store_id,principal_id,provider,external_asset_id) ON integration.bindings TO commerce_integration_writer;
CREATE POLICY meta_registrar_binding_insert ON integration.bindings FOR INSERT TO commerce_integration_writer
 WITH CHECK (provider IN ('facebook','instagram')
  AND tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND principal_id=nullif(current_setting('app.principal_id',true),'')::uuid);
COMMENT ON POLICY meta_registrar_binding_insert ON integration.bindings IS
 '0066: INSERT for the integration.register_meta_binding definer (owner commerce_integration_writer) only; Meta providers and the pinned registrar scope.';
GRANT INSERT ON ops.audit_events TO commerce_integration_writer;
CREATE POLICY meta_binding_audit ON ops.audit_events FOR INSERT TO commerce_integration_writer
 WITH CHECK (tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND store_id=nullif(current_setting('app.store_id',true),'')::uuid
  AND principal_id=nullif(current_setting('app.principal_id',true),'')::uuid
  AND action='meta.binding_registered');
COMMENT ON POLICY meta_binding_audit ON ops.audit_events IS
 '0066: the audit row of integration.register_meta_binding (pinned registrar scope, one fixed action).';
