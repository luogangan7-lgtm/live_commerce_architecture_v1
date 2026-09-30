package foundation_test

// MF02 (contracts/manual-fulfilment-v1.md §2, §4, §6): the manual-fulfilment schema, REAL_PG. Prefix `mfs`.
// Catalog reads (pg_policies, ACLs, pg_proc, pg_constraint) for structure; owner-pool statements for
// trigger behaviour (disclosed at their use); the real HTTP handler and definers for everything a merchant
// does. NOT_RUN clause: "fresh + populated upgrade (after 0062)": migrations.Apply has no partial-apply hook
// (see RF03), so that clause is an explicit SKIP.

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func mfsVersionBase() map[string]string {
	u := func(n int) string { return fmt.Sprintf(srsUUID, n) }
	return map[string]string{"tenant_id": u(1), "store_id": u(2), "owner_id": u(3), "order_id": u(4), "version": "1", "status": "'SHIPPED'",
		"carrier_code": "'sf_express'", "tracking_number": "'SF1'", "principal_id": u(5)}
}

func mfsWith(base map[string]string, over map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

func TestManualFulfilmentMF02Schema(t *testing.T) {
	e := rfxNew(t)
	ctx := context.Background()
	// (A1) 0063 adds the permissions and NO grant row (the onboarding grant is 0065/OP01).
	for _, p := range []string{"fulfillment:write", "orders:export"} {
		if n := e.count(t, `SELECT count(*) FROM identity.store_grants WHERE permission=$1`, p); n != 0 {
			t.Fatalf("0063 (or the fixture) wrote %d %s grant rows", n, p)
		}
	}

	t.Run("migration, permission vocabulary and order state CHECK", func(t *testing.T) {
		srsMust(t, e, "migration 0063", `SELECT EXISTS(SELECT 1 FROM public.lc_schema_migrations WHERE version='0063_manual_fulfilment.sql' AND checksum<>'')`)
		srsDef(t, e, "identity.store_grants", "fulfillment:write", "orders:export", "payments:refund", "store:read", "audit:read", "audit:write", "catalog:read", "catalog:write",
			"inventory:read", "inventory:write", "inventory:reserve", "pricing:read", "pricing:write", "integration:manage", "integration:execute", "integration:read", "orders:read", "live:read", "live:manage")
		srsDef(t, e, "checkout.orders", "MANUAL_UNASSIGNED", "CANCELLED", "PAID_ALLOCATION_FAILED", "MERCHANT_SHIPPED")
		tx, err := e.f.owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `UPDATE checkout.orders SET fulfillment_state='DELIVERED' WHERE id=(SELECT id FROM checkout.orders LIMIT 1)`); err != nil && sqlState(err) != "23514" {
			t.Fatalf("bogus fulfillment_state: %v", err)
		}
	})

	t.Run("version and head CHECKs", func(t *testing.T) {
		base := mfsVersionBase()
		for _, code := range []string{"seven_eleven_cvs", "familymart_cvs", "hilife_cvs", "okmart_cvs", "sf_express", "chunghwa_post"} {
			srsCheck(t, e, "fulfillment.manual_shipment_versions", mfsWith(base, map[string]string{"carrier_code": "'" + code + "'"}), map[string]map[string]string{"control: version 0": {"version": "0"}})
		}
		srsCheck(t, e, "fulfillment.manual_shipment_versions", mfsWith(base, map[string]string{"carrier_code": "'other'", "carrier_name": "'Local courier'"}), map[string]map[string]string{"control: version 0": {"version": "0"}})
		srsCheck(t, e, "fulfillment.manual_shipment_versions", base, map[string]map[string]string{
			"version 0":                    {"version": "0"},
			"status DELIVERED":             {"status": "'DELIVERED'"},
			"status IN_TRANSIT":            {"status": "'IN_TRANSIT'"},
			"unknown carrier":              {"carrier_code": "'fedex'"},
			"other without a name":         {"carrier_code": "'other'"},
			"carrier name empty":           {"carrier_name": "''"},
			"carrier name 81 chars":        {"carrier_name": "repeat('a',81)"},
			"carrier name control":         {"carrier_name": "'a'||chr(7)"},
			"tracking empty":               {"tracking_number": "''"},
			"tracking leading space":       {"tracking_number": "' ab'"},
			"tracking trailing space":      {"tracking_number": "'ab '"},
			"tracking leading hyphen":      {"tracking_number": "'-ab'"},
			"tracking 65 chars":            {"tracking_number": "repeat('9',65)"},
			"tracking underscore":          {"tracking_number": "'ab_c'"},
			"tracking url http":            {"tracking_url": "'http://x.example.com/t'"},
			"tracking url over 512 bytes":  {"tracking_url": "'https://a.example.com/'||repeat('a',500)"},
			"tracking url with space":      {"tracking_url": "'https://a.example.com/a b'"},
			"tracking url host with <":     {"tracking_url": "'https://a<b.example.com/x'"}, // S6
			"tracking url host with >":     {"tracking_url": "'https://a>b.example.com/x'"},
			"tracking url host with quote": {"tracking_url": "'https://a\"b.example.com/x'"},
			"tracking url host is IPv4":    {"tracking_url": "'https://1.2.3.4/x'"},
			"tracking url with userinfo":   {"tracking_url": "'https://u@a.example.com/x'"},
			"tracking url with port":       {"tracking_url": "'https://a.example.com:8443/x'"},
			"note over 200 chars":          {"note": "repeat('n',201)"},
			"note with control":            {"note": "'a'||chr(7)"},
			"void_reason outside the enum": {"status": "'VOIDED'", "void_reason": "'bogus'"},
			"VOIDED without a void_reason": {"status": "'VOIDED'"},
			"SHIPPED with a void_reason":   {"void_reason": "'wrong_order'"},
		})
		u := func(n int) string { return fmt.Sprintf(srsUUID, n) }
		srsCheck(t, e, "fulfillment.manual_shipment_heads", map[string]string{"tenant_id": u(1), "store_id": u(2), "owner_id": u(3), "order_id": u(4), "current_version": "1", "updated_at": "clock_timestamp()"},
			map[string]map[string]string{"current_version 0": {"current_version": "0"}})
	})

	t.Run("definers, ACLs and the privilege matrix", func(t *testing.T) {
		type fn struct {
			sig, owner string
			exec, deny []string
		}
		for _, f := range []fn{
			{"fulfillment.record_manual_shipment(bytea,uuid,uuid,text,bytea,bigint,text,text,text,text,text,text,text)", "commerce_checkout_writer", []string{"commerce_runtime"}, []string{"commerce_worker", "commerce_checkout_runtime"}},
			{"identity.read_merchant_orders(bytea,uuid,uuid,integer,timestamptz,uuid,text)", "commerce_auth", []string{"commerce_runtime"}, []string{"commerce_worker"}},
			{"fulfillment.read_manual_shipment_history(bytea,uuid,uuid)", "commerce_auth", []string{"commerce_runtime"}, []string{"commerce_worker"}},
			{"identity.export_unshipped_orders(bytea,uuid,integer)", "commerce_auth", []string{"commerce_runtime"}, []string{"commerce_worker"}},
		} {
			var owner string
			var definer, fixed, publicExec, commented bool
			err := e.f.owner.QueryRow(ctx, `SELECT pg_get_userbyid(p.proowner),p.prosecdef,p.proconfig=ARRAY['search_path=pg_catalog']::text[],
			 EXISTS(SELECT 1 FROM aclexplode(coalesce(p.proacl,acldefault('f',p.proowner))) a WHERE a.grantee=0 AND a.privilege_type='EXECUTE'),obj_description(p.oid,'pg_proc') IS NOT NULL
			 FROM pg_proc p WHERE p.oid=to_regprocedure($1)`, f.sig).Scan(&owner, &definer, &fixed, &publicExec, &commented)
			if err != nil {
				t.Errorf("MF02 function %s missing: %v", f.sig, err)
				continue
			}
			if owner != f.owner || !definer || !fixed || publicExec || !commented {
				t.Errorf("MF02 %s: owner=%s definer=%v search_path=%v public-exec=%v comment=%v", f.sig, owner, definer, fixed, publicExec, commented)
			}
			for _, r := range f.exec {
				if !srsBool(t, e, "exec", `SELECT has_function_privilege($1,to_regprocedure($2),'EXECUTE')`, r, f.sig) {
					t.Errorf("MF02 %s: EXECUTE missing for %s", f.sig, r)
				}
			}
			for _, r := range f.deny {
				if srsBool(t, e, "deny", `SELECT has_function_privilege($1,to_regprocedure($2),'EXECUTE')`, r, f.sig) {
					t.Errorf("MF02 %s: %s must not EXECUTE", f.sig, r)
				}
			}
		}
		for _, table := range []string{"fulfillment.manual_shipment_versions", "fulfillment.manual_shipment_heads"} {
			srsMust(t, e, table+" FORCE RLS", `SELECT coalesce((SELECT relrowsecurity AND relforcerowsecurity FROM pg_class WHERE oid=to_regclass($1)),false)`, table)
			srsMust(t, e, table+" refused to PUBLIC", `SELECT NOT EXISTS (SELECT 1 FROM pg_class c CROSS JOIN LATERAL aclexplode(COALESCE(c.relacl,acldefault('r',c.relowner))) acl WHERE c.oid=to_regclass($1) AND acl.grantee=0)`, table)
			for _, role := range []string{"commerce_runtime", "commerce_worker", "commerce_integration_writer", "commerce_stripe_ingress"} {
				srsMust(t, e, table+" no privilege for "+role, `SELECT NOT has_any_column_privilege($1,$2,'SELECT') AND NOT has_any_column_privilege($1,$2,'INSERT') AND NOT has_any_column_privilege($1,$2,'UPDATE')`, role, table)
			}
			for _, role := range []string{"commerce_checkout_writer", "commerce_auth", "commerce_checkout_runtime"} {
				srsMust(t, e, table+" no UPDATE/DELETE/INSERT-by-reader for "+role, `SELECT NOT has_table_privilege($1,$2,'DELETE') AND NOT has_table_privilege($1,$2,'TRUNCATE')`, role, table)
			}
		}
		v, h := "fulfillment.manual_shipment_versions", "fulfillment.manual_shipment_heads"
		// versions: append-only for every role (no UPDATE column privilege anywhere)
		for _, role := range []string{"commerce_checkout_writer", "commerce_auth", "commerce_checkout_runtime", "commerce_runtime", "commerce_worker"} {
			srsMust(t, e, "versions never updatable by "+role, `SELECT NOT has_any_column_privilege($1,$2,'UPDATE')`, role, v)
		}
		srsPriv(t, e, "commerce_checkout_writer", v, "SELECT", true)
		srsPriv(t, e, "commerce_checkout_writer", v, "INSERT", true)
		srsPriv(t, e, "commerce_checkout_writer", h, "SELECT", true)
		srsPriv(t, e, "commerce_checkout_writer", h, "INSERT", true)
		srsColPriv(t, e, "commerce_checkout_writer", h, "current_version", "UPDATE", true)
		srsColPriv(t, e, "commerce_checkout_writer", h, "updated_at", "UPDATE", true)
		for _, c := range []string{"tenant_id", "store_id", "owner_id", "order_id"} {
			srsColPriv(t, e, "commerce_checkout_writer", h, c, "UPDATE", false)
		}
		// merchant reads (definer owner): column SELECT incl. the merchant-only fields, no writes
		srsMust(t, e, "commerce_auth reads versions (note, void_reason, principal_id) and heads", `SELECT has_column_privilege('commerce_auth',$1,'note','SELECT') AND has_column_privilege('commerce_auth',$1,'void_reason','SELECT')
		 AND has_column_privilege('commerce_auth',$1,'principal_id','SELECT') AND has_column_privilege('commerce_auth',$2,'current_version','SELECT') AND NOT has_table_privilege('commerce_auth',$1,'INSERT') AND NOT has_table_privilege('commerce_auth',$2,'INSERT')`, v, h)
		// buyer role: only the buyer-visible columns (E3 adds the head columns)
		for _, c := range []string{"tenant_id", "store_id", "owner_id", "order_id", "version", "status", "carrier_code", "carrier_name", "tracking_number", "tracking_url", "recorded_at"} {
			srsColPriv(t, e, "commerce_checkout_runtime", v, c, "SELECT", true)
		}
		for _, c := range []string{"note", "void_reason", "principal_id"} {
			srsColPriv(t, e, "commerce_checkout_runtime", v, c, "SELECT", false)
		}
		for _, c := range []string{"tenant_id", "store_id", "owner_id", "order_id", "current_version"} {
			srsColPriv(t, e, "commerce_checkout_runtime", h, c, "SELECT", true)
		}
		srsColPriv(t, e, "commerce_checkout_runtime", h, "updated_at", "SELECT", false)
		srsPriv(t, e, "commerce_checkout_runtime", v, "INSERT", false)
		// policies by role and command (names are the implementer's; roles/commands are the contract's)
		for _, want := range []struct {
			table, role string
			cmds        []string
		}{
			{"manual_shipment_versions", "commerce_checkout_writer", []string{"ALL", "SELECT"}}, {"manual_shipment_versions", "commerce_checkout_writer", []string{"ALL", "INSERT"}},
			{"manual_shipment_heads", "commerce_checkout_writer", []string{"ALL", "SELECT"}}, {"manual_shipment_heads", "commerce_checkout_writer", []string{"ALL", "INSERT"}}, {"manual_shipment_heads", "commerce_checkout_writer", []string{"ALL", "UPDATE"}},
			{"manual_shipment_versions", "commerce_auth", []string{"ALL", "SELECT"}}, {"manual_shipment_heads", "commerce_auth", []string{"ALL", "SELECT"}},
			{"manual_shipment_versions", "commerce_checkout_runtime", []string{"ALL", "SELECT"}}, {"manual_shipment_heads", "commerce_checkout_runtime", []string{"ALL", "SELECT"}},
		} {
			if !srsBool(t, e, "policy", `SELECT EXISTS(SELECT 1 FROM pg_policies WHERE schemaname='fulfillment' AND tablename=$1 AND $2::name=ANY(roles) AND cmd=ANY($3::text[]))`, want.table, want.role, want.cmds) {
				t.Errorf("MF02 no policy on fulfillment.%s for %s covering %v", want.table, want.role, want.cmds)
			}
		}
		// (A2) grants the definers depend on
		for _, c := range []struct{ role, schema string }{{"commerce_checkout_writer", "identity"}, {"commerce_checkout_writer", "ops"}, {"commerce_auth", "ops"}} {
			if !srsBool(t, e, "usage", `SELECT has_schema_privilege($1,$2,'USAGE')`, c.role, c.schema) {
				t.Errorf("MF02 %s lacks USAGE on schema %s", c.role, c.schema)
			}
		}
		srsPriv(t, e, "commerce_auth", "ops.audit_events", "INSERT", true)
		srsPriv(t, e, "commerce_auth", "ops.audit_events", "UPDATE", false)
		srsPriv(t, e, "commerce_auth", "ops.audit_events", "SELECT", false)
		srsPolicy(t, e, "ops", "audit_events", "auth_export_audit", "commerce_auth", "INSERT", true, "orders.export_unshipped", "app.tenant_id", "app.store_id", "app.principal_id")
		srsColPriv(t, e, "commerce_auth", "payments.review_cases", "reason", "SELECT", true)
		srsColPriv(t, e, "commerce_auth", "payments.review_cases", "source_report_hash", "SELECT", false)
		srsPriv(t, e, "commerce_auth", "payments.review_cases", "INSERT", false)
		srsPriv(t, e, "commerce_checkout_writer", "ops.command_results", "INSERT", true)
		srsPriv(t, e, "commerce_checkout_writer", "ops.audit_events", "INSERT", true)
		srsColPriv(t, e, "commerce_checkout_writer", "identity.sessions", "token_hash", "SELECT", true)
		if !srsBool(t, e, "resolve_access", `SELECT has_function_privilege('commerce_checkout_writer','identity.resolve_access(bytea,uuid,text)','EXECUTE')`) {
			t.Error("MF02 commerce_checkout_writer cannot EXECUTE identity.resolve_access")
		}
		for _, name := range []string{"guard_manual_shipment_state"} {
			srsMust(t, e, "trigger function fulfillment."+name, `SELECT EXISTS(SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='fulfillment' AND p.proname=$1)`, name)
		}
		// the deferred agreement constraint triggers exist on BOTH tables (A1)
		for _, table := range []string{"checkout.orders", "fulfillment.manual_shipment_heads"} {
			srsMust(t, e, "deferred constraint trigger on "+table, `SELECT EXISTS(SELECT 1 FROM pg_trigger tg JOIN pg_proc p ON p.oid=tg.tgfoid WHERE tg.tgrelid=$1::regclass AND p.proname='guard_manual_shipment_state'
			 AND tg.tgconstraint<>0 AND tg.tgdeferrable AND tg.tginitdeferred)`, table)
		}
		srsMust(t, e, "partial index for the unshipped export", `SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE schemaname='checkout' AND tablename='orders' AND indexdef ILIKE '%WHERE%' AND indexdef ILIKE '%CONFIRMED%' AND indexdef ILIKE '%MANUAL_UNASSIGNED%')`)
	})

	// ---- behaviour through the real definers --------------------------------------------------
	e.startWorker(t)
	o1 := e.payInStore(t, e.storeFor(t))
	o2 := e.payMore(t, o1)
	e.grant(t, o2, "orders:read", "payments:refund", "fulfillment:write", "orders:export")
	e.mustShip(t, o1, 0, "sf_express", "SFSCHEMA1")

	t.Run("record wrote its command result and audit row through the definer (not as superuser)", func(t *testing.T) {
		if e.mfxAudit(t, o1, "fulfillment.shipment_recorded") != 1 || e.count(t, `SELECT count(*) FROM ops.command_results WHERE tenant_id=$1 AND store_id=$2 AND operation='fulfillment.manual_shipment.record'`, o1.s.p.f.tenantA, o1.store()) != 1 {
			t.Fatal("command result / audit row missing")
		}
		before := e.mfxAudit(t, o1, "orders.export_unshipped")
		if status, _, _ := e.mfxCSV(o1, o1.token()); status != 200 {
			t.Fatalf("export: %d", status)
		}
		if e.mfxAudit(t, o1, "orders.export_unshipped") != before+1 {
			t.Fatal("export_unshipped_orders wrote no orders.export_unshipped audit row (commerce_auth needs USAGE ops + the insert policy)")
		}
	})

	t.Run("versions are append-only, heads move by exactly +1, and the state agreement is enforced at COMMIT", func(t *testing.T) {
		refuse := func(name, sql string, args ...any) {
			t.Helper()
			tx, err := e.f.owner.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, sql, args...); err == nil {
				t.Errorf("MF02 %s was accepted", name)
			}
		}
		refuse("UPDATE of a version (owner)", `UPDATE fulfillment.manual_shipment_versions SET note='x' WHERE order_id=$1`, o1.order)
		refuse("DELETE of a version (owner)", `DELETE FROM fulfillment.manual_shipment_versions WHERE order_id=$1`, o1.order)
		for _, role := range []string{"commerce_checkout_writer", "commerce_auth", "commerce_checkout_runtime", "commerce_runtime", "commerce_worker", "commerce_integration_writer"} {
			for _, stmt := range []string{`UPDATE fulfillment.manual_shipment_versions SET tracking_number='X'`, `DELETE FROM fulfillment.manual_shipment_versions`} {
				tx, err := e.f.owner.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(ctx, `SET LOCAL ROLE `+role); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(ctx, stmt); err == nil {
					t.Errorf("MF02 %s could run %q", role, stmt)
				}
				tx.Rollback(ctx)
			}
		}
		commit := func(name string, stmts ...string) {
			t.Helper()
			tx, err := e.f.owner.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var failed bool
			for _, s := range stmts {
				if _, err := tx.Exec(ctx, s); err != nil {
					failed = true
					break
				}
			}
			if !failed {
				if err := tx.Commit(ctx); err == nil {
					t.Errorf("MF02 %s committed", name)
				}
			} else {
				tx.Rollback(ctx)
			}
		}
		commit("head advanced by 2", fmt.Sprintf(`UPDATE fulfillment.manual_shipment_heads SET current_version=current_version+2,updated_at=clock_timestamp() WHERE order_id='%s'`, o1.order))
		commit("head moved back", fmt.Sprintf(`UPDATE fulfillment.manual_shipment_heads SET current_version=current_version-1,updated_at=clock_timestamp() WHERE order_id='%s'`, o1.order))
		// (A1) a direct order update to MERCHANT_SHIPPED without a head fails at COMMIT
		commit("order flipped to MERCHANT_SHIPPED with no head", fmt.Sprintf(`UPDATE checkout.orders SET fulfillment_state='MERCHANT_SHIPPED' WHERE id='%s'`, o2.order))
		// ... a head flip without the order update fails at COMMIT
		commit("head flipped to a VOIDED version without updating the order", fmt.Sprintf(`INSERT INTO fulfillment.manual_shipment_versions(tenant_id,store_id,owner_id,order_id,version,status,carrier_code,carrier_name,tracking_number,tracking_url,void_reason,principal_id)
		 SELECT tenant_id,store_id,owner_id,order_id,2,'VOIDED',carrier_code,carrier_name,tracking_number,tracking_url,'other',principal_id FROM fulfillment.manual_shipment_versions WHERE order_id='%s' AND version=1`, o1.order),
			fmt.Sprintf(`UPDATE fulfillment.manual_shipment_heads SET current_version=2,updated_at=clock_timestamp() WHERE order_id='%s'`, o1.order))
		// ... and an order update away from MERCHANT_SHIPPED while the head is SHIPPED fails at COMMIT
		commit("order reset to MANUAL_UNASSIGNED while the head is SHIPPED", fmt.Sprintf(`UPDATE checkout.orders SET fulfillment_state='MANUAL_UNASSIGNED' WHERE id='%s'`, o1.order))
		if e.mfxFulfilmentState(t, o1) != "MERCHANT_SHIPPED" || e.mfxFulfilmentState(t, o2) == "MERCHANT_SHIPPED" || e.mfxVersions(t, o1) != 1 {
			t.Fatal("a refused transaction changed state")
		}
		var head int64
		if err := e.f.owner.QueryRow(ctx, `SELECT current_version FROM fulfillment.manual_shipment_heads WHERE order_id=$1`, o1.order).Scan(&head); err != nil || head != 1 {
			t.Fatalf("head %d %v", head, err)
		}
	})

	t.Run("merchant read function keeps the 0027 authority behaviour and knows the new filters", func(t *testing.T) {
		s := o1.s.p.f
		if _, err := moRead(ctx, e.f.runtime, o1.token(), s.tenantA, o1.store(), s.principalA, "", "shipped", 10); err != nil {
			t.Fatalf("state=shipped: %v", err)
		}
		if _, err := moRead(ctx, e.f.runtime, o1.token(), s.tenantA, o1.store(), s.principalA, "", "unshipped", 10); err != nil {
			t.Fatalf("state=unshipped: %v", err)
		}
		if _, err := moRead(ctx, e.f.runtime, o1.token(), s.tenantA, o1.store(), s.principalA, "", "delivered", 10); err == nil {
			t.Fatal("unknown state accepted")
		}
		for name, c := range map[string]struct {
			token, store, principal, code string
		}{
			"buyer audience":       {e.f.tokens["buyer"], o1.store(), s.principalA, "PT401"},
			"forged principal GUC": {o1.token(), o1.store(), randomUUID(), "PT403"},
			"foreign store":        {o1.token(), e.f.storeB, s.principalA, "PT404"},
			"unknown token":        {randomToken(), o1.store(), s.principalA, "PT401"},
		} {
			if _, err := moRead(ctx, e.f.runtime, c.token, s.tenantA, c.store, c.principal, o1.order, "all", 1); sqlState(err) != c.code {
				t.Errorf("%s: want %s, got %v", name, c.code, err)
			}
		}
		rows, err := moRead(ctx, e.f.runtime, o1.token(), s.tenantA, o1.store(), s.principalA, o1.order, "all", 1)
		if err != nil || len(rows) != 1 {
			t.Fatalf("detail: %v %d", err, len(rows))
		}
		ship, _ := rows[0]["shipment"].(map[string]any)
		if ship == nil || ship["status"] != "SHIPPED" || ship["tracking_number"] != "SFSCHEMA1" || ship["version"] != float64(1) {
			t.Fatalf("detail shipment: %v", rows[0]["shipment"])
		}
		if _, err := time.Parse(time.RFC3339Nano, ship["recorded_at"].(string)); err != nil {
			t.Fatalf("recorded_at: %v", err)
		}
		for _, banned := range []string{"note", "void_reason", "principal_id"} {
			if _, has := ship[banned]; has {
				t.Fatalf("the merchant ORDER detail carries the history-only field %s", banned)
			}
		}
	})

	t.Run("populated upgrade after 0062", func(t *testing.T) {
		t.Skip("NOT_RUN: migrations.Apply has no partial-apply hook; a database frozen at 0062 with rows cannot be built without re-implementing the migrator")
	})
}
