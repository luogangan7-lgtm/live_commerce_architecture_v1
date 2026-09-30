package foundation_test

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"livecommerce/internal/buyer"
	"livecommerce/internal/checkout"
	"livecommerce/internal/command"
	"livecommerce/internal/fulfillment"
	"livecommerce/internal/inventory"
	"livecommerce/internal/platform"
	"livecommerce/internal/storefront"
)

// This is the actual Begin -> PG ledger -> River expiry boundary, not a provider
// or public browser checkout. Every identity, address and shipment is synthetic.
type bcHarness struct {
	cqHarness
	pool, worker *pgxpool.Pool
	service      *checkout.Service
	input        checkout.Input
	quote        storefront.Quote
	destination  storefront.Destination
	delivery     fulfillment.ServiceInput
	allocation   fulfillment.AllocationInput
	poolURL      string
}

func bcRole(t *testing.T, f *testFixture, authority string) string {
	t.Helper()
	role := "checkout_test_" + hex.EncodeToString(randomBytes(6))
	password := hex.EncodeToString(randomBytes(24))
	mustExec(t, f.owner, fmt.Sprintf(`CREATE ROLE %s LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION IN ROLE %s PASSWORD '%s'`, pgx.Identifier{role}.Sanitize(), pgx.Identifier{authority}.Sanitize(), password))
	t.Cleanup(func() { mustExec(t, f.owner, `DROP ROLE `+pgx.Identifier{role}.Sanitize()) })
	return roleURL(t, f.databaseURL, role, password)
}

func bcService(t *testing.T, pool *pgxpool.Pool) *checkout.Service {
	return bcServiceIn(t, pool, "river_expiry")
}

func bcServiceIn(t *testing.T, pool *pgxpool.Pool, schema string) *checkout.Service {
	t.Helper()
	jobs, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	service, err := checkout.New(context.Background(), pool, jobs)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func bcSetup(t *testing.T) bcHarness {
	t.Helper()
	h, delivery, allocation := daSetup(t)
	if _, err := daSet(h, t04Key("bc-allocation"), allocation); err != nil {
		t.Fatal(err)
	}
	url := bcRole(t, h.f, "commerce_checkout_runtime")
	pool, err := platform.OpenCheckoutPool(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	worker, err := platform.OpenWorkerPool(context.Background(), bcRole(t, h.f, "commerce_worker"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(worker.Close)
	b := bcHarness{cqHarness: h, pool: pool, worker: worker, service: bcService(t, pool), delivery: delivery, allocation: allocation, poolURL: url}
	b.prepare(t, h.cap, []storefront.Item{{SKUID: h.stock.skus[0].ID, Quantity: 2}})
	return b
}

func (b *bcHarness) prepare(t *testing.T, cap buyer.Capability, items []storefront.Item) {
	t.Helper()
	b.cap = cap
	c, err := b.cart(t04Key("bc-cart"), storefront.CartInput{Items: items})
	if err != nil {
		t.Fatal(err)
	}
	b.destination, err = bdSet(b.cqHarness, t04Key("bc-destination"), bdHome(c))
	if err != nil {
		t.Fatal(err)
	}
	b.quote, err = cqBuyer(b.a.runtime, cap, func(ctx context.Context, tx pgx.Tx, s buyer.Scope) (storefront.Quote, error) {
		return storefront.CreateQuote(ctx, tx, s, t04Key("bc-quote"), storefront.QuoteInput{CartVersion: c.Version, MarketID: b.market.ID, Country: "TW", Method: "delivery:" + b.delivery.Code})
	})
	if err != nil {
		t.Fatal(err)
	}
	b.input = checkout.Input{QuoteID: b.quote.ID, DestinationID: b.destination.ID, CartVersion: c.Version, ServiceVersion: 1, AllocationVersion: 1}
}

func (b bcHarness) begin(key string) (checkout.Result, error) {
	return b.service.Begin(context.Background(), b.cap.Token, b.f.storeA1, key, b.input)
}

func (b bcHarness) facts(t *testing.T) [6]int {
	t.Helper()
	var got [6]int
	for i, table := range []string{"checkout.orders", "checkout.command_results", "checkout.events"} {
		got[i] = countRows(t, b.f.owner, `SELECT count(*) FROM `+table+` WHERE owner_id=$1`, b.cap.Scope.OwnerID)
	}
	got[3] = countRows(t, b.f.owner, `SELECT count(*) FROM inventory.reservations WHERE buyer_owner_id=$1`, b.cap.Scope.OwnerID)
	got[4] = countRows(t, b.f.owner, `SELECT count(*) FROM inventory.ledger WHERE buyer_owner_id=$1`, b.cap.Scope.OwnerID)
	// Serial foundation suite: also catches an orphan job with no order/owner FK.
	got[5] = countRows(t, b.f.owner, `SELECT count(*) FROM river_expiry.river_job WHERE kind='checkout_expiry_v1'`)
	return got
}

func TestBuyerCheckoutCreatesAtomicHoldAndOwnerReplay(t *testing.T) {
	b := bcSetup(t)
	key := t04Key("bc-begin")
	before := b.facts(t)
	result, err := b.begin(key)
	if err != nil {
		t.Fatal(err)
	}
	if result.OrderID == "" || result.OrderID != result.ReservationID || result.Generation != 1 || result.JobID < 1 {
		t.Fatalf("bad receipt: %+v", result)
	}
	after := b.facts(t)
	for i, n := range after {
		if n-before[i] != 1 {
			t.Fatalf("atomic facts[%d]=%d -> %d", i, before[i], n)
		}
	}
	order, err := b.service.Get(context.Background(), b.cap.Token, b.f.storeA1, result.OrderID)
	if err != nil || order.CommercialState != "DRAFT" || order.FulfillmentState != "MANUAL_UNASSIGNED" || !reflect.DeepEqual(order.Snapshot.Quote, b.quote) || !reflect.DeepEqual(order.Snapshot.Destination, b.destination) {
		t.Fatalf("readback contract: %v state=%s", err, order.CommercialState)
	}
	var reserved, qty int64
	var state, actor string
	var principal *string
	if err = b.f.owner.QueryRow(context.Background(), `SELECT b.reserved,r.state,l.delta_reserved,l.actor_kind,l.principal_id::text FROM inventory.reservations r JOIN inventory.ledger l ON l.tenant_id=r.tenant_id AND l.store_id=r.store_id AND l.reservation_id=r.id JOIN inventory.balances b ON b.tenant_id=l.tenant_id AND b.store_id=l.store_id AND b.warehouse_id=l.warehouse_id AND b.sku_id=l.sku_id WHERE r.id=$1 AND l.kind='RESERVE'`, result.OrderID).Scan(&reserved, &state, &qty, &actor, &principal); err != nil {
		t.Fatal(err)
	}
	if reserved != 2 || qty != 2 || state != "HELD" || actor != "BUYER" || principal != nil {
		t.Fatalf("hold conservation/actor: %d/%d/%s/%s", reserved, qty, state, actor)
	}
	var raw, receipt []byte
	var kind, jobState, jobQueue string
	if err = b.f.owner.QueryRow(context.Background(), `SELECT kind,state,queue,args FROM river_expiry.river_job WHERE id=$1`, result.JobID).Scan(&kind, &jobState, &jobQueue, &raw); err != nil {
		t.Fatal(err)
	}
	var args map[string]any
	if err = json.Unmarshal(raw, &args); err != nil || len(args) != 3 || args["order_id"] != result.OrderID || args["generation"] != float64(1) || args["version"] != float64(1) || kind != "checkout_expiry_v1" || jobState != "scheduled" || jobQueue != "checkout_expiry_v1" {
		t.Fatalf("private delayed job: %s %s %s", kind, jobState, raw)
	}
	if err = b.f.owner.QueryRow(context.Background(), `SELECT response FROM checkout.command_results WHERE owner_id=$1 AND idempotency_key=$2`, b.cap.Scope.OwnerID, key).Scan(&receipt); err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{b.cap.Token, b.destination.RecipientName, b.destination.Phone, b.destination.HomeAddress.Line1} {
		if strings.Contains(string(raw)+string(receipt), private) {
			t.Fatal("PII/token in job or receipt")
		}
	}
	// Same owner, new capability: private checkout receipt is NOT session-scoped.
	second := b.cap
	second.Token = base64.RawURLEncoding.EncodeToString(tokenHash(randomUUID()))
	second.Scope.SessionID = randomUUID()
	mustExec(t, b.f.owner, `INSERT INTO buyer.capability_sessions(tenant_id,store_id,owner_id,id,token_hash,expires_at) VALUES($1,$2,$3,$4,$5,clock_timestamp()+interval '1 hour')`, second.Scope.TenantID, second.Scope.StoreID, second.Scope.OwnerID, second.Scope.SessionID, tokenHash(second.Token))
	// Changed catalog does not erase a permanent historical replay.
	mustExec(t, b.f.owner, `UPDATE catalog.skus SET price_minor=price_minor+1,version=version+1 WHERE id=$1`, b.stock.skus[0].ID)
	replay, e := b.service.Begin(context.Background(), second.Token, b.f.storeA1, key, b.input)
	if e != nil || !reflect.DeepEqual(replay, result) || b.facts(t) != after {
		t.Fatalf("owner historical replay: %v", e)
	}
	changed := b.input
	changed.AllocationVersion++
	if _, e = b.service.Begin(context.Background(), second.Token, b.f.storeA1, key, changed); !errors.Is(e, command.ErrConflict) {
		t.Fatalf("changed replay: %v", e)
	}
	foreign := mustIssue(t, b.cqHarness.service, b.f.storeA1)
	if _, e = b.service.Get(context.Background(), foreign.Token, b.f.storeA1, result.OrderID); !errors.Is(e, command.ErrNotFound) {
		t.Fatalf("foreign read: %v", e)
	}
	mustExec(t, b.f.owner, `UPDATE buyer.capability_sessions SET revoked_at=clock_timestamp() WHERE id=$1`, second.Scope.SessionID)
	if _, e = b.service.Begin(context.Background(), second.Token, b.f.storeA1, key, b.input); !errors.Is(e, buyer.ErrUnauthorized) {
		t.Fatalf("revoked replay: %v", e)
	}
}

func TestBuyerCheckoutConcurrentReplayAndCartDedup(t *testing.T) {
	b := bcSetup(t)
	key := t04Key("bc-concurrent")
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]checkout.Result, 4)
	errs := make([]error, 4)
	for i := range results {
		wg.Add(1)
		go func(i int) { defer wg.Done(); <-start; results[i], errs[i] = b.begin(key) }(i)
	}
	close(start)
	wg.Wait()
	for i, e := range errs {
		if e != nil || !reflect.DeepEqual(results[0], results[i]) {
			t.Fatalf("concurrent replay[%d]: %v", i, e)
		}
	}
	before := b.facts(t)
	if _, e := b.begin(t04Key("bc-newkey")); !errors.Is(e, command.ErrConflict) {
		t.Fatalf("same cart double hold: %v", e)
	}
	if b.facts(t) != before {
		t.Fatal("different key left partial hold")
	}
}

func TestBuyerCheckoutTwoBuyersLastStockAndReverseSKUs(t *testing.T) {
	for _, multi := range []bool{false, true} {
		t.Run(fmt.Sprint(multi), func(t *testing.T) {
			b := bcSetup(t)
			items := []storefront.Item{{SKUID: b.stock.skus[0].ID, Quantity: 1}}
			if multi {
				items = append(items, storefront.Item{SKUID: b.stock.skus[1].ID, Quantity: 1})
			}
			// Owner-only stock setup; no production inventory involved.
			for _, item := range items {
				mustExec(t, b.f.owner, `UPDATE inventory.balances SET on_hand=1 WHERE warehouse_id=$1 AND sku_id=$2`, b.stock.warehouse.ID, item.SKUID)
			}
			left := b
			left.prepare(t, mustIssue(t, b.cqHarness.service, b.f.storeA1), items)
			if multi {
				items[0], items[1] = items[1], items[0]
			}
			right := b
			right.prepare(t, mustIssue(t, b.cqHarness.service, b.f.storeA1), items)
			start := make(chan struct{})
			done := make(chan error, 2)
			// Same public idempotency key for different owners must not collide.
			for _, candidate := range []bcHarness{left, right} {
				go func(h bcHarness) { <-start; _, e := h.begin("same-public-key"); done <- e }(candidate)
			}
			close(start)
			wins, short := 0, 0
			for i := 0; i < 2; i++ {
				e := waitError(t, done)
				if e == nil {
					wins++
				} else if errors.Is(e, command.ErrInsufficient) {
					short++
				} else {
					t.Fatalf("unexpected contention failure: %v", e)
				}
			}
			if wins != 1 || short != 1 {
				t.Fatalf("wins=%d insufficient=%d", wins, short)
			}
			for _, item := range items {
				var reserved int64
				if e := b.f.owner.QueryRow(context.Background(), `SELECT reserved FROM inventory.balances WHERE warehouse_id=$1 AND sku_id=$2`, b.stock.warehouse.ID, item.SKUID).Scan(&reserved); e != nil || reserved != 1 {
					t.Fatalf("oversell/partial multi hold: %d %v", reserved, e)
				}
			}
		})
	}
}

func TestBuyerCheckoutStaleInputsLeaveNoHold(t *testing.T) {
	for _, target := range []string{"cart", "price", "destination", "service", "hidden", "allocation", "warehouse", "policy"} {
		t.Run(target, func(t *testing.T) {
			b := bcSetup(t)
			before := b.facts(t)
			switch target {
			case "cart":
				mustExec(t, b.f.owner, `UPDATE storefront.carts SET version=version+1 WHERE id=$1`, b.quote.CartID)
			case "price":
				mustExec(t, b.f.owner, `UPDATE catalog.skus SET price_minor=price_minor+1,version=version+1 WHERE id=$1`, b.stock.skus[0].ID)
			case "destination":
				next := bdHome(storefront.Cart{Version: b.input.CartVersion})
				next.ExpectedVersion = 1
				next.HomeAddress.Line1 = "Other synthetic address"
				if _, e := bdSet(b.cqHarness, t04Key("bc-new-destination"), next); e != nil {
					t.Fatal(e)
				}
			case "service", "hidden":
				in := b.delivery
				in.ExpectedVersion = 1
				if target == "hidden" {
					in.Visible = false
				} else {
					in.Enabled = false
				}
				if _, e := dsSet(b.cqHarness, t04Key("bc-disable"), in); e != nil {
					t.Fatal(e)
				}
			case "allocation":
				in := b.allocation
				in.ExpectedVersion = 1
				in.WarehouseIDs = nil
				if _, e := daSet(b.cqHarness, t04Key("bc-clear"), in); e != nil {
					t.Fatal(e)
				}
			case "warehouse":
				mustExec(t, b.f.owner, `UPDATE inventory.warehouses SET active=false WHERE id=$1`, b.stock.warehouse.ID)
			case "policy":
				dsPolicy(t, b.cqHarness, b.delivery, 1, 60, true)
			}
			if _, e := b.begin(t04Key("bc-stale")); e == nil {
				t.Fatal("stale input accepted")
			}
			if b.facts(t) != before {
				t.Fatal("stale input left partial facts")
			}
		})
	}
}

func TestBuyerCheckoutAuthorityAndLegacyMerchantFence(t *testing.T) {
	b := bcSetup(t)
	result, e := b.begin(t04Key("bc-authority"))
	if e != nil {
		t.Fatal(e)
	}
	for name, pool := range map[string]*pgxpool.Pool{"buyer": b.a.runtime, "issuer": b.a.issuer, "merchant": b.f.runtime, "identity": b.a.identity, "worker": b.worker} {
		t.Run(name, func(t *testing.T) {
			for _, sql := range []string{`SELECT checkout.begin_hold(NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL)`, `INSERT INTO checkout.command_results DEFAULT VALUES`, `UPDATE checkout.orders SET commercial_state='CANCELLED'`} {
				_, e := pool.Exec(context.Background(), sql)
				daSQLState(t, e, "42501")
			}
		})
	}
	before := b.facts(t)
	_, e = t04Scoped(context.Background(), b.f, b.f.tokens["a"], b.f.storeA1, "inventory:reserve", func(tx pgx.Tx, s platform.Scope) (int, error) {
		_, err := inventory.ReleaseReservation(context.Background(), tx, s, t04Key("bc-legacy-release"), result.ReservationID, false)
		return 0, err
	})
	if e == nil {
		t.Fatal("merchant ReleaseReservation altered checkout")
	}
	for _, sql := range []string{
		`UPDATE inventory.reservations SET state='RELEASED' WHERE id=$1`,
		`INSERT INTO inventory.reservation_lines(tenant_id,store_id,reservation_id,warehouse_id,sku_id,quantity) SELECT tenant_id,store_id,$1,warehouse_id,sku_id,1 FROM inventory.balances WHERE warehouse_id=$2 AND sku_id=$3`,
		`INSERT INTO inventory.ledger(tenant_id,store_id,warehouse_id,sku_id,kind,delta_on_hand,delta_reserved,operation,command_key,reservation_id,reason,principal_id) SELECT tenant_id,store_id,warehouse_id,sku_id,'RELEASE',0,-1,'legacy.bypass','legacy-key',$1,'test',$4 FROM inventory.balances WHERE warehouse_id=$2 AND sku_id=$3`,
	} {
		_, e = t04Scoped(context.Background(), b.f, b.f.tokens["a"], b.f.storeA1, "inventory:reserve", func(tx pgx.Tx, s platform.Scope) (int, error) {
			args := []any{result.ReservationID}
			if strings.Contains(sql, "$2") {
				sku := b.stock.skus[0].ID
				if strings.Contains(sql, "INSERT INTO inventory.reservation_lines") {
					sku = b.stock.skus[1].ID
				}
				args = append(args, b.stock.warehouse.ID, sku)
			}
			if strings.Contains(sql, "$4") {
				args = append(args, s.PrincipalID)
			}
			tag, err := tx.Exec(context.Background(), sql, args...)
			if err == nil && tag.RowsAffected() > 0 {
				return 0, fmt.Errorf("legacy mutation succeeded")
			}
			return 0, err
		})
		if e != nil && e.Error() == "legacy mutation succeeded" {
			t.Fatal(e)
		}
		if strings.HasPrefix(sql, "INSERT") {
			daSQLState(t, e, "42501")
		}
	}
	if b.facts(t) != before {
		t.Fatal("legacy path changed facts")
	}
	var state string
	if e = b.f.owner.QueryRow(context.Background(), `SELECT state FROM inventory.reservations WHERE id=$1`, result.OrderID).Scan(&state); e != nil || state != "HELD" {
		t.Fatalf("legacy state=%s %v", state, e)
	}
	// Direct checkout role may lock but not mutate buyer-controlled inputs.
	_ = buyer.WithScope(context.Background(), b.pool, b.cap.Token, b.f.storeA1, func(ctx context.Context, tx pgx.Tx, s buyer.Scope) error {
		_, e := tx.Exec(ctx, `UPDATE storefront.carts SET version=version+1 WHERE id=$1`, b.quote.CartID)
		return e
	})
	var version int64
	if e = b.f.owner.QueryRow(context.Background(), `SELECT version FROM storefront.carts WHERE id=$1`, b.quote.CartID).Scan(&version); e != nil || version != b.input.CartVersion {
		t.Fatalf("checkout directly updated cart: %d %v", version, e)
	}
}

func TestBuyerCheckoutPoolAuthorityMatrix(t *testing.T) {
	b := bcSetup(t)
	openers := map[string]func(context.Context, string) (*pgxpool.Pool, error){
		"commerce_runtime": platform.OpenPool, "commerce_identity": platform.OpenIdentityPool,
		"commerce_buyer_runtime": platform.OpenBuyerPool, "commerce_buyer_issuer": platform.OpenBuyerIssuerPool,
		"commerce_worker": platform.OpenWorkerPool, "commerce_checkout_runtime": platform.OpenCheckoutPool,
	}
	for authority, open := range openers {
		t.Run(authority, func(t *testing.T) {
			dsn := bcRole(t, b.f, authority)
			pool, e := open(context.Background(), dsn)
			if e != nil {
				t.Fatal(e)
			}
			pool.Close()
			if authority != "commerce_checkout_runtime" {
				pool, e = platform.OpenCheckoutPool(context.Background(), dsn)
				if e == nil {
					pool.Close()
					t.Fatal("wrong authority accepted")
				}
				config, e := pgxpool.ParseConfig(dsn)
				if e != nil {
					t.Fatal(e)
				}
				mustExec(t, b.f.owner, `GRANT commerce_checkout_runtime TO `+pgx.Identifier{config.ConnConfig.User}.Sanitize())
				// Both the old authority gate and the new one must reject the mix.
				for _, gate := range []func(context.Context, string) (*pgxpool.Pool, error){open, platform.OpenCheckoutPool} {
					pool, e = gate(context.Background(), dsn)
					if e == nil {
						pool.Close()
						t.Fatal("mixed authority accepted")
					}
				}
			}
		})
	}
	for name, pool := range map[string]*pgxpool.Pool{"owner": b.f.owner, "buyer": b.a.runtime, "merchant": b.f.runtime, "worker": b.worker} {
		t.Run("constructor-"+name, func(t *testing.T) {
			jobs, e := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: "river_expiry"})
			if e != nil {
				t.Fatal(e)
			}
			if _, e = checkout.New(context.Background(), pool, jobs); e == nil {
				t.Fatal("unsafe existing pool admitted")
			}
		})
	}
}

func TestBuyerCheckoutAtomicFaultRollback(t *testing.T) {
	for _, table := range []string{"checkout.orders", "inventory.reservations", "inventory.reservation_lines", "inventory.ledger", "checkout.events", "checkout.command_results", "river_expiry.river_job"} {
		t.Run(table, func(t *testing.T) {
			b := bcSetup(t)
			before := b.facts(t)
			name := "bc_fault_" + t04Tag()
			// Isolated fixture only. Scope the trigger to this buyer or its job kind;
			// transaction rollback, not test teardown, must remove every attempted fact.
			condition := `current_setting('app.buyer_id',true)=` + quoteLiteral(b.cap.Scope.OwnerID)
			// Sequence progress is deliberately nontransactional: proves this exact
			// injection fired instead of accepting an unrelated earlier failure.
			mustExec(t, b.f.owner, `CREATE SEQUENCE public.`+name+`_hits`)
			mustExec(t, b.f.owner, `GRANT USAGE ON SEQUENCE public.`+name+`_hits TO commerce_checkout_runtime,commerce_checkout_writer`)
			mustExec(t, b.f.owner, `CREATE FUNCTION public.`+name+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF `+condition+` THEN PERFORM nextval('public.`+name+`_hits'); RAISE EXCEPTION 'checkout synthetic fault'; END IF; RETURN NEW; END $$`)
			mustExec(t, b.f.owner, `CREATE TRIGGER `+name+` BEFORE INSERT ON `+table+` FOR EACH ROW EXECUTE FUNCTION public.`+name+`()`)
			t.Cleanup(func() {
				mustExec(t, b.f.owner, `DROP TRIGGER `+name+` ON `+table)
				mustExec(t, b.f.owner, `DROP FUNCTION public.`+name+`()`)
				mustExec(t, b.f.owner, `DROP SEQUENCE public.`+name+`_hits`)
			})
			if _, e := b.begin(t04Key("bc-fault")); e == nil {
				t.Fatal("fault did not fire")
			}
			var fired bool
			if e := b.f.owner.QueryRow(context.Background(), `SELECT is_called FROM public.`+name+`_hits`).Scan(&fired); e != nil || !fired {
				t.Fatalf("injected table not reached: %s %v", table, e)
			}
			if b.facts(t) != before {
				t.Fatal("rollback left checkout or orphan job")
			}
			var reserved int64
			if e := b.f.owner.QueryRow(context.Background(), `SELECT reserved FROM inventory.balances WHERE warehouse_id=$1 AND sku_id=$2`, b.stock.warehouse.ID, b.stock.skus[0].ID).Scan(&reserved); e != nil || reserved != 0 {
				t.Fatalf("fault reserved=%d %v", reserved, e)
			}
		})
	}
}

func quoteLiteral(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }

func TestBuyerCheckoutFinalCapabilityClockAfterBalanceWait(t *testing.T) {
	b := bcSetup(t)
	name := "checkout-wait-" + t04Tag()
	pool, e := platform.OpenCheckoutPool(context.Background(), withApplicationName(t, b.poolURL, name))
	if e != nil {
		t.Fatal(e)
	}
	defer pool.Close()
	b.service = bcService(t, pool)
	holder, e := b.f.owner.Begin(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer holder.Rollback(context.Background())
	if _, e = holder.Exec(context.Background(), `SELECT 1 FROM inventory.balances WHERE warehouse_id=$1 AND sku_id=$2 FOR UPDATE`, b.stock.warehouse.ID, b.stock.skus[0].ID); e != nil {
		t.Fatal(e)
	}
	var expiry time.Time
	if e = b.f.owner.QueryRow(context.Background(), `UPDATE buyer.capability_sessions SET expires_at=clock_timestamp()+interval '700 milliseconds' WHERE id=$1 RETURNING expires_at`, b.cap.Scope.SessionID).Scan(&expiry); e != nil {
		t.Fatal(e)
	}
	before := b.facts(t)
	done := make(chan error, 1)
	go func() { _, e := b.begin(t04Key("bc-expired-wait")); done <- e }()
	waitForDatabaseLock(t, b.f.owner, name)
	var valid bool
	if e = b.f.owner.QueryRow(context.Background(), `SELECT clock_timestamp()<$1`, expiry).Scan(&valid); e != nil || !valid {
		t.Fatalf("test missed valid-before-wait window %v", e)
	}
	mustExec(t, b.f.owner, `SELECT pg_sleep(GREATEST(0,extract(epoch FROM $1::timestamptz-clock_timestamp()))+0.02)`, expiry)
	if e = holder.Commit(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e = waitError(t, done); !errors.Is(e, buyer.ErrUnauthorized) {
		t.Fatalf("expired capability held stock: %v", e)
	}
	if b.facts(t) != before {
		t.Fatal("expired wait left hold")
	}
}

func bcDue(t *testing.T, b bcHarness, result checkout.Result) {
	t.Helper()
	mustExec(t, b.f.owner, `UPDATE checkout.orders SET expires_at=clock_timestamp()-interval '1 second',created_at=clock_timestamp()-interval '901 seconds' WHERE id=$1`, result.OrderID)
	mustExec(t, b.f.owner, `UPDATE inventory.reservations SET expires_at=clock_timestamp()-interval '1 second',created_at=clock_timestamp()-interval '901 seconds' WHERE id=$1`, result.OrderID)
}

func bcExpire(t *testing.T, b bcHarness, result checkout.Result, generation int64) string {
	t.Helper()
	var disposition string
	var retry *time.Time
	if e := b.worker.QueryRow(context.Background(), `SELECT disposition,retry_at FROM checkout.expire_held($1,$2)`, result.OrderID, generation).Scan(&disposition, &retry); e != nil {
		t.Fatal(e)
	}
	return disposition
}

func TestBuyerCheckoutExpiryFencesAndConservation(t *testing.T) {
	b := bcSetup(t)
	key := t04Key("bc-expire")
	result, e := b.begin(key)
	if e != nil {
		t.Fatal(e)
	}
	if got := bcExpire(t, b, result, 1); got != "NOT_DUE" {
		t.Fatalf("early=%s", got)
	}
	bcDue(t, b, result)
	if got := bcExpire(t, b, result, 2); got != "STALE" {
		t.Fatalf("wrong generation=%s", got)
	}
	if got := bcExpire(t, b, result, 1); got != "EXPIRED" {
		t.Fatalf("due=%s", got)
	}
	before := b.facts(t)
	if got := bcExpire(t, b, result, 1); got != "STALE" {
		t.Fatalf("duplicate=%s", got)
	}
	if b.facts(t) != before {
		t.Fatal("duplicate expiry wrote facts")
	}
	order, e := b.service.Get(context.Background(), b.cap.Token, b.f.storeA1, result.OrderID)
	if e != nil || order.CommercialState != "CANCELLED" {
		t.Fatalf("expired order: %s %v", order.CommercialState, e)
	}
	var sum, reserved int64
	var actor string
	if e = b.f.owner.QueryRow(context.Background(), `SELECT sum(delta_reserved),max(actor_kind) FILTER(WHERE kind='RELEASE') FROM inventory.ledger WHERE checkout_id=$1`, result.OrderID).Scan(&sum, &actor); e != nil {
		t.Fatal(e)
	}
	if e = b.f.owner.QueryRow(context.Background(), `SELECT reserved FROM inventory.balances WHERE warehouse_id=$1 AND sku_id=$2`, b.stock.warehouse.ID, b.stock.skus[0].ID).Scan(&reserved); e != nil {
		t.Fatal(e)
	}
	if sum != 0 || reserved != 0 || actor != "SYSTEM_EXPIRY" {
		t.Fatalf("release conservation %d/%d/%s", sum, reserved, actor)
	}
	// Historic receipt neither extends nor resurrects the original hold.
	if replay, e := b.begin(key); e != nil || !reflect.DeepEqual(replay, result) {
		t.Fatalf("expired replay: %v", e)
	}
	if next, e := b.begin(t04Key("bc-recheckout")); e != nil || next.OrderID == result.OrderID {
		t.Fatalf("new checkout after cancellation: %v", e)
	}
	for _, reservationState := range []string{"PAYMENT_PENDING", "COMMITTED"} {
		t.Run(reservationState, func(t *testing.T) {
			c := bcSetup(t)
			r, e := c.begin(t04Key("bc-pending"))
			if e != nil {
				t.Fatal(e)
			}
			bcDue(t, c, r)
			// Fixture-only state fence, NOT an implemented StartPayment race test.
			mustExec(t, c.f.owner, `UPDATE inventory.reservations SET state=$2,generation=2 WHERE id=$1`, r.OrderID, reservationState)
			mustExec(t, c.f.owner, `UPDATE checkout.orders SET generation=2,commercial_state='AWAITING_PAYMENT' WHERE id=$1`, r.OrderID)
			before := c.facts(t)
			for _, generation := range []int64{1, 2} {
				if got := bcExpire(t, c, r, generation); got != "STALE" {
					t.Fatalf("pending stock released: %s", got)
				}
			}
			if c.facts(t) != before {
				t.Fatal("pending expiry changed ledger")
			}
		})
	}
}

func TestBuyerCheckoutActualRiverExpiry(t *testing.T) {
	for _, mode := range []string{"due", "early", "stale", "duplicate"} {
		t.Run(mode, func(t *testing.T) { bcActualRiverExpiry(t, mode) })
	}
}

type bcExpiryJob struct {
	OrderID    string `json:"order_id"`
	Generation int64  `json:"generation"`
	Version    int    `json:"version"`
}

func (bcExpiryJob) Kind() string { return "checkout_expiry_v1" }

func bcActualRiverExpiry(t *testing.T, mode string) {
	p := psSetupItemsOn(t, pwIsolatedFixture(t), 1)
	b, r := p.bcHarness, p.hold
	if mode != "early" {
		bcDue(t, b, r)
	}
	if mode == "stale" {
		mustExec(t, b.f.owner, `UPDATE checkout.orders SET generation=2 WHERE id=$1`, r.OrderID)
		mustExec(t, b.f.owner, `UPDATE inventory.reservations SET generation=2 WHERE id=$1`, r.OrderID)
	}
	// A fresh cluster isolates this real worker run without changing its fixed
	// producer queue or weakening family admission.
	queue := ewQueue
	mustExec(t, b.f.owner, `UPDATE river_expiry.river_job SET state='available',scheduled_at=clock_timestamp() WHERE id=$1`, r.JobID)
	var e error
	w, e := checkout.NewExpiryWorker(context.Background(), b.worker)
	if e != nil {
		t.Fatal(e)
	}
	workers := river.NewWorkers()
	river.AddWorker(workers, w)
	client, e := river.NewClient(riverpgxv5.New(b.worker), &river.Config{Schema: "river_expiry", Workers: workers, Queues: map[string]river.QueueConfig{queue: {MaxWorkers: 2}}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if e != nil {
		t.Fatal(e)
	}
	if e = client.Start(context.Background()); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if e := client.StopAndCancel(ctx); e != nil {
			t.Error(e)
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	var state, job string
	var attempted bool
	var attempts, firstAttempts int
	for time.Now().Before(deadline) {
		if e = b.f.owner.QueryRow(context.Background(), `SELECT o.commercial_state,j.state,j.attempted_at IS NOT NULL,j.attempt FROM checkout.orders o JOIN river_expiry.river_job j ON j.id=o.job_id WHERE o.id=$1`, r.OrderID).Scan(&state, &job, &attempted, &attempts); e != nil {
			t.Fatal(e)
		}
		wantState, wantJob := "CANCELLED", "completed"
		if mode == "early" {
			wantState, wantJob = "DRAFT", "scheduled"
		} else if mode == "stale" {
			wantState = "DRAFT"
		}
		if state == wantState && job == wantJob && attempted {
			if mode == "duplicate" {
				if firstAttempts == 0 {
					// Redeliver the linked job, not a new orphan ID forbidden by
					// admission. River must actually execute another attempt.
					firstAttempts = attempts
					mustExec(t, b.f.owner, `UPDATE river_expiry.river_job SET state='available',finalized_at=NULL,scheduled_at=clock_timestamp() WHERE id=$1`, r.JobID)
					continue
				}
				if attempts <= firstAttempts {
					time.Sleep(20 * time.Millisecond)
					continue
				}
			}
			wantReleases := 0
			if wantState == "CANCELLED" {
				wantReleases = 1
			}
			if n := countRows(t, b.f.owner, `SELECT count(*) FROM inventory.ledger WHERE checkout_id=$1 AND kind='RELEASE'`, r.OrderID); n != wantReleases {
				t.Fatalf("River release count=%d want%d", n, wantReleases)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("real River expiry %s/%s", state, job)
}
