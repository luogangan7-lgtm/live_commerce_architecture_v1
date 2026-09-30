// R1 ruling F2 gate (REAL_PG): the operator path `meta-admin route` (metareply.RegisterRoute ->
// integration.register_meta_binding 0066 + meta_inbox.activate_route 0028) lets a store receive signed
// Meta comments, and only the registrar authority with an integration:manage principal can use it.
// Evidence label: MOCK (REAL_PG, signed synthetic webhook; no Meta call).
package foundation_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"livecommerce/internal/command"
	"livecommerce/internal/integrations/metareply"
)

func TestMetaRouteRegistrarF2(t *testing.T) {
	m := miSetup(t)
	f := m.f
	ctx := context.Background()
	operator, outsider := randomUUID(), randomUUID()
	mustExec(t, f.owner, `INSERT INTO identity.principals(id) VALUES($1),($2)`, operator, outsider)
	mustExec(t, f.owner, `INSERT INTO identity.memberships(tenant_id,principal_id) VALUES($1,$2),($1,$3)`, f.tenantA, operator, outsider)
	mustExec(t, f.owner, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission)
		VALUES($1,$2,$3,'store:read'),($1,$2,$3,'integration:manage'),($1,$2,$4,'store:read')`, f.tenantA, f.storeA1, operator, outsider)
	asset := miAsset()
	proof := sha256.Sum256([]byte("synthetic ownership evidence " + asset))
	reg := metareply.RouteRegistration{TenantID: f.tenantA, StoreID: f.storeA1, PrincipalID: operator, AppID: miApp, Object: "page",
		AssetID: asset, Proof: hex.EncodeToString(proof[:]), ProofExpires: time.Now().Add(time.Hour)}
	bindings := func() int64 {
		return miCount(t, f.owner, `SELECT count(*) FROM integration.bindings WHERE external_asset_id=$1 AND provider='facebook'`, asset)
	}

	// Authority: a principal without integration:manage, and every non-registrar login, get nothing.
	denied := reg
	denied.PrincipalID = outsider
	if _, err := metareply.RegisterRoute(ctx, m.registrar, denied); err == nil || bindings() != 0 {
		t.Fatalf("principal without integration:manage registered a binding (err=%v)", err)
	}
	for _, p := range []struct {
		name string
		err  error
	}{{"ingress", func() error { _, err := metareply.RegisterRoute(ctx, m.ingress, reg); return err }()},
		{"curator", func() error { _, err := metareply.RegisterRoute(ctx, m.curator, reg); return err }()},
		{"runtime", func() error { _, err := metareply.RegisterRoute(ctx, f.runtime, reg); return err }()}} {
		if p.err == nil || bindings() != 0 {
			t.Fatalf("%s login activated a route (err=%v)", p.name, p.err)
		}
	}

	// Before registration a signed comment for the asset is quarantined, never routed.
	jobs := func() int64 {
		return miCount(t, f.owner, `SELECT count(*) FROM river_meta.river_job WHERE kind='meta_inbox_v1'`)
	}
	before := jobs()
	status, body := miPost(t, m, miMessage(asset, "m."+randomUUID(), "before route"))
	miStatus(t, status, body, 200)
	if jobs() != before {
		t.Fatal("unrouted asset produced a job")
	}

	// Operator registration: binding created once, route epoch 1, audited.
	got, err := metareply.RegisterRoute(ctx, m.registrar, reg)
	if err != nil || got.RouteEpoch != 1 || got.BindingVersion != 1 || bindings() != 1 {
		t.Fatalf("register route: %+v %v bindings=%d", got, err, bindings())
	}
	if n := miCount(t, f.owner, `SELECT count(*) FROM ops.audit_events WHERE tenant_id=$1 AND store_id=$2 AND principal_id=$3 AND action='meta.binding_registered'`,
		f.tenantA, f.storeA1, operator); n != 1 {
		t.Fatalf("binding audit rows = %d, want 1", n)
	}
	before = jobs()
	status, body = miPost(t, m, miMessage(asset, "m."+randomUUID(), "after route"))
	miStatus(t, status, body, 200)
	if jobs() != before+1 {
		t.Fatal("registered route did not route a signed comment to exactly one job")
	}
	if n := miCount(t, f.owner, `SELECT count(*) FROM meta_inbox.events WHERE app_id=$1 AND asset_id=$2 AND disposition='ROUTED' AND tenant_id=$3 AND store_id=$4 AND route_id=$5`,
		miApp, asset, f.tenantA, f.storeA1, got.RouteID); n != 1 {
		t.Fatalf("routed events for the store = %d, want 1", n)
	}

	// Re-run: stale epoch conflicts; the current epoch reuses the binding and bumps the epoch.
	if _, err := metareply.RegisterRoute(ctx, m.registrar, reg); !errors.Is(err, command.ErrConflict) {
		t.Fatalf("stale epoch 0 re-registration: %v, want conflict", err)
	}
	again := reg
	again.ExpectedEpoch = 1
	second, err := metareply.RegisterRoute(ctx, m.registrar, again)
	if err != nil || second.BindingID != got.BindingID || second.RouteID != got.RouteID || second.RouteEpoch != 2 || bindings() != 1 {
		t.Fatalf("re-registration: %+v %v bindings=%d", second, err, bindings())
	}

	// The asset belongs to storeA1: another store of the same tenant cannot take it.
	mustExec(t, f.owner, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission)
		VALUES($1,$2,$3,'store:read'),($1,$2,$3,'integration:manage')`, f.tenantA, f.storeA2, operator)
	other := reg
	other.StoreID = f.storeA2
	if _, err := metareply.RegisterRoute(ctx, m.registrar, other); !errors.Is(err, command.ErrConflict) {
		t.Fatalf("second store took the asset: %v", err)
	}
	if n := miCount(t, f.owner, `SELECT count(*) FROM integration.bindings WHERE external_asset_id=$1 AND store_id=$2`, asset, f.storeA2); n != 0 {
		t.Fatal("conflicting activation left a binding behind (transaction not atomic)")
	}

	// Disable: epoch CAS, then new comments quarantine again.
	if _, err := metareply.DisableRoute(ctx, m.registrar, got.RouteID, 1); !errors.Is(err, command.ErrConflict) {
		t.Fatalf("stale disable: %v", err)
	}
	epoch, err := metareply.DisableRoute(ctx, m.registrar, got.RouteID, 2)
	if err != nil || epoch != 3 {
		t.Fatalf("disable: %d %v", epoch, err)
	}
	before = jobs()
	status, body = miPost(t, m, miMessage(asset, "m."+randomUUID(), "after disable"))
	miStatus(t, status, body, 200)
	if jobs() != before {
		t.Fatal("disabled route still produced a job")
	}
}
