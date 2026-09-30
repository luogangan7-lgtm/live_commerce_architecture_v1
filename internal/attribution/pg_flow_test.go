package attribution

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"livecommerce/migrations"
)

// pg_flow_test.go is the implementer's REAL_PG flow check of unit ads-capi (not the independent MA03/MA08 gates): every
// migration on the isolated PG (0078/0079 must be merged), then consent hook -> sweeper transaction -> Check -> lease-fenced
// user data -> feed, all through the real definers under the real roles (commerce_worker, commerce_buyer_runtime). Fixture
// rows for the payment chain are written with session_replication_role=replica (FK triggers off, CHECKs still on) because
// only the CAPI-relevant columns matter here. Evidence tier: REAL_PG, no Meta. Skips unless LC_TEST_DATABASE_ALLOWED=1
// (scripts/dev/test-focused.sh sets it). Non-goal: LoadSecret's token half (needs a sealed credential; ads-tests MA08).

type pgFx struct {
	t                      *testing.T
	ctx                    context.Context
	owner, worker, buyerDB *pgxpool.Pool
	client                 *river.Client[pgx.Tx]
	tenant, store          string
	principal, binding     string
	product, sku, origin   string
}

func hexN(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func (f *pgFx) must(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.owner.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("%s: %v", strings.Fields(sql)[0], err)
	}
}

func (f *pgFx) str(sql string, args ...any) string {
	f.t.Helper()
	var out string
	if err := f.owner.QueryRow(f.ctx, sql, args...).Scan(&out); err != nil {
		f.t.Fatalf("query %.60s: %v", sql, err)
	}
	return out
}

func (f *pgFx) uuid() string { return f.str(`SELECT gen_random_uuid()::text`) }

// replica runs statements with FK triggers and user triggers off on one connection (superuser fixture only).
func (f *pgFx) replica(stmts ...[]any) {
	f.t.Helper()
	conn, err := f.owner.Acquire(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	defer conn.Release()
	if _, err = conn.Exec(f.ctx, `SET session_replication_role=replica`); err != nil {
		f.t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(f.ctx, `RESET session_replication_role`) }()
	for _, s := range stmts {
		if _, err = conn.Exec(f.ctx, s[0].(string), s[1:]...); err != nil {
			f.t.Fatalf("replica fixture: %v", err)
		}
	}
}

func (f *pgFx) login(cfg *pgxpool.Config, name, role string) *pgxpool.Pool {
	secret := hexN(16)
	f.must(`CREATE ROLE ` + name + ` LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE IN ROLE ` + role + ` PASSWORD '` + secret + `'`)
	u := url.URL{Scheme: "postgres", Host: cfg.ConnConfig.Host + ":" + strconv.Itoa(int(cfg.ConnConfig.Port)), Path: "/lc_foundation_test", RawQuery: "sslmode=disable"}
	u.User = url.UserPassword(name, secret)
	pool, err := pgxpool.New(f.ctx, u.String())
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(pool.Close)
	return pool
}

func newPGFx(t *testing.T) *pgFx {
	if os.Getenv("LC_TEST_DATABASE_ALLOWED") != "1" || os.Getenv("LC_TEST_DATABASE_URL") == "" {
		t.Skip("LC_TEST_DATABASE_ALLOWED=1 and LC_TEST_DATABASE_URL are required; attribution real-PG flow NOT_RUN")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(os.Getenv("LC_TEST_DATABASE_URL"))
	if err != nil || cfg.ConnConfig.Host != "127.0.0.1" || cfg.ConnConfig.Database != "lc_foundation_test" {
		t.Fatal("refusing database outside 127.0.0.1/lc_foundation_test")
	}
	owner, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	if err = migrations.Apply(ctx, owner); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	f := &pgFx{t: t, ctx: ctx, owner: owner}
	f.worker = f.login(cfg, "capi_test_worker", "commerce_worker")
	f.buyerDB = f.login(cfg, "capi_test_buyer", "commerce_buyer_runtime")
	if f.client, err = river.NewClient(riverpgxv5.New(f.worker), &river.Config{Schema: "river"}); err != nil {
		t.Fatal(err)
	}
	f.tenant, f.store, f.principal, f.binding = f.uuid(), f.uuid(), f.uuid(), f.uuid()
	f.product, f.sku = f.uuid(), f.uuid()
	f.origin = "https://capi-shop.example.test"
	f.must(`INSERT INTO control.tenants(id,name) VALUES($1,'capi-tenant')`, f.tenant)
	f.must(`INSERT INTO control.stores(tenant_id,id,name,currency) VALUES($1,$2,'Capi Shop, Inc','TWD')`, f.tenant, f.store)
	f.must(`INSERT INTO identity.principals(id) VALUES($1)`, f.principal)
	f.must(`INSERT INTO identity.memberships(tenant_id,principal_id) VALUES($1,$2)`, f.tenant, f.principal)
	for _, p := range []string{"store:read", "ads:manage"} {
		f.must(`INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission) VALUES($1,$2,$3,$4)`, f.tenant, f.store, f.principal, p)
	}
	f.must(`INSERT INTO integration.bindings(id,tenant_id,store_id,principal_id,provider,external_asset_id) VALUES($1,$2,$3,$4,'meta_dataset','7001')`,
		f.binding, f.tenant, f.store, f.principal)
	f.must(`INSERT INTO ads.store_settings(tenant_id,store_id,environment,capi_enabled,capi_dataset_binding,capi_enabled_by,capi_test_event_code)
		VALUES($1,$2,'SANDBOX',true,$3,$4,'TEST1234')`, f.tenant, f.store, f.binding, f.principal)
	f.must(`INSERT INTO control.storefront_publications(tenant_id,store_id,published) VALUES($1,$2,true)`, f.tenant, f.store)
	f.must(`INSERT INTO control.storefront_domains(tenant_id,store_id,origin,state,ownership_verified_at,tls_verified_at,valid_until,evidence_ref)
		VALUES($1,$2,$3,'ACTIVE',now()-interval '1 day',now()-interval '1 day',now()+interval '30 days','fixture')`, f.tenant, f.store, f.origin)
	f.must(`INSERT INTO catalog.products(tenant_id,store_id,id,name,description) VALUES($1,$2,$3,'Tea "Set", boxed','Two cups
and a pot')`, f.tenant, f.store, f.product)
	f.must(`INSERT INTO catalog.skus(tenant_id,store_id,id,product_id,code,currency,price_minor) VALUES($1,$2,$3,$4,'TEA-1','TWD',250000)`,
		f.tenant, f.store, f.sku, f.product)
	wh := f.uuid()
	f.must(`INSERT INTO inventory.warehouses(tenant_id,store_id,id,name) VALUES($1,$2,$3,'w1')`, f.tenant, f.store, wh)
	f.must(`INSERT INTO inventory.balances(tenant_id,store_id,warehouse_id,sku_id,on_hand) VALUES($1,$2,$3,$4,5)`, f.tenant, f.store, wh, f.sku)
	return f
}

type buyer struct {
	owner, session string
	hash           []byte
}

func (f *pgFx) newBuyer() buyer {
	token := hexN(32)
	sum := sha256.Sum256([]byte(token))
	b := buyer{owner: f.uuid(), session: f.uuid(), hash: sum[:]}
	f.must(`INSERT INTO buyer.owners(tenant_id,store_id,id) VALUES($1,$2,$3)`, f.tenant, f.store, b.owner)
	f.must(`INSERT INTO buyer.capability_sessions(id,tenant_id,store_id,owner_id,token_hash,expires_at) VALUES($1,$2,$3,$4,$5,now()+interval '1 hour')`,
		b.session, f.tenant, f.store, b.owner, b.hash)
	return b
}

// consent runs customers.buyer_set_consent as the buyer runtime login, then (grant only) the A-3 hook in the same tx.
func (f *pgFx) consent(b buyer, granted bool, ua string) {
	f.t.Helper()
	tx, err := f.buyerDB.Begin(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	if _, err = tx.Exec(f.ctx, `SELECT customers.buyer_set_consent($1::bytea,$2::uuid,'ads_personalization','meta_ads',$3,'buyer_settings','policy-v1',gen_random_uuid())`,
		b.hash, f.store, granted); err != nil {
		f.t.Fatalf("buyer_set_consent: %v", err)
	}
	if granted {
		if err = PutCAPIContext(f.ctx, tx, b.hash, f.store, ua); err != nil {
			f.t.Fatalf("PutCAPIContext: %v", err)
		}
	}
	if err = tx.Commit(f.ctx); err != nil {
		f.t.Fatal(err)
	}
}

// attempt seeds quote -> order -> payment attempt -> CAPTURED fact for a buyer and returns the attempt id.
func (f *pgFx) attempt(b buyer, environment, profile, age string) string {
	f.t.Helper()
	cart, quote, order, market, dest, attempt, conn := f.uuid(), f.uuid(), f.uuid(), f.uuid(), f.uuid(), f.uuid(), f.uuid()
	snapshot := map[string]any{"id": quote, "cart_id": cart, "cart_version": 1, "market_version": 1, "currency": "TWD", "calculation_version": "v1",
		"policy":     map[string]any{"market_id": market, "country": "TW", "method": "delivery:manual", "version": 1, "currency": "TWD"},
		"created_at": "2026-09-01T00:00:00Z", "expires_at": "2026-09-01T00:30:00Z",
		"lines": []any{map[string]any{"sku_id": f.sku, "product_id": f.product, "code": "TEA-1", "quantity": 2, "unit_price_minor": 125000}}}
	raw, _ := json.Marshal(snapshot)
	job := f.str(`SELECT (floor(random()*1e9)+1)::bigint::text`)
	f.replica(
		[]any{`INSERT INTO storefront.quotes(tenant_id,store_id,owner_id,id,cart_id,creator_session_id,cart_version,market_id,market_version,country,method,policy_version,currency,created_at,expires_at,snapshot)
			VALUES($1,$2,$3,$4,$5,$6,1,$7,1,'TW','delivery:manual',1,'TWD','2026-09-01T00:00:00Z','2026-09-01T00:30:00Z',$8::jsonb)`,
			f.tenant, f.store, b.owner, quote, cart, b.session, market, string(raw)},
		[]any{`INSERT INTO checkout.orders(tenant_id,store_id,owner_id,id,creator_session_id,cart_id,cart_version,quote_id,destination_id,market_id,country,service_code,service_version,allocation_version,currency,total_minor,commercial_state,fulfillment_state,generation,expires_at,job_id,snapshot)
			VALUES($1,$2,$3,$4,$5,$6,1,$7,$8,$9,'TW','manual',1,1,'TWD',250000,'CONFIRMED','MANUAL_UNASSIGNED',1,now()+interval '10 minutes',$10::bigint,'{}'::jsonb)`,
			f.tenant, f.store, b.owner, order, b.session, cart, quote, dest, market, job},
		[]any{`INSERT INTO checkout.payment_attempts(tenant_id,store_id,owner_id,id,session_id,order_id,market_id,country,method_code,method_version,connection_id,credential_version,qualification_id,environment,execution_profile,binding_id,binding_version,currency,amount_minor,merchant_trade_no,state,generation,job_id)
			VALUES($1,$2,$3,$4,$5,$6,$7,'TW','payuni_credit',1,$8,1,$9,$10,$11,$12,1,'TWD',250000,$13,'PAYMENT_PENDING',2,$14::bigint)`,
			f.tenant, f.store, b.owner, attempt, b.session, order, market, conn, f.uuid(), environment, profile, f.binding, hexN(10), job + "1"},
		[]any{`INSERT INTO payments.facts(tenant_id,store_id,attempt_id,kind,amount_minor,currency,provider_reference,connection_id,execution_profile,environment,source_report_hash,received_at)
			VALUES($1,$2,$3,'CAPTURED',250000,'TWD',$4,$5,$6,$7,$8,now()-$9::interval)`,
			f.tenant, f.store, attempt, "pr_" + hexN(8), conn, profile, environment, sha256Bytes(attempt), age},
	)
	return attempt
}

func sha256Bytes(s string) []byte { sum := sha256.Sum256([]byte(s)); return sum[:] }

func (f *pgFx) eligible(attempt string) bool {
	f.t.Helper()
	var ok bool
	if err := f.worker.QueryRow(f.ctx, `SELECT ads.plan_capi_eligible($1::uuid)`, attempt).Scan(&ok); err != nil {
		f.t.Fatal(err)
	}
	return ok
}

func (f *pgFx) candidates() []string {
	f.t.Helper()
	rows, err := f.worker.Query(f.ctx, `SELECT a::text FROM ads.plan_capi_candidates(200) a`)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

func (f *pgFx) plan(attempt string) error {
	w := &sweepWorker{pool: f.worker}
	return w.planOne(f.ctx, f.client, attempt)
}

func (f *pgFx) opOf(attempt string) string {
	return f.str(`SELECT operation_id::text FROM ads.capi_events WHERE attempt_id=$1`, attempt)
}

func (f *pgFx) check(op string) string {
	f.t.Helper()
	var code string
	if err := f.worker.QueryRow(f.ctx, `SELECT ads.check_capi($1::uuid)`, op).Scan(&code); err != nil {
		f.t.Fatal(err)
	}
	return code
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func sqlState(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}

func TestAttributionRealPGFlow(t *testing.T) {
	f := newPGFx(t)
	const ua = "Mozilla/5.0 (fixture)"
	alice, bob := f.newBuyer(), f.newBuyer()

	t.Run("consent hook records a context only while consent holds", func(t *testing.T) {
		f.t = t
		// Bob never consents: the hook writes nothing even when called (consent_allows false).
		tx, _ := f.buyerDB.Begin(f.ctx)
		if err := PutCAPIContext(f.ctx, tx, bob.hash, f.store, ua); err != nil {
			t.Fatalf("hook without consent: %v", err)
		}
		_ = tx.Commit(f.ctx)
		if n := f.str(`SELECT count(*)::text FROM ads.capi_contexts WHERE owner_id=$1`, bob.owner); n != "0" {
			t.Fatalf("context without consent: %s", n)
		}
		f.consent(alice, true, ua+"\r\n")
		if got := f.str(`SELECT user_agent FROM ads.capi_contexts WHERE owner_id=$1`, alice.owner); got != ua {
			t.Fatalf("stored user agent = %q", got)
		}
		// The buyer runtime has no table access to the context or the events.
		if _, err := f.buyerDB.Exec(f.ctx, `SELECT * FROM ads.capi_contexts`); sqlState(err) != "42501" {
			t.Fatalf("buyer table read = %v", err)
		}
	})

	t.Run("sweeper plans exactly one operation per eligible attempt", func(t *testing.T) {
		f.t = t
		good := f.attempt(alice, "SANDBOX", "SANDBOX", "1 minute")
		never := f.attempt(bob, "SANDBOX", "SANDBOX", "1 minute")
		var noConsent string
		mock := f.attempt(alice, "SANDBOX", "PROVIDER_MOCK", "1 minute")
		liveFact := f.attempt(alice, "LIVE", "LIVE", "1 minute")
		old := f.attempt(alice, "SANDBOX", "SANDBOX", "6 days 1 hour")
		// Consent withdrawn while the context row still exists (the purge has not run): consent_allows alone must stop it (CD5).
		carol := f.newBuyer()
		f.consent(carol, true, ua)
		withdrawn := f.attempt(carol, "SANDBOX", "SANDBOX", "1 minute")
		if !f.eligible(withdrawn) {
			t.Fatal("consented attempt must be eligible before the withdrawal")
		}
		f.consent(carol, false, "")
		if n := f.str(`SELECT count(*)::text FROM ads.capi_contexts WHERE owner_id=$1`, carol.owner); n != "1" {
			t.Fatalf("context must still exist before the purge: %s", n)
		}
		noConsent = withdrawn // reported under "no consent" below
		got := f.candidates()
		if !contains(got, good) {
			t.Fatalf("eligible attempt missing from candidates %v", got)
		}
		for name, id := range map[string]string{"no consent (withdrawn, context kept)": noConsent, "never consented": never, "PROVIDER_MOCK": mock, "LIVE fact in SANDBOX store": liveFact, "older than 6 days": old} {
			if contains(got, id) || f.eligible(id) {
				t.Errorf("%s must not be eligible", name)
			}
		}
		if err := f.plan(good); err != nil {
			t.Fatalf("plan: %v", err)
		}
		op := f.opOf(good)
		var state, action, provider, key, queue, principal string
		var prio int
		if err := f.owner.QueryRow(f.ctx, `SELECT o.state,o.action,o.provider,o.semantic_key,j.queue,j.priority,o.principal_id::text
			FROM integration.operations o JOIN river.river_job j ON j.id=o.job_id WHERE o.id=$1`, op).Scan(&state, &action, &provider, &key, &queue, &prio, &principal); err != nil {
			t.Fatal(err)
		}
		if state != "READY" || action != "meta.capi.purchase" || provider != "meta_dataset" || key != "ads:capi:"+good || queue != "ads" || prio != 3 || principal != f.principal {
			t.Fatalf("op = %s %s %s %s %s %d %s", state, action, provider, key, queue, prio, principal)
		}
		var req map[string]any
		if err := json.Unmarshal([]byte(f.str(`SELECT request::text FROM integration.operations WHERE id=$1`, op)), &req); err != nil {
			t.Fatal(err)
		}
		if len(req) != 5 || req["v"] != float64(1) || req["attempt_id"] != good || req["event_id"] != "lc-purchase-"+good || req["test_event_code"] != "TEST1234" || req["event_time"] == nil {
			t.Fatalf("frozen request = %v", req)
		}
		// One per attempt, ever: it is no longer eligible, a second plan is a no-op commit, and a direct re-plan fails the PK.
		if f.eligible(good) || contains(f.candidates(), good) {
			t.Fatal("planned attempt is still eligible")
		}
		if err := f.plan(good); err != nil {
			t.Fatalf("second plan must be a no-op: %v", err)
		}
		if n := f.str(`SELECT count(*)::text FROM integration.operations WHERE semantic_key=$1`, "ads:capi:"+good); n != "1" {
			t.Fatalf("operations for the attempt: %s", n)
		}
		// The worker cannot plan a CAPI op without the ads-lane job of this very transaction.
		if _, err := f.worker.Exec(f.ctx, `SELECT ads.plan_capi($1::uuid,gen_random_uuid(),1)`, mock); sqlState(err) != "22023" {
			t.Fatalf("plan without eligibility/job = %v", err)
		}
	})

	t.Run("Check codes", func(t *testing.T) {
		f.t = t
		fresh := f.newBuyer()
		f.consent(fresh, true, ua)
		attempt := f.attempt(fresh, "SANDBOX", "SANDBOX", "1 minute")
		if err := f.plan(attempt); err != nil {
			t.Fatal(err)
		}
		op := f.opOf(attempt)
		if got := f.check(op); got != "" {
			t.Fatalf("baseline Check = %q", got)
		}
		if got := f.check(f.uuid()); got != "unknown_action" {
			t.Fatalf("unknown op = %q", got)
		}
		f.consent(fresh, false, "")
		if got := f.check(op); got != "consent_withdrawn" {
			t.Fatalf("after withdrawal = %q", got)
		}
		f.consent(fresh, true, ua)
		if got := f.check(op); got != "" {
			t.Fatalf("after re-grant = %q", got)
		}
		mutate := func(name, want, apply, restore string, args ...any) {
			f.must(apply, args...)
			got := f.check(op)
			f.must(restore, args...)
			if got != want || f.check(op) != "" {
				t.Errorf("%s: Check = %q want %q", name, got, want)
			}
		}
		mutate("capi disabled", "capi_disabled", `UPDATE ads.store_settings SET capi_enabled=false WHERE store_id=$1`, `UPDATE ads.store_settings SET capi_enabled=true WHERE store_id=$1`, f.store)
		other := f.uuid()
		f.must(`INSERT INTO integration.bindings(id,tenant_id,store_id,principal_id,provider,external_asset_id) VALUES($1,$2,$3,$4,'meta_dataset','7002')`, other, f.tenant, f.store, f.principal)
		f.must(`UPDATE ads.store_settings SET capi_dataset_binding=$2 WHERE store_id=$1`, f.store, other)
		if got := f.check(op); got != "dataset_binding_changed" {
			t.Errorf("binding changed = %q", got)
		}
		f.must(`UPDATE ads.store_settings SET capi_dataset_binding=$2 WHERE store_id=$1`, f.store, f.binding)
		f.must(`DELETE FROM identity.store_grants WHERE store_id=$1 AND permission='ads:manage'`, f.store)
		if got := f.check(op); got != "capi_principal_revoked" {
			t.Errorf("principal revoked = %q", got)
		}
		f.must(`INSERT INTO identity.store_grants(tenant_id,store_id,principal_id,permission) VALUES($1,$2,$3,'ads:manage')`, f.tenant, f.store, f.principal)
		mutate("environment mismatch", "environment_mismatch", `UPDATE ads.store_settings SET environment='LIVE' WHERE store_id=$1`, `UPDATE ads.store_settings SET environment='SANDBOX' WHERE store_id=$1`, f.store)
		f.replica([]any{`UPDATE integration.operations SET request=jsonb_set(request,'{event_time}',to_jsonb(extract(epoch FROM now()-interval '7 days')::bigint)) WHERE id=$1`, op})
		if got := f.check(op); got != "event_too_old" {
			t.Errorf("event too old = %q", got)
		}
		// Billing never blocks CAPI (BD5): the Check has no billing branch (asserted by the plan above running as UNBILLED and
		// by the function body); the RESTRICTED read itself is MA02's.
	})

	t.Run("lease-fenced user data", func(t *testing.T) {
		f.t = t
		fresh := f.newBuyer()
		f.consent(fresh, true, ua)
		attempt := f.attempt(fresh, "SANDBOX", "SANDBOX", "1 minute")
		if err := f.plan(attempt); err != nil {
			t.Fatal(err)
		}
		op := f.opOf(attempt)
		token := []byte("0123456789abcdef0123456789abcdef")
		read := func(gen int64, tok []byte, id string) (map[string]any, error) {
			var ph *string
			var owner, contents, currency, source, agent string
			var value int64
			err := f.worker.QueryRow(f.ctx, `SELECT ph_e164,owner_id::text,contents::text,value_minor,currency,event_source_url,user_agent FROM ads.capi_user_data($1::uuid,$2::bigint,$3::bytea)`,
				id, gen, tok).Scan(&ph, &owner, &contents, &value, &currency, &source, &agent)
			if err != nil {
				return nil, err
			}
			return map[string]any{"ph": ph, "owner": owner, "contents": contents, "value": value, "currency": currency, "source": source, "agent": agent}, nil
		}
		// READY (never claimed): refused.
		if _, err := read(1, token, op); sqlState(err) != "40001" {
			t.Fatalf("READY op = %v", err)
		}
		sum := sha256.Sum256(token)
		f.must(`INSERT INTO integration.operation_events(tenant_id,store_id,operation_id,generation,state,mode,reason_code) VALUES($1,$2,$3,1,'DISPATCHING','dispatch','dispatch_claimed')`, f.tenant, f.store, op)
		f.must(`UPDATE integration.operations SET state='DISPATCHING',generation=1,lease_mode='dispatch',lease_until=now()+interval '30 seconds',lease_token_hash=$2 WHERE id=$1`, op, sum[:])
		got, err := read(1, token, op)
		if err != nil {
			t.Fatalf("fenced read: %v", err)
		}
		var lines []struct {
			ID       string `json:"id"`
			Quantity int    `json:"quantity"`
		}
		if json.Unmarshal([]byte(got["contents"].(string)), &lines) != nil || len(lines) != 1 || lines[0].ID != f.sku || lines[0].Quantity != 2 ||
			got["ph"] != (*string)(nil) || got["owner"] != fresh.owner || got["value"] != int64(250000) || got["currency"] != "TWD" ||
			got["source"] != f.origin+"/orders" || got["agent"] != ua {
			t.Fatalf("user data = %v", got)
		}
		for name, c := range map[string]struct {
			gen int64
			tok []byte
			id  string
			st  string
		}{
			"stale generation": {2, token, op, "40001"}, "wrong token": {1, []byte("fedcba9876543210fedcba9876543210"), op, "40001"},
			"other operation": {1, token, f.uuid(), "P0002"}, "short token": {1, token[:8], op, "22023"},
		} {
			if _, err := read(c.gen, c.tok, c.id); sqlState(err) != c.st {
				t.Errorf("%s = %v want %s", name, err, c.st)
			}
		}
		f.must(`UPDATE integration.operations SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, op)
		if _, err := read(1, token, op); sqlState(err) != "40001" {
			t.Errorf("expired lease = %v", err)
		}
		f.must(`UPDATE integration.operations SET lease_until=now()+interval '30 seconds',lease_mode='dispatch' WHERE id=$1`, op)
		// commerce_worker has no direct read of the destination (phone) table: the definer is the only way.
		if _, err := f.worker.Exec(f.ctx, `SELECT phone FROM storefront.destination_snapshots`); sqlState(err) != "42501" {
			t.Errorf("worker read of destination_snapshots = %v", err)
		}
		// Consent withdrawn and purged: the context is gone, so the definer returns no row (the route then denies, zero HTTP).
		f.consent(fresh, false, "")
		if _, err := f.worker.Exec(f.ctx, `SELECT ads.plan_capi_purge()`); err != nil {
			t.Fatal(err)
		}
		if _, err := read(1, token, op); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("after purge = %v", err)
		}
		if n := f.str(`SELECT count(*)::text FROM ads.capi_contexts WHERE owner_id=$1`, fresh.owner); n != "0" {
			t.Errorf("context survived the purge: %s", n)
		}
	})

	t.Run("feed", func(t *testing.T) {
		f.t = t
		serve := func(origin string) *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodGet, "/v1/buyer/feeds/meta.csv", nil)
			req.Header.Set(originHeader, origin)
			rec := httptest.NewRecorder()
			FeedHandler(f.buyerDB).ServeHTTP(rec, req)
			return rec
		}
		rec := serve(f.origin)
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/csv; charset=utf-8" || rec.Header().Get("Cache-Control") != "public, max-age=900" {
			t.Fatalf("feed = %d %v", rec.Code, rec.Header())
		}
		rows, err := csv.NewReader(rec.Body).ReadAll()
		if err != nil || len(rows) != 2 {
			t.Fatalf("csv = %v %v", rows, err)
		}
		want := []string{f.sku, `Tea "Set", boxed`, "Two cups\nand a pot", "in stock", "new", "2500.00 TWD", f.origin + "/products/" + f.product, "", "Capi Shop, Inc"}
		for i := range want {
			if rows[1][i] != want[i] {
				t.Errorf("col %s = %q want %q", feedColumns[i], rows[1][i], want[i])
			}
		}
		f.must(`UPDATE inventory.balances SET unavailable=on_hand WHERE store_id=$1`, f.store)
		rows, _ = csv.NewReader(serve(f.origin).Body).ReadAll()
		if len(rows) != 2 || rows[1][3] != "out of stock" {
			t.Errorf("sold out row = %v", rows)
		}
		f.must(`UPDATE catalog.products SET status='archived' WHERE id=$1`, f.product)
		if rows, _ = csv.NewReader(serve(f.origin).Body).ReadAll(); len(rows) != 1 {
			t.Errorf("archived product listed: %v", rows)
		}
		// Unknown (well-formed) host: 404; malformed origin: 422; unpublished store: 404.
		if c := serve("https://nobody.example.test").Code; c != 404 {
			t.Errorf("unknown host = %d", c)
		}
		if c := serve("http://capi-shop.example.test").Code; c != 422 {
			t.Errorf("http origin = %d", c)
		}
		f.must(`UPDATE control.storefront_publications SET published=false WHERE store_id=$1`, f.store)
		if c := serve(f.origin).Code; c != 404 {
			t.Errorf("unpublished = %d", c)
		}
		// The definer is the buyer runtime's only door; the table grants of 0080 are not reachable directly.
		if _, err := f.buyerDB.Exec(f.ctx, `SELECT * FROM catalog.skus`); sqlState(err) != "42501" {
			t.Errorf("buyer read of catalog.skus = %v", err)
		}
	})
}
