package foundation_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
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
	integration "livecommerce/internal/integrations/core"
	"livecommerce/internal/platform"
	"livecommerce/internal/pricing"
	"livecommerce/internal/storefront"
)

type psHarness struct {
	bcHarness
	starter                 *checkout.PaymentStarter
	input                   checkout.PaymentInput
	hold                    checkout.Result
	account, binding, proof string
}

func psStarter(t *testing.T, pool *pgxpool.Pool, profile string) *checkout.PaymentStarter {
	return psStarterIn(t, pool, profile, "river_payment")
}

func psStarterIn(t *testing.T, pool *pgxpool.Pool, profile, schema string) *checkout.PaymentStarter {
	t.Helper()
	jobs, e := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: schema})
	if e != nil {
		t.Fatal(e)
	}
	s, e := checkout.NewPaymentStarter(context.Background(), pool, jobs, profile)
	if e != nil {
		t.Fatal(e)
	}
	return s
}

// A new synthetic tenant avoids changing the shared USD fixture's currency.
// All identities/keys/proofs are fictional; the task-owned PG container is the
// teardown boundary. No production role can mint this MOCK qualification.
func psSetup(t *testing.T) psHarness {
	return psSetupItems(t, 1)
}
func psSetupItems(t *testing.T, skuCount int) psHarness {
	return psSetupItemsOn(t, fixture(t), skuCount)
}

// Runtime queue audits require an isolated database, not the shared fixture's
// deliberately corrupted/relocated jobs. Domain setup remains identical.
func psSetupItemsOn(t *testing.T, base *testFixture, skuCount int, historical ...string) psHarness {
	t.Helper()
	expirySchema, paymentSchema := "river_expiry", "river_payment"
	if len(historical) == 1 && historical[0] == "river" {
		expirySchema, paymentSchema = "river", "river"
	} else if len(historical) != 0 {
		t.Fatal("unsupported historical River fixture")
	}
	ctx := context.Background()
	f := *base
	f.tenantA, f.storeA1, f.principalA = randomUUID(), randomUUID(), randomUUID()
	f.tokens = map[string]string{"a": randomToken()}
	mustExec(t, f.owner, `INSERT INTO control.tenants(id,name) VALUES($1,'payment mock tenant')`, f.tenantA)
	mustExec(t, f.owner, `INSERT INTO control.stores(tenant_id,id,name,currency) VALUES($1,$2,'payment mock store','TWD')`, f.tenantA, f.storeA1)
	mustExec(t, f.owner, `INSERT INTO identity.principals(id) VALUES($1)`, f.principalA)
	mustExec(t, f.owner, `INSERT INTO identity.memberships(tenant_id,principal_id) VALUES($1,$2)`, f.tenantA, f.principalA)
	mustExec(t, f.owner, `INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission)
 SELECT $1,$2,$3,p FROM unnest(ARRAY['store:read','catalog:read','catalog:write','inventory:read','inventory:write','inventory:reserve','pricing:read','pricing:write','integration:manage','integration:read','integration:execute']) p`, f.tenantA, f.storeA1, f.principalA)
	tx, e := f.owner.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if e = insertSession(ctx, tx, f.tokens["a"], f.principalA, "merchant", t06GoFuture(), nil); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	quantities := make([]int64, skuCount)
	for i := range quantities {
		quantities[i] = 10
	}
	h := cqHarness{f: &f, a: openBuyerTestPools(t, &f), stock: t04CreateStock(t, &f, f.tokens["a"], f.storeA1, quantities...)}
	h.service, e = buyer.New(h.a.issuer, time.Hour)
	if e != nil {
		t.Fatal(e)
	}
	h.cap = mustIssue(t, h.service, f.storeA1)
	h.market, e = pricingScoped(ctx, &f, f.tokens["a"], f.storeA1, "pricing:write", func(tx pgx.Tx, s platform.Scope) (pricing.Market, error) {
		return pricing.CreateMarket(ctx, tx, s, t04Key("ps-market"), pricing.MarketInput{Code: "tw", Name: "Synthetic TW", Currency: "TWD"})
	})
	if e != nil {
		t.Fatal(e)
	}
	zero := int64(0)
	h.policy = pricing.PolicyInput{MarketID: h.market.ID, Country: "TW", Currency: "TWD", ShippingMode: "country_flat", ShippingMinor: &zero, TaxMode: "none", TaxBasis: "goods", TaxRateBPS: &zero, QuoteTTLSeconds: 300, Enabled: true, ConfigurationRef: "synthetic only"}
	delivery := fulfillment.ServiceInput{MarketID: h.market.ID, Country: "TW", Code: "home", PolicyVersion: 1, NameHans: "测试配送", NameHant: "測試配送", NameEN: "Mock delivery", DeliveryKind: "home", Mode: "MANUAL", Enabled: true, Visible: true}
	dsPolicy(t, h, delivery, 0, 0, true)
	if _, e = dsSet(h, t04Key("ps-delivery"), delivery); e != nil {
		t.Fatal(e)
	}
	allocation := fulfillment.AllocationInput{MarketID: h.market.ID, Country: "TW", Code: "home", ExpectedServiceVersion: 1, WarehouseIDs: []string{h.stock.warehouse.ID}}
	if _, e = daSet(h, t04Key("ps-allocation"), allocation); e != nil {
		t.Fatal(e)
	}
	poolURL := bcRole(t, &f, "commerce_checkout_runtime")
	pool, e := platform.OpenCheckoutPool(ctx, poolURL)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(pool.Close)
	worker, e := platform.OpenWorkerPool(ctx, bcRole(t, &f, "commerce_worker"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(worker.Close)
	b := bcHarness{cqHarness: h, pool: pool, worker: worker, service: bcServiceIn(t, pool, expirySchema), delivery: delivery, allocation: allocation, poolURL: poolURL}
	items := make([]storefront.Item, skuCount)
	for i := range items {
		items[i] = storefront.Item{SKUID: h.stock.skus[skuCount-1-i].ID, Quantity: 2}
	}
	b.prepare(t, h.cap, items)
	hold, e := b.begin(t04Key("ps-hold"))
	if e != nil {
		t.Fatal(e)
	}
	p := psHarness{bcHarness: b, starter: psStarterIn(t, pool, "PROVIDER_MOCK", paymentSchema), hold: hold, account: randomUUID(), binding: randomUUID(), proof: randomUUID(), input: checkout.PaymentInput{OrderID: hold.OrderID, MethodCode: "payuni_credit", MethodVersion: 1}}
	tx, e = f.owner.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	statements := []string{
		`INSERT INTO integration.bindings(id,tenant_id,store_id,principal_id,provider,external_asset_id) VALUES($5,$1,$2,$3,'payuni','SANDBOX:mock-account')`,
		`INSERT INTO integration.merchant_accounts(id,tenant_id,store_id,principal_id,provider,environment,account_id,binding_id,credential_version) VALUES($4,$1,$2,$3,'payuni','SANDBOX','mock-account',$5,1)`,
		`INSERT INTO integration.account_credentials(tenant_id,store_id,connection_id,version,key_id,nonce,ciphertext,principal_id) VALUES($1,$2,$4,1,'mock_key',decode(repeat('00',12),'hex'),decode(repeat('00',17),'hex'),$3)`,
		`INSERT INTO payments.account_qualifications(id,tenant_id,store_id,connection_id,credential_version,environment,code,proof_class,evidence_ref,observed_at,expires_at) VALUES($6,$1,$2,$4,1,'SANDBOX','payuni_credit','PROVIDER_MOCK','disposable fixture; no provider request',clock_timestamp()-interval '1 second',clock_timestamp()+interval '1 hour')`,
		`INSERT INTO payments.method_versions(tenant_id,store_id,market_id,country,code,version,provider,environment,connection_id,binding_version,currency,name_hans,name_hant,name_en,enabled,visible,sort_order,min_amount_minor,max_amount_minor,principal_id,qualification_id) VALUES($1,$2,$7,'TW','payuni_credit',1,'payuni','SANDBOX',$4,1,'TWD','测试','測試','Mock',true,true,0,100,19999900,$3,$6)`,
		`INSERT INTO payments.method_heads(tenant_id,store_id,market_id,country,code,current_version) VALUES($1,$2,$7,'TW','payuni_credit',1)`,
	}
	// Bind all fixture identities with explicit types, including statements which
	// use only a subset. This is test data, not a dynamic production SQL builder.
	for _, sql := range statements {
		if _, e = tx.Exec(ctx, `WITH fixture_args AS (SELECT $1::uuid t,$2::uuid s,$3::uuid p,$4::uuid a,$5::uuid b,$6::uuid q,$7::uuid m) `+sql, f.tenantA, f.storeA1, f.principalA, p.account, p.binding, p.proof, h.market.ID); e != nil {
			t.Fatal(e)
		}
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	return p
}

func (p psHarness) start(key string) (checkout.PaymentResult, error) {
	return p.starter.StartPayment(context.Background(), p.cap.Token, p.f.storeA1, key, p.input)
}
func (p psHarness) facts(t *testing.T) (out [7]int) {
	t.Helper()
	e := p.f.owner.QueryRow(context.Background(), `SELECT
 (SELECT count(*) FROM checkout.payment_attempts WHERE owner_id=$1),
 (SELECT count(*) FROM integration.operations WHERE buyer_owner_id=$1),
 (SELECT count(*) FROM checkout.command_results WHERE owner_id=$1 AND operation='checkout.payment.start'),
 (SELECT count(*) FROM checkout.events WHERE owner_id=$1 AND action='checkout.payment_started'),
 (SELECT count(*) FROM river_payment.river_job WHERE kind='payment_query_v1'),
 (SELECT count(*) FROM checkout.orders WHERE owner_id=$1 AND commercial_state='AWAITING_PAYMENT'),
 (SELECT count(*) FROM inventory.reservations WHERE buyer_owner_id=$1 AND state='PAYMENT_PENDING')`, p.cap.Scope.OwnerID).Scan(&out[0], &out[1], &out[2], &out[3], &out[4], &out[5], &out[6])
	if e != nil {
		t.Fatal(e)
	}
	return
}

func TestBuyerPaymentAtomicReplayAndPendingStock(t *testing.T) {
	p := psSetup(t)
	before := p.facts(t)
	key := t04Key("ps-start")
	out, e := p.start(key)
	if e != nil {
		t.Fatal(e)
	}
	after := p.facts(t)
	for i := range before {
		if after[i]-before[i] != 1 {
			t.Fatalf("atomic fact %d: %v -> %v", i, before, after)
		}
	}
	if out.AmountMinor != 2500 || out.Currency != "TWD" || out.Generation != 2 || len(out.MerchantTradeNo) != 23 || out.State != "PAYMENT_PENDING" {
		t.Fatalf("frozen result: %+v", out)
	}
	var principal *string
	var actor, mode, state, session string
	var generation int64
	e = p.f.owner.QueryRow(context.Background(), `SELECT principal_id::text,actor_kind,lease_mode,state,generation,buyer_session_id::text FROM integration.operations WHERE id=$1`, out.OperationID).Scan(&principal, &actor, &mode, &state, &generation, &session)
	if e != nil || principal != nil || actor != "BUYER_PAYMENT_QUERY" || mode != "" || state != "UNKNOWN" || generation != 1 || session != p.cap.Scope.SessionID {
		t.Fatalf("query actor/state: %v %s %s %s", e, actor, mode, state)
	}
	var raw []byte
	var kind string
	if e = p.f.owner.QueryRow(context.Background(), `SELECT kind,args FROM river_payment.river_job WHERE id=$1`, out.JobID).Scan(&kind, &raw); e != nil {
		t.Fatal(e)
	}
	var args map[string]any
	if e = json.Unmarshal(raw, &args); e != nil || len(args) != 2 || args["operation_id"] != out.OperationID || args["version"] != float64(1) || kind != "payment_query_v1" {
		t.Fatalf("private job %s %s", kind, raw)
	}
	second := p.cap
	second.Token = base64.RawURLEncoding.EncodeToString(tokenHash(randomUUID()))
	second.Scope.SessionID = randomUUID()
	mustExec(t, p.f.owner, `INSERT INTO buyer.capability_sessions(tenant_id,store_id,owner_id,id,token_hash,expires_at) VALUES($1,$2,$3,$4,$5,clock_timestamp()+interval '1 hour')`, second.Scope.TenantID, second.Scope.StoreID, second.Scope.OwnerID, second.Scope.SessionID, tokenHash(second.Token))
	mustExec(t, p.f.owner, `UPDATE integration.bindings SET enabled=false,semantic_version=semantic_version+1 WHERE id=$1`, p.binding)
	replay, e := p.starter.StartPayment(context.Background(), second.Token, p.f.storeA1, key, p.input)
	if e != nil || replay != out || p.facts(t) != after {
		t.Fatalf("historical owner replay: %v", e)
	}
	if _, e = psStarter(t, p.pool, "LIVE").StartPayment(context.Background(), second.Token, p.f.storeA1, key, p.input); !errors.Is(e, command.ErrConflict) {
		t.Fatalf("profile replay changed: %v", e)
	}
	mustExec(t, p.f.owner, `UPDATE buyer.capability_sessions SET revoked_at=clock_timestamp() WHERE id=$1`, second.Scope.SessionID)
	if _, e = p.starter.StartPayment(context.Background(), second.Token, p.f.storeA1, key, p.input); !errors.Is(e, buyer.ErrUnauthorized) {
		t.Fatalf("revoked replay: %v", e)
	}
	bcDue(t, p.bcHarness, p.hold)
	if got := bcExpire(t, p.bcHarness, p.hold, 1); got != "STALE" {
		t.Fatalf("old expiry: %s", got)
	}
	var reserved int64
	if e = p.f.owner.QueryRow(context.Background(), `SELECT reserved FROM inventory.balances WHERE warehouse_id=$1 AND sku_id=$2`, p.stock.warehouse.ID, p.stock.skus[0].ID).Scan(&reserved); e != nil || reserved != 2 {
		t.Fatalf("pending stock=%d %v", reserved, e)
	}
	var disposition, claimMode string
	var gen int64
	if e = p.worker.QueryRow(context.Background(), `SELECT disposition,generation,mode FROM integration.claim_operation($1,30,$2)`, out.OperationID, randomBytes(32)).Scan(&disposition, &gen, &claimMode); e != nil || disposition != "claimed" || claimMode != "reconcile" || gen != 2 {
		t.Fatalf("historical query claim: %s %s %d %v", disposition, claimMode, gen, e)
	}
}

func TestBuyerPaymentConcurrentKeys(t *testing.T) {
	for _, same := range []bool{true, false} {
		t.Run(fmt.Sprint(same), func(t *testing.T) {
			p := psSetup(t)
			before := p.facts(t)
			key := t04Key("ps-race")
			var wg sync.WaitGroup
			results := make(chan checkout.PaymentResult, 2)
			errs := make(chan error, 2)
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					k := key
					if !same {
						k += fmt.Sprint(i)
					}
					r, e := p.start(k)
					results <- r
					errs <- e
				}(i)
			}
			wg.Wait()
			close(results)
			close(errs)
			successes := 0
			for e := range errs {
				if e == nil {
					successes++
				} else if !errors.Is(e, command.ErrConflict) {
					t.Fatal(e)
				}
			}
			want := 1
			if same {
				want = 2
			}
			if successes != want {
				t.Fatalf("successes=%d want%d", successes, want)
			}
			var first string
			for r := range results {
				if r.AttemptID != "" {
					if first != "" && first != r.AttemptID {
						t.Fatal("duplicate attempts")
					}
					first = r.AttemptID
				}
			}
			after := p.facts(t)
			for i := range before {
				if after[i]-before[i] != 1 {
					t.Fatalf("race facts %v -> %v", before, after)
				}
			}
		})
	}
}

func TestBuyerPaymentAdmissionDenials(t *testing.T) {
	cases := []struct{ name, sql string }{
		{"hidden", `UPDATE payments.method_versions SET visible=false WHERE tenant_id=$1`},
		{"disabled", `UPDATE payments.method_versions SET enabled=false WHERE tenant_id=$1`},
		{"binding_disabled", `UPDATE integration.bindings SET enabled=false WHERE tenant_id=$1`},
		{"binding_revised", `UPDATE integration.bindings SET semantic_version=2 WHERE tenant_id=$1`},
		{"revoked_proof", `UPDATE payments.account_qualifications SET revoked_at=clock_timestamp() WHERE tenant_id=$1`},
		{"expired_proof", `UPDATE payments.account_qualifications SET observed_at=clock_timestamp()-interval '2 hours',expires_at=clock_timestamp()-interval '1 hour' WHERE tenant_id=$1`},
		{"future_proof", `UPDATE payments.account_qualifications SET observed_at=clock_timestamp()+interval '1 minute' WHERE tenant_id=$1`},
		{"inactive_market", `UPDATE pricing.markets SET active=false,version=version+1 WHERE tenant_id=$1`},
		{"fractional_TWD", `UPDATE checkout.orders SET total_minor=2501 WHERE tenant_id=$1`},
		{"method_amount_limit", `UPDATE payments.method_versions SET max_amount_minor=1000 WHERE tenant_id=$1`},
		{"rotated_credential", `INSERT INTO integration.account_credentials SELECT tenant_id,store_id,connection_id,2,key_id,nonce,ciphertext,principal_id,created_at FROM integration.account_credentials WHERE tenant_id=$1`},
		{"stale_method", ``}, {"SANDBOX", ``}, {"LIVE", ``}, {"foreign_owner", ``}, {"expired_hold", ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := psSetup(t)
			if tc.sql != "" {
				qualExec(t, p.f.owner, tc.sql, p.f.tenantA) // some rows rewind the qualification (revoke-only trigger, 0077)
			}
			switch tc.name {
			case "rotated_credential":
				mustExec(t, p.f.owner, `UPDATE integration.merchant_accounts SET credential_version=2 WHERE tenant_id=$1`, p.f.tenantA)
			case "stale_method":
				p.input.MethodVersion = 2
			case "SANDBOX", "LIVE":
				p.starter = psStarter(t, p.pool, tc.name)
			case "foreign_owner":
				p.cap = mustIssue(t, p.cqHarness.service, p.f.storeA1)
			case "expired_hold":
				bcDue(t, p.bcHarness, p.hold)
			}
			before := p.facts(t)
			out, e := p.start(t04Key("ps-deny"))
			if !errors.Is(e, command.ErrConflict) || out != (checkout.PaymentResult{}) {
				t.Fatalf("denied result=%+v err=%v", out, e)
			}
			if after := p.facts(t); after != before {
				t.Fatalf("denial mutated facts %v -> %v", before, after)
			}
		})
	}
}

func TestBuyerPaymentFaultRollback(t *testing.T) {
	for _, table := range []string{"river_payment.river_job", "checkout.payment_attempts", "integration.operations", "integration.operation_events", "checkout.events", "checkout.command_results"} {
		t.Run(table, func(t *testing.T) {
			p := psSetup(t)
			before := p.facts(t)
			name := "ps_fault_" + t04Tag()
			mustExec(t, p.f.owner, `CREATE SEQUENCE public.`+name+`_hits`)
			mustExec(t, p.f.owner, `GRANT USAGE ON SEQUENCE public.`+name+`_hits TO commerce_checkout_runtime,commerce_checkout_writer`)
			mustExec(t, p.f.owner, `CREATE FUNCTION public.`+name+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF current_setting('app.buyer_id',true)=`+quoteLiteral(p.cap.Scope.OwnerID)+` THEN PERFORM nextval('public.`+name+`_hits'); RAISE EXCEPTION 'payment synthetic fault'; END IF; RETURN NEW; END $$`)
			mustExec(t, p.f.owner, `CREATE TRIGGER `+name+` BEFORE INSERT ON `+table+` FOR EACH ROW EXECUTE FUNCTION public.`+name+`() `)
			t.Cleanup(func() {
				mustExec(t, p.f.owner, `DROP TRIGGER `+name+` ON `+table)
				mustExec(t, p.f.owner, `DROP FUNCTION public.`+name+`() `)
				mustExec(t, p.f.owner, `DROP SEQUENCE public.`+name+`_hits`)
			})
			if _, e := p.start(t04Key("ps-fault")); e == nil {
				t.Fatal("injected failure missing")
			}
			var fired bool
			if e := p.f.owner.QueryRow(context.Background(), `SELECT is_called FROM public.`+name+`_hits`).Scan(&fired); e != nil || !fired {
				t.Fatalf("fault point not reached: %s %v", table, e)
			}
			if after := p.facts(t); before != after {
				t.Fatalf("partial facts: %v -> %v", before, after)
			}
			var state string
			var gen, reserved int64
			if e := p.f.owner.QueryRow(context.Background(), `SELECT r.state,r.generation,b.reserved FROM inventory.reservations r JOIN inventory.balances b ON b.tenant_id=r.tenant_id AND b.store_id=r.store_id WHERE r.id=$1 AND b.sku_id=$2`, p.hold.OrderID, p.stock.skus[0].ID).Scan(&state, &gen, &reserved); e != nil || state != "HELD" || gen != 1 || reserved != 2 {
				t.Fatalf("stock rollback %s %d %d %v", state, gen, reserved, e)
			}
		})
	}
}

func TestBuyerPaymentFinalWaitGate(t *testing.T) {
	for _, kind := range []string{"session", "qualification", "market"} {
		t.Run(kind, func(t *testing.T) {
			p := psSetup(t)
			name := "payment-wait-" + t04Tag()
			pool, e := platform.OpenCheckoutPool(context.Background(), withApplicationName(t, p.poolURL, name))
			if e != nil {
				t.Fatal(e)
			}
			defer pool.Close()
			p.starter = psStarter(t, pool, "PROVIDER_MOCK")
			holder, e := p.f.owner.Begin(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			defer holder.Rollback(context.Background())
			if _, e = holder.Exec(context.Background(), `SELECT 1 FROM checkout.orders WHERE id=$1 FOR UPDATE`, p.hold.OrderID); e != nil {
				t.Fatal(e)
			}
			before := p.facts(t)
			var expiry time.Time
			if kind == "session" {
				if e = p.f.owner.QueryRow(context.Background(), `UPDATE buyer.capability_sessions SET expires_at=clock_timestamp()+interval '600 milliseconds' WHERE id=$1 RETURNING expires_at`, p.cap.Scope.SessionID).Scan(&expiry); e != nil {
					t.Fatal(e)
				}
			}
			done := make(chan error, 1)
			go func() { _, e := p.start(t04Key("ps-wait")); done <- e }()
			waitForDatabaseLock(t, p.f.owner, name)
			want := command.ErrConflict
			switch kind {
			case "session":
				var valid bool
				if e = p.f.owner.QueryRow(context.Background(), `SELECT clock_timestamp()<$1`, expiry).Scan(&valid); e != nil || !valid {
					t.Fatalf("missed valid wait: %v", e)
				}
				// resolve_scope holds SHARE on the session: revocation correctly
				// waits for this transaction. Wall-clock expiry needs rechecking.
				mustExec(t, p.f.owner, `SELECT pg_sleep(GREATEST(0,extract(epoch FROM $1::timestamptz-clock_timestamp()))+0.02)`, expiry)
				want = buyer.ErrUnauthorized
			case "qualification":
				qualExec(t, p.f.owner, `UPDATE payments.account_qualifications SET observed_at=clock_timestamp()-interval '2 hours',expires_at=clock_timestamp()-interval '1 hour' WHERE id=$1`, p.proof)
			case "market":
				mustExec(t, p.f.owner, `UPDATE pricing.markets SET active=false WHERE id=$1`, p.market.ID)
			}
			if e = holder.Commit(context.Background()); e != nil {
				t.Fatal(e)
			}
			if e = waitError(t, done); !errors.Is(e, want) {
				t.Fatalf("post-wait error %v want%v", e, want)
			}
			if after := p.facts(t); after != before {
				t.Fatalf("post-wait facts %v -> %v", before, after)
			}
		})
	}
}

func TestBuyerPaymentAuthorityAndDispatcherFence(t *testing.T) {
	p := psSetup(t)
	before := p.facts(t)
	var functionOID, proofOID, credentialOID uint32
	if e := p.f.owner.QueryRow(context.Background(), `SELECT 'checkout.start_payment(bytea,uuid,text,bytea,uuid,text,bigint,text,uuid,bigint)'::regprocedure::oid,'payments.account_qualifications'::regclass::oid,'integration.account_credentials'::regclass::oid`).Scan(&functionOID, &proofOID, &credentialOID); e != nil {
		t.Fatal(e)
	}
	for _, pool := range []*pgxpool.Pool{p.f.runtime, p.a.runtime, p.worker} {
		var allowed bool
		if e := pool.QueryRow(context.Background(), `SELECT has_function_privilege(current_user,$1::oid,'EXECUTE')`, functionOID).Scan(&allowed); e != nil || allowed {
			t.Fatalf("wrong SQL authority allowed %v", e)
		}
	}
	for _, priv := range []string{"INSERT", "UPDATE", "DELETE"} {
		var allowed bool
		if e := p.pool.QueryRow(context.Background(), `SELECT has_table_privilege(current_user,$1::oid,$2)`, proofOID, priv).Scan(&allowed); e != nil || allowed {
			t.Fatalf("runtime can mint proof %s %v", priv, e)
		}
	}
	for _, token := range []string{p.f.tokens["a"], randomToken()} {
		if _, e := p.starter.StartPayment(context.Background(), token, p.f.storeA1, t04Key("ps-auth"), p.input); !errors.Is(e, buyer.ErrUnauthorized) {
			t.Fatalf("wrong token %v", e)
		}
	}
	if _, e := p.starter.StartPayment(context.Background(), p.cap.Token, p.f.storeA2, t04Key("ps-store"), p.input); !errors.Is(e, buyer.ErrUnauthorized) {
		t.Fatalf("wrong store %v", e)
	}
	if p.facts(t) != before {
		t.Fatal("authority denial wrote facts")
	}
	out, e := p.start(t04Key("ps-fence"))
	if e != nil {
		t.Fatal(e)
	}
	var calls atomic.Int32
	route := t06Route()
	route.Provider = "payuni"
	route.Action = "payuni.query"
	route.Check = func(context.Context, integration.DispatchRequest) error { calls.Add(1); return nil }
	route.Dispatch = func(context.Context, integration.DispatchRequest) (integration.Outcome, error) {
		calls.Add(1)
		return integration.Outcome{State: "SUCCEEDED", Code: "must_not_run"}, nil
	}
	route.Reconcile = route.Dispatch
	queue := "ps_fence_" + t04Tag()
	client := t06StartDispatcher(t, p.worker, queue, []integration.DispatchRoute{route}, t06DispatchOptions())
	job, e := client.Insert(context.Background(), t06DuplicateOperationArgs{OperationID: out.OperationID, Version: 1}, &river.InsertOpts{Queue: queue, MaxAttempts: 1})
	if e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(5 * time.Second)
	finished := false
	for time.Now().Before(deadline) {
		var state, kind, operation string
		if e = p.f.owner.QueryRow(context.Background(), `SELECT state,kind,args->>'operation_id' FROM river.river_job WHERE id=$1`, job.Job.ID).Scan(&state, &kind, &operation); e != nil {
			t.Fatal(e)
		}
		if kind != "external_operation_v1" || operation != out.OperationID {
			t.Fatal("generic dispatcher job resolved to wrong family or operation")
		}
		if state == "discarded" || state == "cancelled" || state == "completed" {
			finished = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !finished || calls.Load() != 0 {
		t.Fatalf("generic dispatcher accessed buyer op: finished%v calls%d", finished, calls.Load())
	}
	var state string
	var gen int64
	if e = p.f.owner.QueryRow(context.Background(), `SELECT state,generation FROM integration.operations WHERE id=$1`, out.OperationID).Scan(&state, &gen); e != nil || state != "UNKNOWN" || gen != 1 {
		t.Fatalf("generic dispatch mutated operation %s %d %v", state, gen, e)
	}
	// The raw owner can inspect mock facts, but ordinary checkout cannot decrypt.
	var canRead bool
	if e = p.pool.QueryRow(context.Background(), `SELECT has_column_privilege(current_user,$1::oid,'ciphertext','SELECT')`, credentialOID).Scan(&canRead); e != nil || canRead {
		t.Fatalf("credential authority %v", e)
	}
}

func TestBuyerPaymentQualificationExpiresDuringFinalWrite(t *testing.T) {
	p := psSetup(t)
	name := "payment-late-wait-" + t04Tag()
	pool, e := platform.OpenCheckoutPool(context.Background(), withApplicationName(t, p.poolURL, name))
	if e != nil {
		t.Fatal(e)
	}
	defer pool.Close()
	p.starter = psStarter(t, pool, "PROVIDER_MOCK")
	before := p.facts(t)
	holder, e := p.f.owner.Begin(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer holder.Rollback(context.Background())
	if _, e = holder.Exec(context.Background(), `LOCK TABLE checkout.events IN ACCESS EXCLUSIVE MODE`); e != nil {
		t.Fatal(e)
	}
	var expiry time.Time
	if e = qualUpdateScan(context.Background(), p.f.owner, `UPDATE payments.account_qualifications SET expires_at=clock_timestamp()+interval '600 milliseconds' WHERE id=$1 RETURNING expires_at`, []any{p.proof}, &expiry); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { _, e := p.start(t04Key("ps-late-wait")); done <- e }()
	waitForDatabaseLock(t, p.f.owner, name)
	var valid bool
	if e = p.f.owner.QueryRow(context.Background(), `SELECT clock_timestamp()<$1`, expiry).Scan(&valid); e != nil || !valid {
		t.Fatalf("missed qualified-before-wait: %v", e)
	}
	mustExec(t, p.f.owner, `SELECT pg_sleep(GREATEST(0,extract(epoch FROM $1::timestamptz-clock_timestamp()))+0.02)`, expiry)
	if e = holder.Commit(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e = waitError(t, done); !errors.Is(e, command.ErrConflict) {
		t.Fatalf("late expired proof accepted: %v", e)
	}
	if after := p.facts(t); after != before {
		t.Fatalf("late expiry partial facts: %v -> %v", before, after)
	}
}

func TestBuyerPaymentFreshSessionAndQualificationAuthority(t *testing.T) {
	p := psSetup(t)
	foreign := psSetup(t)
	_, e := p.f.owner.Exec(context.Background(), `UPDATE payments.method_versions SET qualification_id=$2 WHERE tenant_id=$1`, p.f.tenantA, foreign.proof)
	daSQLState(t, e, "23503")
	var proofOID uint32
	if e = p.f.owner.QueryRow(context.Background(), `SELECT 'payments.account_qualifications'::regclass::oid`).Scan(&proofOID); e != nil {
		t.Fatal(e)
	}
	for _, pool := range []*pgxpool.Pool{p.f.runtime, p.a.runtime, p.worker, p.pool} {
		for _, priv := range []string{"INSERT", "UPDATE", "DELETE"} {
			var allowed bool
			if e = pool.QueryRow(context.Background(), `SELECT has_table_privilege(current_user,$1::oid,$2)`, proofOID, priv).Scan(&allowed); e != nil || allowed {
				t.Fatalf("proof privilege %s: %v %v", priv, allowed, e)
			}
		}
	}
	original := p.cap.Scope.SessionID
	p.cap.Token = base64.RawURLEncoding.EncodeToString(tokenHash(randomUUID()))
	p.cap.Scope.SessionID = randomUUID()
	mustExec(t, p.f.owner, `INSERT INTO buyer.capability_sessions(tenant_id,store_id,owner_id,id,token_hash,expires_at) VALUES($1,$2,$3,$4,$5,clock_timestamp()+interval '1 hour')`, p.cap.Scope.TenantID, p.cap.Scope.StoreID, p.cap.Scope.OwnerID, p.cap.Scope.SessionID, tokenHash(p.cap.Token))
	out, e := p.start(t04Key("ps-new-session"))
	if e != nil {
		t.Fatal(e)
	}
	var reservationSession, paymentSession string
	e = p.f.owner.QueryRow(context.Background(), `SELECT r.buyer_session_id::text,a.session_id::text FROM checkout.payment_attempts a JOIN inventory.reservations r ON r.id=a.order_id WHERE a.id=$1`, out.AttemptID).Scan(&reservationSession, &paymentSession)
	if e != nil || reservationSession != original || paymentSession != p.cap.Scope.SessionID {
		t.Fatalf("session provenance %s %s %v", reservationSession, paymentSession, e)
	}
}

func TestBuyerPaymentExpiredHoldRace(t *testing.T) {
	p := psSetup(t)
	bcDue(t, p.bcHarness, p.hold)
	before := p.facts(t)
	ready := make(chan struct{})
	done := make(chan error, 1)
	expired := make(chan string, 1)
	go func() { <-ready; _, e := p.start(t04Key("ps-expiry-race")); done <- e }()
	go func() {
		<-ready
		var disposition string
		var retry *time.Time
		e := p.worker.QueryRow(context.Background(), `SELECT disposition,retry_at FROM checkout.expire_held($1,1)`, p.hold.OrderID).Scan(&disposition, &retry)
		if e != nil {
			expired <- "ERROR"
		} else {
			expired <- disposition
		}
	}()
	close(ready)
	if e := waitError(t, done); !errors.Is(e, command.ErrConflict) {
		t.Fatalf("expired race payment: %v", e)
	}
	select {
	case s := <-expired:
		if s != "EXPIRED" {
			t.Fatalf("expiry race: %s", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("expiry did not complete")
	}
	if after := p.facts(t); after != before {
		t.Fatalf("expired race left payment %v -> %v", before, after)
	}
	var state string
	var reserved int64
	e := p.f.owner.QueryRow(context.Background(), `SELECT o.commercial_state,b.reserved FROM checkout.orders o JOIN inventory.balances b ON b.tenant_id=o.tenant_id AND b.store_id=o.store_id WHERE o.id=$1 AND b.sku_id=$2`, p.hold.OrderID, p.stock.skus[0].ID).Scan(&state, &reserved)
	if e != nil || state != "CANCELLED" || reserved != 0 {
		t.Fatalf("expiry conservation %s %d %v", state, reserved, e)
	}
}
