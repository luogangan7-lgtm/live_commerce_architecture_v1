//go:build browser

package foundation_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"livecommerce/internal/fulfillment"
	"livecommerce/internal/httpapi"
	"livecommerce/internal/identity"
	"livecommerce/internal/identityhttp"
	"livecommerce/internal/inventory"
	"livecommerce/internal/oidclogin"
	"livecommerce/internal/platform"
	"livecommerce/internal/storefront"
)

// MOU01-06: only the OIDC issuer is mocked in the happy path. Order, payment,
// expiry and projection reads are all real business APIs over an owned PG18.
func TestBrowserMerchantOrdersUIRealChain(t *testing.T) {
	if os.Getenv("LC_BROWSER_MERCHANT_ORDERS_UI_ACCEPTANCE") != "1" || os.Getenv("LC_TEST_DATABASE_ALLOWED") != "1" {
		t.Fatal("use scripts/dev/test-local.sh --browser-merchant-orders-ui")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 270*time.Second)
	defer cancel()
	q := pqSetup(t)
	moGrant(t, q.f, q.f.tenantA, q.f.storeA1, q.f.principalA)
	// One real inventory adjustment permits page two without more seed pools.
	var stockVersion int64
	if err := q.f.owner.QueryRow(ctx, `SELECT version FROM inventory.balances WHERE tenant_id=$1 AND store_id=$2 AND warehouse_id=$3 AND sku_id=$4`, q.f.tenantA, q.f.storeA1, q.stock.warehouse.ID, q.stock.skus[0].ID).Scan(&stockVersion); err != nil {
		t.Fatal(err)
	}
	_, err := t04Scoped(ctx, q.f, q.f.tokens["a"], q.f.storeA1, "inventory:write", func(tx pgx.Tx, scope platform.Scope) (inventory.Balance, error) {
		return inventory.AdjustOnHand(ctx, tx, scope, t04Key("mou-stock"), inventory.Adjustment{
			WarehouseID: q.stock.warehouse.ID, SKUID: q.stock.skus[0].ID, Delta: 40,
			ExpectedVersion: stockVersion, Reason: "MOU page-two fixture",
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	newDraft := func(quantity int64) (string, pqFixture) {
		t.Helper()
		clone := q
		clone.cap = mustIssue(t, q.cqHarness.service, q.f.storeA1)
		clone.bcHarness.prepare(t, clone.cap, []storefront.Item{{SKUID: q.stock.skus[0].ID, Quantity: quantity}})
		order, e := clone.bcHarness.begin(t04Key("mou-begin"))
		if e != nil {
			t.Fatal(e)
		}
		clone.hold = order
		clone.input.OrderID = order.OrderID
		return order.OrderID, clone
	}
	ids := map[string]string{"pending": q.hold.OrderID}
	for i := 0; i < 11; i++ {
		id, _ := newDraft(1)
		ids["draft"+strconv.Itoa(i)] = id
	}
	for _, mode := range []string{"authorized", "captured", "review", "allocation_failed"} {
		id, clone := newDraft(2)
		clone.result, err = clone.start(t04Key("mou-start"))
		if err != nil {
			t.Fatal(err)
		}
		ids[mode] = id
		report := pcFull(clone)
		switch mode {
		case "authorized":
			report["CloseStatus"] = "9"
			delete(report, "CloseAmountTWD")
		case "review":
			report["CardRefundStatus"], report["CardRefundAmountTWD"] = "2", int64(1)
		}
		hash := pcRecord(t, clone, report)
		if mode == "allocation_failed" {
			// Labeled controlled fault: same fixture as MOR payment capture gate.
			tx, e := q.f.owner.Begin(ctx)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true),set_config('app.store_id',$2,true),set_config('app.principal_id','',true),set_config('app.buyer_id',$3,true),set_config('app.buyer_session_id',$4,true)`, q.f.tenantA, q.f.storeA1, clone.cap.Scope.OwnerID, clone.cap.Scope.SessionID); e != nil {
				_ = tx.Rollback(ctx)
				t.Fatal(e)
			}
			if _, e = tx.Exec(ctx, `INSERT INTO inventory.ledger(tenant_id,store_id,warehouse_id,sku_id,kind,delta_reserved,operation,command_key,reservation_id,principal_id,checkout_id,buyer_owner_id,buyer_session_id,actor_kind)
				SELECT l.tenant_id,l.store_id,l.warehouse_id,l.sku_id,'RELEASE',-l.quantity,'test.mou.release',$1,r.id,NULL,r.id,r.buyer_owner_id,r.buyer_session_id,'SYSTEM_EXPIRY'
				FROM inventory.reservations r JOIN inventory.reservation_lines l ON (l.tenant_id,l.store_id,l.reservation_id)=(r.tenant_id,r.store_id,r.id) WHERE r.id=$2`, clone.result.AttemptID, id); e != nil {
				_ = tx.Rollback(ctx)
				t.Fatal(e)
			}
			if _, e = tx.Exec(ctx, `UPDATE inventory.reservations SET state='RELEASED' WHERE id=$1`, id); e != nil {
				_ = tx.Rollback(ctx)
				t.Fatal(e)
			}
			if _, e = tx.Exec(ctx, `UPDATE checkout.orders SET commercial_state='CANCELLED',fulfillment_state='CANCELLED' WHERE id=$1`, id); e != nil {
				_ = tx.Rollback(ctx)
				t.Fatal(e)
			}
			if e = tx.Commit(ctx); e != nil {
				t.Fatal(e)
			}
		}
		if err = pcApply(clone.worker, clone.result.AttemptID, hash); err != nil {
			t.Fatal(err)
		}
	}
	var expiredFixture pqFixture
	ids["expired"], expiredFixture = newDraft(1)
	// Expire the actual checkout hold through the ordinary expiry worker.
	bcDue(t, expiredFixture.bcHarness, expiredFixture.hold)
	if disposition := bcExpire(t, expiredFixture.bcHarness, expiredFixture.hold, 1); disposition != "EXPIRED" {
		t.Fatalf("expiry disposition=%s", disposition)
	}
	// A real attested pickup keeps its leading-zero code in the checkout snapshot.
	pickupFixture := q
	pickupFixture.cap = mustIssue(t, q.cqHarness.service, q.f.storeA1)
	pickupFixture.bcHarness.prepare(t, pickupFixture.cap, []storefront.Item{{SKUID: q.stock.skus[0].ID, Quantity: 1}})
	cvsService := q.delivery
	cvsService.ExpectedVersion = 1
	cvsService.DeliveryKind = "cvs_familymart"
	if _, err = dsSet(q.cqHarness, t04Key("mou-cvs-service"), cvsService); err != nil {
		t.Fatal(err)
	}
	pickup, err := bdAttest(pickupFixture.cqHarness, t04Key("mou-pickup"), fulfillment.PickupInput{Kind: "cvs_familymart", Namespace: "fixture." + t04Tag(), Code: "017888", Name: "Synthetic convenience store", Address: "Synthetic convenience address", EvidenceRef: "synthetic only", TTLSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	pickupFixture.destination, err = bdSet(pickupFixture.cqHarness, t04Key("mou-pickup-destination"), storefront.DestinationInput{ExpectedVersion: 1, CartVersion: pickupFixture.bcHarness.input.CartVersion, Kind: "cvs_familymart", Country: "TW", RecipientName: "Synthetic Recipient", Phone: "+886900000002", PickupID: pickup.ID})
	if err != nil {
		t.Fatal(err)
	}
	pickupFixture.bcHarness.input.DestinationID = pickupFixture.destination.ID
	pickupFixture.bcHarness.input.ServiceVersion = 2
	pickupOrder, err := pickupFixture.bcHarness.begin(t04Key("mou-pickup-order"))
	if err != nil {
		t.Fatal(err)
	}
	ids["pickup"] = pickupOrder.OrderID
	// Change mutable catalog facts after all checkout snapshots were frozen.
	mustExec(t, q.f.owner, `UPDATE catalog.products SET name='Renamed after checkout' WHERE id=$1`, q.stock.product.ID)
	mustExec(t, q.f.owner, `UPDATE catalog.skus SET code='NEW-CODE',price_minor=99999 WHERE id=$1`, q.stock.skus[0].ID)
	foreignStore, foreignOrder := moBeginInOtherStore(t, q.f, true)
	for _, permission := range []string{"store:read", "orders:read"} {
		mustExec(t, q.f.owner, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission) VALUES($1,$2,$3,$4)`, q.f.tenantA, foreignStore, q.f.principalA, permission)
	}
	secondPrincipal, secondToken := randomUUID(), randomToken()
	noOrdersPrincipal, noOrdersToken := randomUUID(), randomToken()
	expiredToken, revokedToken := randomToken(), randomToken()
	for _, principal := range []string{secondPrincipal, noOrdersPrincipal} {
		mustExec(t, q.f.owner, `INSERT INTO identity.principals(id) VALUES($1)`, principal)
		mustExec(t, q.f.owner, `INSERT INTO identity.memberships(tenant_id,principal_id) VALUES($1,$2)`, q.f.tenantA, principal)
	}
	for _, permission := range []string{"store:read", "orders:read"} {
		mustExec(t, q.f.owner, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission) VALUES($1,$2,$3,$4)`, q.f.tenantA, foreignStore, secondPrincipal, permission)
	}
	for _, principal := range []string{secondPrincipal, noOrdersPrincipal} {
		mustExec(t, q.f.owner, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission) VALUES($1,$2,$3,'store:read')`, q.f.tenantA, q.f.storeA1, principal)
	}
	tx, err := q.f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, item := range []struct {
		token, principal string
		expiry           time.Time
		revoked          *time.Time
	}{
		{secondToken, secondPrincipal, now.Add(time.Hour), nil},
		{noOrdersToken, noOrdersPrincipal, now.Add(time.Hour), nil},
		{expiredToken, q.f.principalA, now.Add(-time.Minute), nil},
		{revokedToken, q.f.principalA, now.Add(time.Hour), &now},
	} {
		if err = insertSession(ctx, tx, item.token, item.principal, "merchant", item.expiry, item.revoked); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	_, port, _ := net.SplitHostPort(address)
	origin := "http://" + address
	idp := newBrowserIDP(t, origin+"/api/auth/callback")
	mustExec(t, q.f.owner, `INSERT INTO identity.external_identities(issuer,subject,principal_id) VALUES($1,'browser-subject',$2)`, idp.server.URL, q.f.principalA)
	_, _, authority := identityFixture(t)
	provider, err := oidclogin.New(ctx, oidclogin.Config{Issuer: idp.server.URL, ClientID: browserClientID, RedirectURL: idp.redirect, AllowLoopbackForTests: true})
	if err != nil {
		t.Fatal(err)
	}
	service, err := identity.New(authority, observedBrowserProvider{Provider: provider, t: t}, identity.Policy{ProviderKey: "browser-orders-ui-v1", SessionTTL: time.Hour, OnboardingEnabled: true, Currencies: []string{"TWD", "USD"}})
	if err != nil {
		t.Fatal(err)
	}
	bffKey := randomToken()
	private, err := identityhttp.NewHandler(service, bffKey)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/identity/", private)
	mux.Handle("/", httpapi.NewHandler(q.f.runtime, httpapi.Options{SessionStoreList: true}))
	var orderCalls, detailCalls, stripped atomic.Int64
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__test/order-observation" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]int64{"orders": orderCalls.Load(), "details": detailCalls.Load()})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/admin/stores/") && strings.Contains(r.URL.Path, "/orders") {
			orderCalls.Add(1)
			if strings.Contains(strings.TrimPrefix(r.URL.Path, "/v1/admin/stores/"), "/orders/") {
				detailCalls.Add(1)
			}
			if r.Header.Get("Cookie") != "" || r.Header.Get("X-Tenant-ID") != "" || r.Header.Get("X-Forwarded-Host") != "" {
				stripped.Add(1)
			}
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(api.Close)
	tables := []string{"checkout.orders", "checkout.payment_attempts", "payments.facts", "inventory.ledger", "checkout.command_results", "checkout.events", "fulfillment.payment_work_items", "river.river_job", "river_payment.river_job", "river_expiry.river_job"}
	before := map[string]int{}
	for _, table := range tables {
		before[table] = countRows(t, q.f.owner, "SELECT count(*) FROM "+table)
	}
	var originalHash string
	if err = q.f.owner.QueryRow(ctx, `SELECT md5(o::text) FROM checkout.orders o WHERE id=$1`, q.hold.OrderID).Scan(&originalHash); err != nil {
		t.Fatal(err)
	}
	// Evidence stays under the gitignored repo output/playwright like the other
	// browser chains; a workstation-only absolute path fails on Linux/CI hosts.
	evidence := filepath.Join(root, "output/playwright/merchant-orders-c-browser", time.Now().UTC().Format("20060102T150405.000000000"))
	if err = os.MkdirAll(evidence, 0700); err != nil {
		t.Fatal(err)
	}
	nextLog := browserLog(t, filepath.Join(evidence, "next.log"))
	next := exec.CommandContext(ctx, "node", filepath.Join(root, "apps/admin/.next/standalone/apps/admin/server.js"))
	next.Dir = root
	next.Env = browserEnvironment(map[string]string{"HOSTNAME": "127.0.0.1", "PORT": port, "NODE_ENV": "production", "COMMERCE_IDENTITY_ENABLED": "1", "COMMERCE_IDENTITY_ALLOW_LOOPBACK_TESTS": "1", "COMMERCE_PUBLIC_ORIGIN": origin, "COMMERCE_API_ORIGIN": api.URL, "COMMERCE_OIDC_ISSUER": idp.server.URL, "COMMERCE_BFF_KEY": bffKey, "COMMERCE_ONBOARDING_ENABLED": "1", "COMMERCE_ONBOARDING_CURRENCIES": "TWD,USD"})
	next.Stdout, next.Stderr = nextLog, nextLog
	if err = next.Start(); err != nil {
		t.Fatal(err)
	}
	nextDone := make(chan error, 1)
	go func() { nextDone <- next.Wait() }()
	t.Cleanup(func() {
		_ = next.Process.Kill()
		select {
		case <-nextDone:
		case <-time.After(5 * time.Second):
			t.Error("owned Next process did not stop")
		}
	})
	client := &http.Client{Timeout: time.Second}
	ready := false
	for attempt := 0; attempt < 100; attempt++ {
		response, e := client.Get(origin + "/api/stores")
		if e == nil {
			_ = response.Body.Close()
			ready = response.StatusCode == http.StatusUnauthorized
		}
		if ready {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("Next readiness deadline")
		case <-time.After(100 * time.Millisecond):
		}
	}
	if !ready {
		t.Fatalf("Next readiness failed; evidence=%s", evidence)
	}
	fixtures, _ := json.Marshal(ids)
	playwrightLog := browserLog(t, filepath.Join(evidence, "playwright.log"))
	browser := exec.CommandContext(ctx, "pnpm", "exec", "playwright", "test", "tests/admin/orders-ui.spec.ts", "--reporter=list", "--output="+filepath.Join(evidence, "results"))
	browser.Dir = root
	browser.Env = browserEnvironment(map[string]string{"LC_BROWSER_SUITE": "merchant-orders-ui", "LC_BROWSER_PUBLIC_ORIGIN": origin, "LC_BROWSER_API_ORIGIN": api.URL, "LC_BROWSER_ORDER_STORE": q.f.storeA1, "LC_BROWSER_ORDER_IDS": string(fixtures), "LC_BROWSER_FROZEN_SKU_CODE": q.stock.skus[0].Code, "LC_BROWSER_FOREIGN_STORE": foreignStore, "LC_BROWSER_FOREIGN_ORDER_ID": foreignOrder, "LC_BROWSER_UNLISTED_STORE": q.f.storeB, "LC_BROWSER_SECOND_TOKEN": secondToken, "LC_BROWSER_NO_ORDERS_TOKEN": noOrdersToken, "LC_BROWSER_EXPIRED_TOKEN": expiredToken, "LC_BROWSER_REVOKED_TOKEN": revokedToken, "LC_BROWSER_EVIDENCE": evidence})
	browser.Stdout, browser.Stderr = playwrightLog, playwrightLog
	if err = browser.Run(); err != nil {
		t.Fatalf("MOU browser gate failed: %v; evidence=%s", err, evidence)
	}
	if stripped.Load() != 0 || orderCalls.Load() < 10 {
		t.Fatalf("BFF bridge proof stripped=%d calls=%d evidence=%s", stripped.Load(), orderCalls.Load(), evidence)
	}
	for _, table := range tables {
		if after := countRows(t, q.f.owner, "SELECT count(*) FROM "+table); after != before[table] {
			t.Fatalf("read mutated %s: %d -> %d", table, before[table], after)
		}
	}
	var afterHash string
	if err = q.f.owner.QueryRow(ctx, `SELECT md5(o::text) FROM checkout.orders o WHERE id=$1`, q.hold.OrderID).Scan(&afterHash); err != nil || afterHash != originalHash {
		t.Fatalf("order changed after reads: match=%t err=%v", afterHash == originalHash, err)
	}
	var native struct {
		Events []struct {
			State   string `json:"state"`
			Trusted bool   `json:"trusted"`
		} `json:"events"`
		BeforeHide  int  `json:"beforeHide"`
		WhileHidden int  `json:"whileHidden"`
		AfterReturn int  `json:"afterReturn"`
		PiiCleared  bool `json:"piiCleared"`
		Revalidated bool `json:"revalidated"`
	}
	nativeProof, err := os.ReadFile(filepath.Join(evidence, "native-visibility.json"))
	if err != nil {
		t.Fatalf("MOU03 native visibility proof missing: %v; evidence=%s", err, evidence)
	}
	if err := json.Unmarshal(nativeProof, &native); err != nil {
		t.Fatalf("MOU03 native visibility proof invalid: %v; evidence=%s", err, evidence)
	}
	if len(native.Events) != 2 || native.Events[0].State != "hidden" || !native.Events[0].Trusted || native.Events[1].State != "visible" || !native.Events[1].Trusted || native.BeforeHide != native.WhileHidden || native.AfterReturn <= native.WhileHidden || !native.PiiCleared || !native.Revalidated {
		t.Fatalf("MOU03 native visibility proof violated: events=%v before=%d hidden=%d return=%d piiCleared=%t revalidated=%t; evidence=%s", native.Events, native.BeforeHide, native.WhileHidden, native.AfterReturn, native.PiiCleared, native.Revalidated, evidence)
	}
	t.Logf("MOU real-chain browser cases, trusted native visibility, and PG read-only facts checked; evidence=%s", evidence)
}
