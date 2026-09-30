package foundation_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
	"livecommerce/internal/buyer"
	"livecommerce/internal/checkout"
	"livecommerce/internal/command"
	"livecommerce/internal/storefront"
)

const ewQueue = "checkout_expiry_v1"

// Every EW test owns pwIsolatedFixture's labelled, loopback-only PG container.
// The suite database's default River queue is never consumed or mutated here.
func ewFixture(t *testing.T) *testFixture {
	t.Helper()
	return pwIsolatedFixture(t)
}

func ewSetup(t *testing.T, f *testFixture, lines int, historical ...string) psHarness {
	t.Helper()
	p := psSetupItemsOn(t, f, lines, historical...)
	table := "river_expiry.river_job"
	if len(historical) == 1 && historical[0] == "river" {
		table = "river.river_job"
	}
	if queue := pwQueueIn(t, f.owner, table, p.hold.JobID); queue != ewQueue {
		t.Fatalf("checkout producer queue=%s", queue)
	}
	return p
}

// ewCloseSeedPools releases only the five role pools created for one finished
// producer seed. The fixture owner and runtime pools belong to the caller.
func ewCloseSeedPools(t *testing.T, p psHarness) {
	t.Helper()
	show := func(setting string) int {
		var raw string
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := p.f.owner.QueryRow(ctx, "SHOW "+setting).Scan(&raw); err != nil {
			t.Fatal("seed pool capacity SHOW failed")
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatal("seed pool capacity is not numeric")
		}
		return n
	}
	max, reserved, super := show("max_connections"), show("reserved_connections"), show("superuser_reserved_connections")
	if max != 60 || reserved < 0 || super < 0 || reserved+super >= max {
		t.Fatalf("unexpected isolated PG capacity: max=%d reserved=%d super=%d", max, reserved, super)
	}
	seeds := []struct {
		kind string
		pool *pgxpool.Pool
	}{
		{"checkout", p.pool}, {"worker", p.worker},
		{"buyer_runtime", p.a.runtime}, {"buyer_issuer", p.a.issuer}, {"identity", p.a.identity},
	}
	ownerRole, sharedRole := p.f.owner.Config().ConnConfig.User, p.f.runtime.Config().ConnConfig.User
	seen := make(map[string]bool, len(seeds))
	for _, seed := range seeds {
		role := seed.pool.Config().ConnConfig.User
		if role == "" || role == ownerRole || role == sharedRole || seen[role] {
			t.Fatal("seed pool role is not uniquely owned")
		}
		seen[role] = true
		count := func() int {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var n int
			if err := p.f.owner.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND usename=$1`, role).Scan(&n); err != nil {
				t.Fatal("seed role activity count failed")
			}
			return n
		}
		stats, before := seed.pool.Stat(), count()
		if stats.AcquiredConns() != 0 || before == 0 || stats.TotalConns() != int32(before) {
			t.Fatalf("seed %s ownership mismatch: total=%d acquired=%d backend=%d", seed.kind, stats.TotalConns(), stats.AcquiredConns(), before)
		}
		t.Logf("seed %s capacity=%d reserved=%d super=%d total=%d idle=%d acquired=%d pool_max=%d backend_before=%d", seed.kind, max, reserved, super, stats.TotalConns(), stats.IdleConns(), stats.AcquiredConns(), stats.MaxConns(), before)
		seed.pool.Close()
		deadline := time.Now().Add(2 * time.Second)
		for n := count(); n != 0; n = count() {
			if time.Now().After(deadline) {
				t.Fatalf("seed %s retained %d backends after close", seed.kind, n)
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Logf("seed %s backend_after=0", seed.kind)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		ownerErr, runtimeErr := p.f.owner.Ping(ctx), p.f.runtime.Ping(ctx)
		cancel()
		if ownerErr != nil || runtimeErr != nil {
			t.Fatal("shared fixture pool unavailable after seed close")
		}
	}
}

func ewDue(t *testing.T, p psHarness) {
	t.Helper()
	bcDue(t, p.bcHarness, p.hold)
	mustExec(t, p.f.owner, `UPDATE river_expiry.river_job SET scheduled_at=clock_timestamp() WHERE id=$1`, p.hold.JobID)
}

func ewJob(t *testing.T, pool *pgxpool.Pool, id int64) (state string, attempt int) {
	return ewJobIn(t, pool, "river_expiry.river_job", id)
}

func ewJobIn(t *testing.T, pool *pgxpool.Pool, table string, id int64) (state string, attempt int) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `SELECT state,attempt FROM `+table+` WHERE id=$1`, id).Scan(&state, &attempt); err != nil {
		t.Fatal(err)
	}
	return
}

// A second row-lock waiter can be soft-blocked by the first waiter rather
// than directly by the holder. Follow PostgreSQL's actual blocker chain to
// the owned transaction instead of guessing which backend is the head.
func ewBlockedContenders(t *testing.T, owner *pgxpool.Pool, jobID int64, paymentRole, workerRole string, holderPID int) (state string, payment, expiry bool) {
	t.Helper()
	err := owner.QueryRow(context.Background(), `WITH RECURSIVE chain(root_role,pid,seen) AS (
	 SELECT a.usename,a.pid,ARRAY[a.pid] FROM pg_stat_activity a
	 WHERE a.usename IN ($2,$3) AND a.wait_event_type='Lock'
	 UNION ALL
	 SELECT c.root_role,b.pid,c.seen||b.pid FROM chain c
	 CROSS JOIN LATERAL unnest(pg_blocking_pids(c.pid)) AS b(pid)
	 WHERE NOT b.pid=ANY(c.seen) AND cardinality(c.seen)<8
	 )
	 SELECT j.state,
	 coalesce((SELECT bool_or(root_role=$2 AND pid=$4) FROM chain),false),
	 coalesce((SELECT bool_or(root_role=$3 AND pid=$4) FROM chain),false)
	 FROM river_expiry.river_job j WHERE j.id=$1`, jobID, paymentRole, workerRole, holderPID).Scan(&state, &payment, &expiry)
	if err != nil {
		t.Fatal(err)
	}
	return
}

func ewAwait(t *testing.T, pool *pgxpool.Pool, id int64, want string) int {
	return ewAwaitIn(t, pool, "river_expiry.river_job", id, want)
}

func ewAwaitIn(t *testing.T, pool *pgxpool.Pool, table string, id int64, want string) int {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		state, attempt := ewJobIn(t, pool, table, id)
		if state == want && attempt > 0 {
			return attempt
		}
		time.Sleep(20 * time.Millisecond)
	}
	state, attempt := ewJobIn(t, pool, table, id)
	t.Fatalf("expiry job %d state=%s attempt=%d, want %s after actual claim", id, state, attempt, want)
	return 0
}

func ewAwaitSnooze(t *testing.T, pool *pgxpool.Pool, id int64) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		var state string
		var attempt int
		var attempted, future bool
		if err := pool.QueryRow(context.Background(), `SELECT state,attempt,attempted_at IS NOT NULL,scheduled_at>clock_timestamp()
		 FROM river_expiry.river_job WHERE id=$1`, id).Scan(&state, &attempt, &attempted, &future); err != nil {
			t.Fatal(err)
		}
		// River decrements attempt on JobSnooze. attempted_at proves this
		// observed scheduled state followed a real claim, not initial insertion.
		if state == "scheduled" && attempt == 0 && attempted && future {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("early expiry was not claimed and snoozed against DB deadline")
}

func ewClient(t *testing.T, pool *pgxpool.Pool, concurrency int) *river.Client[pgx.Tx] {
	t.Helper()
	client, err := checkout.NewExpiryClient(context.Background(), pool, concurrency)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pwStopClient(t, client) })
	return client
}

func ewReady(t *testing.T, worker *pgxpool.Pool) bool {
	t.Helper()
	var ready bool
	if err := worker.QueryRow(context.Background(), `SELECT checkout.expiry_queue_ready()`).Scan(&ready); err != nil {
		t.Fatal(err)
	}
	return ready
}

func ewRemoveRouter(t *testing.T, owner *pgxpool.Pool) {
	t.Helper()
	// Only the new checksummed migration is rewound in this test-owned database.
	mustExec(t, owner, `DROP TRIGGER checkout_expiry_queue_route_v1 ON river.river_job`)
	mustExec(t, owner, `DROP FUNCTION checkout.expiry_queue_ready()`)
	mustExec(t, owner, `DROP FUNCTION checkout.route_expiry_queue_v1()`)
	mustExec(t, owner, `DROP FUNCTION checkout.expiry_job_linked(bigint)`)
	mustExec(t, owner, `DELETE FROM public.lc_schema_migrations WHERE version='post_river/0002_checkout_expiry_queue.sql'`)
}

func ewRouterAbsent(t *testing.T, owner *pgxpool.Pool) {
	t.Helper()
	var ledger, trigger int
	if err := owner.QueryRow(context.Background(), `SELECT
	 (SELECT count(*) FROM public.lc_schema_migrations WHERE version='post_river/0002_checkout_expiry_queue.sql'),
	 (SELECT count(*) FROM pg_trigger WHERE tgrelid='river.river_job'::regclass AND tgname='checkout_expiry_queue_route_v1')`).Scan(&ledger, &trigger); err != nil {
		t.Fatal(err)
	}
	if ledger != 0 || trigger != 0 {
		t.Fatalf("failed migration partly installed: ledger=%d trigger=%d", ledger, trigger)
	}
}

// The real checkout service still inserts its job before begin_hold. Middleware
// models the older producer's requested queue without reproducing business SQL.
func ewOldBegin(t *testing.T, p psHarness, historical ...string) checkout.Result {
	t.Helper()
	b := p.bcHarness
	var currentVersion int64
	if err := b.f.owner.QueryRow(context.Background(), `SELECT version FROM storefront.carts WHERE owner_id=$1`, b.cap.Scope.OwnerID).Scan(&currentVersion); err != nil {
		t.Fatalf("old begin read cart: %v", err)
	}
	cart, err := b.cart(t04Key("ew-cart"), storefront.CartInput{ExpectedVersion: currentVersion, Items: []storefront.Item{{SKUID: b.stock.skus[0].ID, Quantity: 1}}})
	if err != nil {
		t.Fatalf("old begin set cart: %v", err)
	}
	var destinationVersion int64
	if err := b.f.owner.QueryRow(context.Background(), `SELECT current_version FROM storefront.destination_heads WHERE owner_id=$1 AND cart_id=$2`, b.cap.Scope.OwnerID, cart.ID).Scan(&destinationVersion); err != nil {
		t.Fatalf("old begin read destination head: %v", err)
	}
	destinationInput := bdHome(cart)
	destinationInput.ExpectedVersion = destinationVersion
	destination, err := bdSet(b.cqHarness, t04Key("ew-destination"), destinationInput)
	if err != nil {
		t.Fatalf("old begin destination: %v", err)
	}
	quote, err := cqBuyer(b.a.runtime, b.cap, func(ctx context.Context, tx pgx.Tx, s buyer.Scope) (storefront.Quote, error) {
		return storefront.CreateQuote(ctx, tx, s, t04Key("ew-quote"), storefront.QuoteInput{CartVersion: cart.Version, MarketID: b.market.ID, Country: "TW", Method: "delivery:" + b.delivery.Code})
	})
	if err != nil {
		t.Fatalf("old begin quote: %v", err)
	}
	b.input.QuoteID, b.input.DestinationID, b.input.CartVersion = quote.ID, destination.ID, cart.Version
	middleware := river.JobInsertMiddlewareFunc(func(ctx context.Context, params []*rivertype.JobInsertParams, next func(context.Context) ([]*rivertype.JobInsertResult, error)) ([]*rivertype.JobInsertResult, error) {
		for _, row := range params {
			if row.Kind == ewQueue {
				row.Queue = "default"
			}
		}
		return next(ctx)
	})
	schema := "river_expiry"
	if len(historical) == 1 {
		schema = "river"
	}
	jobs, err := river.NewClient(riverpgxv5.New(b.pool), &river.Config{Schema: schema, JobInsertMiddleware: []rivertype.JobInsertMiddleware{middleware}})
	if err != nil {
		t.Fatalf("old begin river client: %v", err)
	}
	service, err := checkout.New(context.Background(), b.pool, jobs)
	if err != nil {
		t.Fatalf("old begin checkout service: %v", err)
	}
	result, err := service.Begin(context.Background(), b.cap.Token, b.f.storeA1, t04Key("ew-legacy"), b.input)
	if err != nil {
		t.Fatalf("old begin checkout service.Begin: %v", err)
	}
	return result
}

func TestBuyerCheckoutExpiryRuntimeMigrationUpgradeRollbackAndLedger(t *testing.T) {
	f := lriPre0032Fixture(t)
	p := ewSetup(t, f, 1, "river")
	worker := p.worker
	if !ewReady(t, worker) {
		t.Fatal("fresh migration not ready")
	}
	ewRemoveRouter(t, f.owner)
	mustExec(t, f.owner, `UPDATE river.river_job SET queue='default' WHERE id=$1`, p.hold.JobID)
	legacy := ewOldBegin(t, p, "river")
	if queue := pwQueue(t, f.owner, legacy.JobID); queue != "default" {
		t.Fatalf("legacy producer queue=%s", queue)
	}
	if _, err := p.start(t04Key("ew-upgrade-pending")); err != nil {
		t.Fatal(err)
	}
	var advancedGeneration int64
	if err := f.owner.QueryRow(context.Background(), `SELECT generation FROM checkout.orders WHERE id=$1`, p.hold.OrderID).Scan(&advancedGeneration); err != nil || advancedGeneration != 2 {
		t.Fatalf("pre-upgrade payment did not advance current generation: %d %v", advancedGeneration, err)
	}
	advancedBefore := pwJobExceptQueue(t, f.owner, p.hold.JobID)
	// The original due time and every other River column must survive the move.
	before := pwJobExceptQueue(t, f.owner, legacy.JobID)
	terminal := ewOldBegin(t, p, "river").JobID
	mustExec(t, f.owner, `UPDATE river.river_job SET state='completed',queue='default',finalized_at=clock_timestamp() WHERE id=$1`, terminal)
	terminalBefore := pwJobExceptQueue(t, f.owner, terminal)
	var unrelated int64
	if err := f.owner.QueryRow(context.Background(), `INSERT INTO river.river_job(kind,args,max_attempts,queue) VALUES('external_operation_v1','{}',2,'default') RETURNING id`).Scan(&unrelated); err != nil {
		t.Fatal(err)
	}
	unrelatedBefore := pwJobExceptQueue(t, f.owner, unrelated)
	validArgs := fmt.Sprintf(`{"order_id":"%s","generation":1,"version":1}`, legacy.OrderID)
	for _, tc := range []struct{ name, mutation, restore string }{
		{"running", `UPDATE river.river_job SET state='running' WHERE id=$1`, `UPDATE river.river_job SET state='scheduled' WHERE id=$1`},
		{"orphan", `UPDATE river.river_job SET args=jsonb_build_object('order_id',gen_random_uuid()::text,'generation',1,'version',1) WHERE id=$1`, `UPDATE river.river_job SET args=$2::jsonb WHERE id=$1`},
		{"decimal_generation", `UPDATE river.river_job SET args=jsonb_set(args,'{generation}','1.0'::jsonb) WHERE id=$1`, `UPDATE river.river_job SET args=$2::jsonb WHERE id=$1`},
		{"decimal_version", `UPDATE river.river_job SET args=jsonb_set(args,'{version}','1.0'::jsonb) WHERE id=$1`, `UPDATE river.river_job SET args=$2::jsonb WHERE id=$1`},
		{"extra_key", `UPDATE river.river_job SET args=args||'{"extra":true}'::jsonb WHERE id=$1`, `UPDATE river.river_job SET args=$2::jsonb WHERE id=$1`},
		{"unique_key", `UPDATE river.river_job SET unique_key=decode(repeat('ab',32),'hex') WHERE id=$1`, `UPDATE river.river_job SET unique_key=NULL WHERE id=$1`},
		{"wrong_queue", `UPDATE river.river_job SET queue='other_expiry_queue' WHERE id=$1`, `UPDATE river.river_job SET queue='default' WHERE id=$1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mustExec(t, f.owner, tc.mutation, legacy.JobID)
			rowBefore := pwJobExceptQueue(t, f.owner, legacy.JobID)
			queueBefore := pwQueue(t, f.owner, legacy.JobID)
			validBefore := pwJobExceptQueue(t, f.owner, p.hold.JobID)
			if err := lriApplyHistoricalPost(f, "post_river/0002_checkout_expiry_queue.sql"); err == nil {
				t.Fatal("invalid active expiry row migrated")
			}
			ewRouterAbsent(t, f.owner)
			if pwJobExceptQueue(t, f.owner, legacy.JobID) != rowBefore || pwQueue(t, f.owner, legacy.JobID) != queueBefore ||
				pwJobExceptQueue(t, f.owner, p.hold.JobID) != validBefore || pwQueue(t, f.owner, p.hold.JobID) != "default" {
				t.Fatal("failed migration changed invalid or valid legacy row")
			}
			if strings.Contains(tc.restore, "$2") {
				mustExec(t, f.owner, tc.restore, legacy.JobID, validArgs)
			} else {
				mustExec(t, f.owner, tc.restore, legacy.JobID)
			}
		})
	}
	var foreign int64
	if err := f.owner.QueryRow(context.Background(), `INSERT INTO river.river_job(kind,args,max_attempts,queue) VALUES('foreign_v1','{}',2,$1) RETURNING id`, ewQueue).Scan(&foreign); err != nil {
		t.Fatal(err)
	}
	if err := lriApplyHistoricalPost(f, "post_river/0002_checkout_expiry_queue.sql"); err == nil {
		t.Fatal("foreign reserved kind passed upgrade")
	}
	ewRouterAbsent(t, f.owner)
	if pwQueue(t, f.owner, p.hold.JobID) != "default" || pwJobExceptQueue(t, f.owner, p.hold.JobID) != advancedBefore {
		t.Fatal("foreign-kind failed migration partially moved valid row")
	}
	mustExec(t, f.owner, `DELETE FROM river.river_job WHERE id=$1`, foreign)
	if err := lriApplyHistoricalPost(f, "post_river/0002_checkout_expiry_queue.sql"); err != nil {
		t.Fatalf("valid upgrade: %v", err)
	}
	if pwQueue(t, f.owner, legacy.JobID) != ewQueue || pwJobExceptQueue(t, f.owner, legacy.JobID) != before ||
		pwQueue(t, f.owner, p.hold.JobID) != ewQueue || pwJobExceptQueue(t, f.owner, p.hold.JobID) != advancedBefore ||
		pwQueue(t, f.owner, terminal) != "default" || pwJobExceptQueue(t, f.owner, terminal) != terminalBefore ||
		pwQueue(t, f.owner, unrelated) != "default" || pwJobExceptQueue(t, f.owner, unrelated) != unrelatedBefore || !ewReady(t, worker) {
		t.Fatal("backfill changed non-queue fields/history/unrelated job or left router unready")
	}
	if err := lriApplyHistoricalPost(f, "post_river/0002_checkout_expiry_queue.sql"); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	mustExec(t, f.owner, `INSERT INTO public.lc_schema_migrations(version,checksum) VALUES('post_river/9999_unknown.sql','test-only')`)
	if err := lriApplyHistoricalPost(f, "post_river/0002_checkout_expiry_queue.sql"); err == nil {
		t.Fatal("unknown post-River version accepted")
	}
	mustExec(t, f.owner, `DELETE FROM public.lc_schema_migrations WHERE version='post_river/9999_unknown.sql'`)
	var checksum string
	if err := f.owner.QueryRow(context.Background(), `SELECT checksum FROM public.lc_schema_migrations WHERE version='post_river/0002_checkout_expiry_queue.sql'`).Scan(&checksum); err != nil {
		t.Fatal(err)
	}
	mustExec(t, f.owner, `UPDATE public.lc_schema_migrations SET checksum='test-invalid' WHERE version='post_river/0002_checkout_expiry_queue.sql'`)
	if err := lriApplyHistoricalPost(f, "post_river/0002_checkout_expiry_queue.sql"); err == nil {
		t.Fatal("changed checksum accepted")
	}
	mustExec(t, f.owner, `UPDATE public.lc_schema_migrations SET checksum=$1 WHERE version='post_river/0002_checkout_expiry_queue.sql'`, checksum)
	late := ewOldBegin(t, p, "river")
	if pwQueue(t, f.owner, late.JobID) != ewQueue {
		t.Fatal("old job-before-order producer was not routed at commit")
	}
	// The order advanced to generation 2 before upgrade. Its immutable
	// generation-1 task still routes, then the business transition returns STALE.
	bcDue(t, p.bcHarness, p.hold)
	mustExec(t, f.owner, `UPDATE river.river_job SET scheduled_at=clock_timestamp() WHERE id=$1`, p.hold.JobID)
	oldWorker, err := checkout.NewExpiryWorker(context.Background(), worker)
	if err != nil {
		t.Fatal(err)
	}
	workers := river.NewWorkers()
	river.AddWorker(workers, oldWorker)
	client, err := river.NewClient(riverpgxv5.New(worker), &river.Config{Schema: "river", Workers: workers,
		Queues: map[string]river.QueueConfig{ewQueue: {MaxWorkers: 1}}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ewAwaitIn(t, f.owner, "river.river_job", p.hold.JobID, "completed")
	pwStopClient(t, client)
	var order, reservation string
	if err := f.owner.QueryRow(context.Background(), `SELECT o.commercial_state,r.state FROM checkout.orders o JOIN inventory.reservations r ON r.id=o.id WHERE o.id=$1`, p.hold.OrderID).Scan(&order, &reservation); err != nil || order != "AWAITING_PAYMENT" || reservation != "PAYMENT_PENDING" {
		t.Fatalf("advanced legacy order released: %s/%s %v", order, reservation, err)
	}
	if n := countRows(t, f.owner, `SELECT count(*) FROM inventory.ledger WHERE checkout_id=$1 AND kind='RELEASE'`, p.hold.OrderID); n != 0 {
		t.Fatalf("advanced legacy job released stock %d times", n)
	}
}

func ewAssertOrder(t *testing.T, p psHarness, orderState, reservationState string, releases int) {
	t.Helper()
	var actualOrder, actualReservation string
	var reserved, allocated, onHand int64
	err := p.f.owner.QueryRow(context.Background(), `SELECT o.commercial_state,r.state,
	 coalesce(sum(b.reserved),0),coalesce(sum(b.allocated),0),coalesce(sum(b.on_hand),0)
	 FROM checkout.orders o JOIN inventory.reservations r ON r.id=o.id
	 JOIN inventory.reservation_lines l ON l.reservation_id=r.id
	 JOIN inventory.balances b ON (b.tenant_id,b.store_id,b.warehouse_id,b.sku_id)=(l.tenant_id,l.store_id,l.warehouse_id,l.sku_id)
	 WHERE o.id=$1 GROUP BY o.commercial_state,r.state`, p.hold.OrderID).
		Scan(&actualOrder, &actualReservation, &reserved, &allocated, &onHand)
	if err != nil {
		t.Fatal(err)
	}
	if actualOrder != orderState || actualReservation != reservationState {
		t.Fatalf("order/reservation=%s/%s want %s/%s", actualOrder, actualReservation, orderState, reservationState)
	}
	wantReserved := int64(2 * len(p.stock.skus))
	if orderState == "CANCELLED" || orderState == "CONFIRMED" {
		wantReserved = 0
	}
	wantAllocated := int64(0)
	if orderState == "CONFIRMED" {
		wantAllocated = int64(2 * len(p.stock.skus))
	}
	if reserved != wantReserved || allocated != wantAllocated || onHand != int64(10*len(p.stock.skus)) {
		t.Fatalf("balance reserved/allocated/on_hand=%d/%d/%d want %d/%d/%d", reserved, allocated, onHand, wantReserved, wantAllocated, 10*len(p.stock.skus))
	}
	if n := countRows(t, p.f.owner, `SELECT count(*) FROM inventory.ledger WHERE checkout_id=$1 AND kind='RELEASE'`, p.hold.OrderID); n != releases {
		t.Fatalf("release ledger=%d want %d", n, releases)
	}
	if n := countRows(t, p.f.owner, `SELECT count(*) FROM checkout.events WHERE order_id=$1 AND action='checkout.expired'`, p.hold.OrderID); n != map[bool]int{true: 1, false: 0}[orderState == "CANCELLED"] {
		t.Fatalf("expiry events=%d", n)
	}
}

func TestBuyerCheckoutExpiryRuntimeRiverTwoTenantReplayAndIsolation(t *testing.T) {
	f := ewFixture(t)
	one := ewSetup(t, f, 2)
	two := ewSetup(t, f, 1)
	if one.f.tenantA == two.f.tenantA || one.hold.OrderID == two.hold.OrderID {
		t.Fatal("two-tenant fixture collapsed")
	}
	// These are real payment and external-operation producers. Neither queue
	// belongs to the expiry client, even while due expiry jobs are consumed.
	unrelated := pqSetupItemsOn(t, f, nil, false, 1)
	unrelatedExpiry, external := pwDefaultDomainJobs(t, unrelated)
	var paymentJob int64
	if err := f.owner.QueryRow(context.Background(), `SELECT id FROM river_payment.river_job WHERE kind='payment_query_v1' AND args->>'operation_id'=$1`, unrelated.result.OperationID).Scan(&paymentJob); err != nil {
		t.Fatal(err)
	}
	// A due payment task remains eligible for payment maintenance; expiry must
	// not promote it from a different native schema.
	mustExec(t, f.owner, `UPDATE river_payment.river_job SET state='scheduled',scheduled_at=clock_timestamp()-interval '1 second' WHERE id=$1`, paymentJob)
	otherIDs := []int64{unrelatedExpiry, external, paymentJob}
	otherTables := []string{"river_expiry.river_job", "river.river_job", "river_payment.river_job"}
	otherBefore := make([]string, len(otherIDs))
	for i, id := range otherIDs {
		otherBefore[i] = miIsoRows(t, f, otherTables[i], "WHERE id="+fmt.Sprint(id))
	}
	ewDue(t, one)
	ewDue(t, two)
	client := ewClient(t, one.worker, 2)
	for _, p := range []psHarness{one, two} {
		ewAwait(t, f.owner, p.hold.JobID, "completed")
		ewAssertOrder(t, p, "CANCELLED", "EXPIRED", len(p.stock.skus))
	}
	pwStopClient(t, client)
	// The same committed job ID is redelivered after a client restart. A new
	// duplicate Insert is deliberately rejected by the immutable-link trigger.
	beforeAttempts := []int{0, 0}
	for i, p := range []psHarness{one, two} {
		_, beforeAttempts[i] = ewJob(t, f.owner, p.hold.JobID)
		mustExec(t, f.owner, `UPDATE river_expiry.river_job SET state='available',finalized_at=NULL,scheduled_at=clock_timestamp() WHERE id=$1`, p.hold.JobID)
	}
	restarted := ewClient(t, one.worker, 2)
	for i, p := range []psHarness{one, two} {
		attempt := ewAwait(t, f.owner, p.hold.JobID, "completed")
		if attempt <= beforeAttempts[i] {
			t.Fatalf("same-ID redelivery did not execute: %d -> %d", beforeAttempts[i], attempt)
		}
		ewAssertOrder(t, p, "CANCELLED", "EXPIRED", len(p.stock.skus))
	}
	pwStopClient(t, restarted)
	for i, id := range otherIDs {
		if after := miIsoRows(t, f, otherTables[i], "WHERE id="+fmt.Sprint(id)); after != otherBefore[i] {
			t.Fatalf("expiry client changed unrelated job %d: before=%s after=%s", id, otherBefore[i], after)
		}
	}
	if pwQueueIn(t, f.owner, "river.river_job", external) != "default" || pwQueueIn(t, f.owner, "river_payment.river_job", paymentJob) == ewQueue {
		t.Fatal("expiry client consumed an unrelated queue")
	}
}

func TestBuyerCheckoutExpiryRuntimeEarlyStalePendingAndConfirmed(t *testing.T) {
	f := ewFixture(t)
	early := ewSetup(t, f, 1)
	stale := ewSetup(t, f, 1)
	pending := ewSetup(t, f, 1)
	confirmed := pqSetupItemsOn(t, f, nil, false, 1)
	mustExec(t, f.owner, `UPDATE river_expiry.river_job SET scheduled_at=clock_timestamp() WHERE id=$1`, early.hold.JobID)
	ewDue(t, stale)
	mustExec(t, f.owner, `UPDATE checkout.orders SET generation=2 WHERE id=$1`, stale.hold.OrderID)
	mustExec(t, f.owner, `UPDATE inventory.reservations SET generation=2 WHERE id=$1`, stale.hold.OrderID)
	if _, err := pending.start(t04Key("ew-pending")); err != nil {
		t.Fatal(err)
	}
	ewDue(t, pending)
	hash := pcRecord(t, confirmed, pcFull(confirmed))
	if err := pcApply(confirmed.worker, confirmed.result.AttemptID, hash); err != nil {
		t.Fatal(err)
	}
	ewDue(t, confirmed.psHarness)
	client := ewClient(t, early.worker, 2)
	ewAwaitSnooze(t, f.owner, early.hold.JobID)
	ewAwait(t, f.owner, stale.hold.JobID, "completed")
	ewAwait(t, f.owner, pending.hold.JobID, "completed")
	ewAwait(t, f.owner, confirmed.hold.JobID, "completed")
	pwStopClient(t, client)
	ewAssertOrder(t, early, "DRAFT", "HELD", 0)
	ewAssertOrder(t, stale, "DRAFT", "HELD", 0)
	ewAssertOrder(t, pending, "AWAITING_PAYMENT", "PAYMENT_PENDING", 0)
	ewAssertOrder(t, confirmed.psHarness, "CONFIRMED", "COMMITTED", 0)
	var scheduled time.Time
	if err := f.owner.QueryRow(context.Background(), `SELECT scheduled_at FROM river_expiry.river_job WHERE id=$1`, early.hold.JobID).Scan(&scheduled); err != nil || !scheduled.After(time.Now()) {
		t.Fatalf("early job did not snooze until future DB deadline: %s %v", scheduled, err)
	}
	var currentGeneration int64
	if err := f.owner.QueryRow(context.Background(), `SELECT generation FROM checkout.orders WHERE id=$1`, pending.hold.OrderID).Scan(&currentGeneration); err != nil || currentGeneration != 2 {
		t.Fatalf("payment start did not advance generation: %d %v", currentGeneration, err)
	}
}

func TestBuyerCheckoutExpiryRuntimePaymentStartRace(t *testing.T) {
	f := ewFixture(t)
	for _, dueWhileWaiting := range []bool{false, true} {
		name := "payment-valid"
		if dueWhileWaiting {
			name = "expires-while-both-wait"
		}
		t.Run(name, func(t *testing.T) {
			p := ewSetup(t, f, 1)
			lock, err := f.owner.Begin(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Rollback(context.Background())
			var holderPID int
			if err := lock.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
				t.Fatal(err)
			}
			if _, err := lock.Exec(context.Background(), `SELECT 1 FROM checkout.orders WHERE id=$1 FOR UPDATE`, p.hold.OrderID); err != nil {
				t.Fatal(err)
			}
			mustExec(t, f.owner, `UPDATE river_expiry.river_job SET state='available',scheduled_at=clock_timestamp() WHERE id=$1`, p.hold.JobID)
			client := ewClient(t, p.worker, 1)
			mustExec(t, f.owner, `UPDATE river_expiry.river_job SET scheduled_at=clock_timestamp() WHERE id=$1`, p.hold.JobID)
			type paymentOutcome struct {
				result checkout.PaymentResult
				err    error
			}
			paymentDone := make(chan paymentOutcome, 1)
			// Arm the payment contender as soon as the actual River worker is
			// blocked. Its SQL lock timeout is one second, so a blind wait for
			// both callers would miss the lease's first attempt.
			deadline := time.Now().Add(8 * time.Second)
			var state string
			var paymentWaiting, expiryWaiting bool
			for time.Now().Before(deadline) {
				state, _, expiryWaiting = ewBlockedContenders(t, f.owner, p.hold.JobID, p.pool.Config().ConnConfig.User, p.worker.Config().ConnConfig.User, holderPID)
				if state == "running" && expiryWaiting {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if state != "running" || !expiryWaiting {
				t.Fatalf("River expiry did not block on owned order row: state=%s blocked=%t", state, expiryWaiting)
			}
			go func() {
				result, err := p.start(t04Key("ew-race-payment"))
				paymentDone <- paymentOutcome{result, err}
			}()
			// Both real callers must be blocked by this transaction's order row,
			// while River visibly holds a running lease. This is the race witness.
			deadline = time.Now().Add(2 * time.Second)
			witness := false
			for time.Now().Before(deadline) {
				state, paymentWaiting, expiryWaiting = ewBlockedContenders(t, f.owner, p.hold.JobID, p.pool.Config().ConnConfig.User, p.worker.Config().ConnConfig.User, holderPID)
				if state == "running" && paymentWaiting && expiryWaiting {
					witness = true
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !witness {
				t.Fatalf("payment and River expiry did not contend: state=%s payment_blocked=%t expiry_blocked=%t roles=%s/%s",
					state, paymentWaiting, expiryWaiting, p.pool.Config().ConnConfig.User, p.worker.Config().ConnConfig.User)
			}
			if dueWhileWaiting {
				if _, err := lock.Exec(context.Background(), `UPDATE checkout.orders SET expires_at=clock_timestamp()-interval '1 second',created_at=clock_timestamp()-interval '901 seconds' WHERE id=$1`, p.hold.OrderID); err != nil {
					t.Fatal(err)
				}
				if _, err := lock.Exec(context.Background(), `UPDATE inventory.reservations SET expires_at=clock_timestamp()-interval '1 second',created_at=clock_timestamp()-interval '901 seconds' WHERE id=$1`, p.hold.OrderID); err != nil {
					t.Fatal(err)
				}
			}
			if err := lock.Commit(context.Background()); err != nil {
				t.Fatal(err)
			}
			var outcome paymentOutcome
			select {
			case outcome = <-paymentDone:
			case <-time.After(8 * time.Second):
				t.Fatal("contending payment start did not settle")
			}
			if dueWhileWaiting {
				if !errors.Is(outcome.err, command.ErrConflict) || outcome.result.AttemptID != "" {
					t.Fatalf("expired order did not return business conflict: %+v %v", outcome.result, outcome.err)
				}
				ewAwait(t, f.owner, p.hold.JobID, "completed")
				ewAssertOrder(t, p, "CANCELLED", "EXPIRED", 1)
				if n := countRows(t, f.owner, `SELECT count(*) FROM checkout.payment_attempts WHERE order_id=$1`, p.hold.OrderID); n != 0 {
					t.Fatalf("expired order has %d payment attempts", n)
				}
			} else {
				if outcome.err != nil || outcome.result.AttemptID == "" {
					t.Fatalf("valid contending payment was rejected: %+v %v", outcome.result, outcome.err)
				}
				// The expiry worker either snoozes an early hold or observes
				// payment's new generation and completes STALE. Both preserve stock.
				ewAssertOrder(t, p, "AWAITING_PAYMENT", "PAYMENT_PENDING", 0)
				if n := countRows(t, f.owner, `SELECT count(*) FROM checkout.payment_attempts WHERE order_id=$1`, p.hold.OrderID); n != 1 {
					t.Fatalf("valid order has %d payment attempts", n)
				}
				var persistedAttempt string
				if err := f.owner.QueryRow(context.Background(), `SELECT id::text FROM checkout.payment_attempts WHERE order_id=$1`, p.hold.OrderID).Scan(&persistedAttempt); err != nil || persistedAttempt != outcome.result.AttemptID {
					t.Fatalf("payment result does not match durable attempt: result=%s persisted=%s err=%v", outcome.result.AttemptID, persistedAttempt, err)
				}
			}
			pwStopClient(t, client)
		})
	}
}

func TestBuyerCheckoutExpiryRuntimeLateOldProducerPoll(t *testing.T) {
	f := ewFixture(t)
	p := ewSetup(t, f, 1)
	client := ewClient(t, p.worker, 1)
	late := ewOldBegin(t, p)
	if queue := pwQueueIn(t, f.owner, "river_expiry.river_job", late.JobID); queue != ewQueue {
		t.Fatalf("late old producer committed to %s", queue)
	}
	// The INSERT announced the requested default queue before deferred commit
	// routing; this already-running fixed-queue client must find it by polling.
	mustExec(t, f.owner, `UPDATE checkout.orders SET expires_at=clock_timestamp()-interval '1 second',created_at=clock_timestamp()-interval '901 seconds' WHERE id=$1`, late.OrderID)
	mustExec(t, f.owner, `UPDATE inventory.reservations SET expires_at=clock_timestamp()-interval '1 second',created_at=clock_timestamp()-interval '901 seconds' WHERE id=$1`, late.OrderID)
	mustExec(t, f.owner, `UPDATE river_expiry.river_job SET scheduled_at=clock_timestamp() WHERE id=$1`, late.JobID)
	ewAwait(t, f.owner, late.JobID, "completed")
	pwStopClient(t, client)
	var order, reservation string
	if err := f.owner.QueryRow(context.Background(), `SELECT o.commercial_state,r.state FROM checkout.orders o JOIN inventory.reservations r ON r.id=o.id WHERE o.id=$1`, late.OrderID).Scan(&order, &reservation); err != nil || order != "CANCELLED" || reservation != "EXPIRED" {
		t.Fatalf("late old producer job was not executed: %s/%s %v", order, reservation, err)
	}
	if n := countRows(t, f.owner, `SELECT count(*) FROM inventory.ledger WHERE checkout_id=$1 AND kind='RELEASE'`, late.OrderID); n != 1 {
		t.Fatalf("late old producer release count=%d", n)
	}
	if state, attempt := ewJob(t, f.owner, p.hold.JobID); state != "scheduled" || attempt != 0 {
		t.Fatalf("unrelated future expiry was consumed: %s/%d", state, attempt)
	}
}

type ewBinary struct {
	cmd  *exec.Cmd
	done chan error
}

func ewBuildBinary(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "expiry-worker")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/expiry-worker")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build real expiry-worker: %v %s", err, out)
	}
	return binary
}

func ewWorkerDSN(t *testing.T, f *testFixture) (string, string) {
	t.Helper()
	u, err := url.Parse(bcRole(t, f, "commerce_worker"))
	if err != nil {
		t.Fatal(err)
	}
	app := "ew_binary_" + t04Tag()
	q := u.Query()
	q.Set("application_name", app)
	u.RawQuery = q.Encode()
	return u.String(), app
}

func ewStartBinary(t *testing.T, binary, dsn string) *ewBinary {
	t.Helper()
	cmd := exec.Command(binary)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "COMMERCE_EXPIRY_WORKER_ENABLED=1",
		"COMMERCE_EXPIRY_WORKER_DATABASE_URL=" + dsn, "COMMERCE_EXPIRY_WORKER_CONCURRENCY=1",
		"COMMERCE_ACCOUNT_KEYS_JSON=invalid", "COMMERCE_PAYMENT_WORKER_DATABASE_URL=invalid"}
	cmd.Stdout = io.Discard
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{}, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		scan := bufio.NewScanner(stderr)
		for scan.Scan() {
			if strings.HasSuffix(scan.Text(), " INFO expiry_worker_ready") {
				select {
				case ready <- struct{}{}:
				default:
				}
			}
		}
	}()
	p := &ewBinary{cmd: cmd, done: make(chan error, 1)}
	go func() { <-readerDone; p.done <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	select {
	case <-ready:
	case err := <-p.done:
		t.Fatalf("expiry-worker exited before ready: %v", err)
	case <-time.After(8 * time.Second):
		t.Fatal("expiry-worker did not report ready after River Start")
	}
	return p
}

func ewExit(t *testing.T, p *ewBinary, signal syscall.Signal, wantSuccess bool) {
	t.Helper()
	if err := p.cmd.Process.Signal(signal); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-p.done:
		if (err == nil) != wantSuccess {
			t.Fatalf("expiry-worker signal %v exit=%v want success=%t", signal, err, wantSuccess)
		}
	case <-time.After(8 * time.Second):
		t.Fatalf("expiry-worker did not exit after %v", signal)
	}
}

func ewConnections(t *testing.T, f *testFixture, app string) int {
	t.Helper()
	var n int
	if err := f.owner.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity WHERE datname='lc_foundation_test' AND application_name=$1`, app).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestBuyerCheckoutExpiryRuntimeBinarySignalAndPoolCleanup(t *testing.T) {
	f := ewFixture(t)
	binary := ewBuildBinary(t)
	disabled := exec.Command(binary)
	disabled.Env = []string{"PATH=" + os.Getenv("PATH"), "COMMERCE_EXPIRY_WORKER_ENABLED=0", "COMMERCE_EXPIRY_WORKER_DATABASE_URL=invalid", "COMMERCE_ACCOUNT_KEYS_JSON=invalid"}
	if out, err := disabled.CombinedOutput(); err != nil || len(out) != 0 {
		t.Fatalf("disabled process opened resources: %v %q", err, out)
	}
	dsn, app := ewWorkerDSN(t, f)
	p := ewStartBinary(t, binary, dsn)
	deadline := time.Now().Add(8 * time.Second)
	for ewConnections(t, f, app) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if ewConnections(t, f, app) == 0 {
		t.Fatal("ready process has no worker DB connection")
	}
	ewExit(t, p, syscall.SIGTERM, true)
	waitPoolsGone(t, f, "SIGTERM left worker connections", app)
}

func TestBuyerCheckoutExpiryRuntimeBinaryCrashRiverRescue(t *testing.T) {
	f := ewFixture(t)
	p := ewSetup(t, f, 1)
	ewDue(t, p)
	lock, err := f.owner.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(context.Background())
	var holderPID int
	if err := lock.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatal(err)
	}
	if _, err := lock.Exec(context.Background(), `SELECT 1 FROM checkout.orders WHERE id=$1 FOR UPDATE`, p.hold.OrderID); err != nil {
		t.Fatal(err)
	}
	binary := ewBuildBinary(t)
	dsn, app := ewWorkerDSN(t, f)
	crashed := ewStartBinary(t, binary, dsn)
	deadline := time.Now().Add(8 * time.Second)
	witness := false
	for time.Now().Before(deadline) {
		var state string
		var blocked bool
		err := f.owner.QueryRow(context.Background(), `SELECT j.state,
	 EXISTS(SELECT 1 FROM pg_stat_activity a WHERE a.application_name=$2 AND $3::int=ANY(pg_blocking_pids(a.pid)))
	 FROM river_expiry.river_job j WHERE j.id=$1`, p.hold.JobID, app, holderPID).Scan(&state, &blocked)
		if err != nil {
			t.Fatal(err)
		}
		if state == "running" && blocked {
			witness = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !witness {
		t.Fatal("no running River lease blocked on the owned order lock")
	}
	ewExit(t, crashed, syscall.SIGKILL, false)
	if err := lock.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state, _ := ewJob(t, f.owner, p.hold.JobID); state != "running" {
		t.Fatalf("SIGKILL did not leave a River running lease: %s", state)
	}
	// Clock-age only the killed process's owned attempted_at. River's default
	// one-hour lease and 30-second rescue scan are unchanged: this is not an SLA.
	var aged int64
	if err := f.owner.QueryRow(context.Background(), `UPDATE river_expiry.river_job SET attempted_at=clock_timestamp()-interval '2 hours'
	 WHERE id=$1 AND state='running' RETURNING id`, p.hold.JobID).Scan(&aged); err != nil || aged != p.hold.JobID {
		t.Fatalf("age owned running lease: %d %v", aged, err)
	}
	restarted := ewStartBinary(t, binary, dsn)
	rescueDeadline := time.Now().Add(65 * time.Second)
	var state string
	var attempt int
	var rescued bool
	for time.Now().Before(rescueDeadline) {
		if err := f.owner.QueryRow(context.Background(), `SELECT state,attempt,
	 coalesce(errors::text LIKE '%Stuck job rescued by JobRescuer%',false)
	 FROM river_expiry.river_job WHERE id=$1`, p.hold.JobID).Scan(&state, &attempt, &rescued); err != nil {
			t.Fatal(err)
		}
		if state == "completed" && attempt >= 2 && rescued {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if state != "completed" || attempt < 2 || !rescued {
		t.Fatalf("River rescue did not finish same job: state=%s attempt=%d rescued=%t", state, attempt, rescued)
	}
	ewExit(t, restarted, syscall.SIGTERM, true)
	ewAssertOrder(t, p, "CANCELLED", "EXPIRED", 1)
	waitPoolsGone(t, f, "restarted process left worker connections", app)
}
