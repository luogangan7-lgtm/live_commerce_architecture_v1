package foundation_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"livecommerce/internal/buyer"
	"livecommerce/internal/domains"
)

// These control-plane facts are SYNTHETIC and inserted by the isolated owner.
// They exercise admission only, never DNS/TLS verification or public checkout.
func publishedFixture(t *testing.T, f *testFixture, tenant, store, origin string) string {
	t.Helper()
	mustExec(t, f.owner, `INSERT INTO control.storefront_publications(tenant_id,store_id,published) VALUES($1,$2,true)`, tenant, store)
	var id string
	err := f.owner.QueryRow(context.Background(), `INSERT INTO control.storefront_domains
		(tenant_id,store_id,origin,state,ownership_verified_at,tls_verified_at,valid_until,evidence_ref)
		VALUES($1,$2,$3,'ACTIVE',clock_timestamp()-interval '2 hours',clock_timestamp()-interval '2 hours',
		clock_timestamp()+interval '1 hour','SYNTHETIC_LOCAL_TEST_NOT_PROVIDER_PROOF') RETURNING id::text`, tenant, store, origin).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func publishedResolver(t *testing.T, issuer *pgxpool.Pool) *domains.Resolver {
	t.Helper()
	r, err := domains.New(context.Background(), issuer)
	if err != nil {
		t.Fatalf("resolver constructor: %v", err)
	}
	return r
}

func TestPublishedStorefrontAdmissionAndRevocation(t *testing.T) {
	f := fixture(t)
	a := openBuyerTestPools(t, f)
	r := publishedResolver(t, a.issuer)
	tenant, store, _ := seedBuyerStores(t, f)
	origin := "https://shop-" + strings.ReplaceAll(store, "-", "") + ".test"
	ctx := context.Background()
	deny := func() {
		t.Helper()
		out, err := r.Resolve(ctx, origin)
		if !errors.Is(err, domains.ErrUnavailable) || out != (domains.Route{}) {
			t.Fatalf("ineligible origin: route=%+v err=%v", out, err)
		}
	}
	deny() // Active tenant/store alone is not publication.
	mustExec(t, f.owner, `INSERT INTO control.storefront_publications(tenant_id,store_id) VALUES($1,$2)`, tenant, store)
	mustExec(t, f.owner, `INSERT INTO control.storefront_domains(tenant_id,store_id,origin) VALUES($1,$2,$3)`, tenant, store, origin)
	deny()
	mustExec(t, f.owner, `UPDATE control.storefront_domains SET state='ACTIVE',ownership_verified_at=clock_timestamp()-interval '2 hours',
		tls_verified_at=clock_timestamp()-interval '2 hours',valid_until=clock_timestamp()+interval '1 hour',evidence_ref='SYNTHETIC' WHERE origin=$1`, origin)
	deny() // Verified domain still needs distinct publication consent.
	mustExec(t, f.owner, `UPDATE control.storefront_publications SET published=true WHERE store_id=$1`, store)
	assertActive := func() domains.Route {
		t.Helper()
		out, err := r.Resolve(ctx, origin)
		if err != nil || out.StoreID != store || out.Origin != origin || out.DomainID == "" || out.DomainVersion < 1 || out.PublicationVersion < 1 {
			t.Fatalf("active mapping: %+v err=%v", out, err)
		}
		return out
	}
	first := assertActive()
	before := countRows(t, f.owner, `SELECT count(*) FROM buyer.capability_sessions`)
	for _, state := range []string{"REQUESTED", "OWNERSHIP_PENDING", "TLS_PENDING", "SUSPENDED", "DETACHED"} {
		t.Run(state, func(t *testing.T) {
			mustExec(t, f.owner, `UPDATE control.storefront_domains SET state=$2,version=version+1 WHERE origin=$1`, origin, state)
			deny()
			mustExec(t, f.owner, `UPDATE control.storefront_domains SET state='ACTIVE',version=version+1 WHERE origin=$1`, origin)
			assertActive()
		})
	}
	for _, change := range []struct{ table, column, id string }{
		{"control.tenants", "active", tenant}, {"control.stores", "active", store},
		{"control.storefront_publications", "published", store},
	} {
		t.Run(change.table, func(t *testing.T) {
			key := "id"
			if change.column == "published" {
				key = "store_id"
			}
			stmt := "UPDATE " + change.table + " SET " + change.column + "=$2 WHERE " + key + "=$1"
			mustExec(t, f.owner, stmt, change.id, false)
			deny()
			mustExec(t, f.owner, stmt, change.id, true)
			assertActive()
		})
	}
	for _, times := range []string{
		`ownership_verified_at=clock_timestamp()+interval '30 minutes'`,
		`tls_verified_at=clock_timestamp()+interval '30 minutes'`,
		`valid_until=clock_timestamp()-interval '1 hour'`,
	} {
		mustExec(t, f.owner, `UPDATE control.storefront_domains SET `+times+` WHERE origin=$1`, origin)
		deny()
		mustExec(t, f.owner, `UPDATE control.storefront_domains SET ownership_verified_at=clock_timestamp()-interval '2 hours',
			tls_verified_at=clock_timestamp()-interval '2 hours',valid_until=clock_timestamp()+interval '1 hour' WHERE origin=$1`, origin)
		assertActive()
	}
	mustExec(t, f.owner, `UPDATE control.storefront_publications SET version=version+1 WHERE store_id=$1`, store)
	last := assertActive()
	if last.DomainID != first.DomainID || last.DomainVersion <= first.DomainVersion || last.PublicationVersion != 2 {
		t.Fatal("resolver cached previous committed revision")
	}
	if countRows(t, f.owner, `SELECT count(*) FROM buyer.capability_sessions`) != before {
		t.Fatal("lookup minted a buyer session")
	}
}

func TestPublishedStorefrontOriginGrammarParity(t *testing.T) {
	f := fixture(t)
	a := openBuyerTestPools(t, f)
	r := publishedResolver(t, a.issuer)
	ctx := context.Background()
	valid := []string{"https://a.b", "https://shop.example", "https://xn--bcher-kva.example", "https://a-1.b2.test", "https://" + strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)}
	invalid := []string{"", "http://shop.test", "HTTPS://shop.test", "https://Shop.test", "https://localhost", "https://127.0.0.1", "https://[::1]", "https://shop.test:443", "https://shop.test/", "https://shop.test?", "https://shop.test#", "https://shop.test.", "https://*.test", "https://user@shop.test", "https://shop_test.example", "https://-shop.test", "https://shop-.test", "https://shop..test", "https://商店.test", " https://shop.test", "https://shop.test\n", "https://shop.test\r", "https://shop.test\t", "https://shop.test\x00", "https://" + strings.Repeat("a", 64) + ".test", "https://" + strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 62)}
	invalid = append(invalid, "https://shop.localhost")
	for i, origin := range valid {
		t.Run(fmt.Sprintf("valid-%d", i), func(t *testing.T) {
			if _, err := r.Resolve(ctx, origin); !errors.Is(err, domains.ErrUnavailable) {
				t.Fatalf("Go grammar rejected canonical origin: %v", err)
			}
			var count int
			if err := a.issuer.QueryRow(ctx, `SELECT count(*) FROM buyer.resolve_published_store($1)`, origin).Scan(&count); err != nil || count != 0 {
				t.Fatalf("SQL grammar: count=%d err=%v", count, err)
			}
		})
	}
	for i, origin := range invalid {
		t.Run(fmt.Sprintf("invalid-%d", i), func(t *testing.T) {
			if _, err := r.Resolve(ctx, origin); !errors.Is(err, domains.ErrInvalid) {
				t.Fatalf("Go grammar accepted invalid origin: %v", err)
			}
			var count int
			err := a.issuer.QueryRow(ctx, `SELECT count(*) FROM buyer.resolve_published_store($1)`, origin).Scan(&count)
			// PostgreSQL rejects a NUL before the SQL function can receive text.
			code := "PT400"
			if strings.ContainsRune(origin, 0) {
				code = "22021"
			}
			requirePGCode(t, err, code, "direct SQL origin grammar")
		})
	}
	var count int
	requirePGCode(t, a.issuer.QueryRow(ctx, `SELECT count(*) FROM buyer.resolve_published_store(NULL)`).Scan(&count), "PT400", "NULL SQL origin")
}

func TestPublishedStorefrontAuthorityAndConstraints(t *testing.T) {
	f := fixture(t)
	a := openBuyerTestPools(t, f)
	ctx := context.Background()
	tenant, store, other := seedBuyerStores(t, f)
	origin := "https://acl-" + strings.ReplaceAll(store, "-", "") + ".test"
	id := publishedFixture(t, f, tenant, store, origin)
	for _, pool := range []*pgxpool.Pool{f.owner, f.runtime, a.runtime, a.identity} {
		if r, err := domains.New(ctx, pool); err == nil || r != nil {
			t.Fatal("constructor accepted wrong authority")
		}
	}
	for _, dsn := range []string{a.mixedURL, bcRole(t, f, "commerce_worker"), bcRole(t, f, "commerce_checkout_runtime"), bcRole(t, f, "commerce_auth")} {
		pool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			t.Fatal("fixture pool")
		}
		func() {
			defer pool.Close()
			if r, err := domains.New(ctx, pool); err == nil || r != nil {
				t.Fatal("constructor accepted mixed/non-issuer authority")
			}
		}()
	}
	for _, role := range []string{"commerce_runtime", "commerce_auth", "commerce_identity", "commerce_buyer_runtime", "commerce_checkout_runtime", "commerce_worker"} {
		t.Run(role, func(t *testing.T) {
			tx, err := f.owner.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err = tx.Exec(ctx, `SET LOCAL ROLE `+pgx.Identifier{role}.Sanitize()); err != nil {
				t.Fatal(err)
			}
			_, err = tx.Exec(ctx, `SELECT * FROM buyer.resolve_published_store($1)`, origin)
			requirePGCode(t, err, "42501", "non-issuer execute")
		})
	}
	for _, table := range []string{"control.storefront_publications", "control.storefront_domains"} {
		for _, role := range []string{"commerce_runtime", "commerce_identity", "commerce_buyer_runtime", "commerce_buyer_issuer", "commerce_checkout_runtime", "commerce_worker"} {
			var access bool
			if err := f.owner.QueryRow(ctx, `SELECT has_table_privilege($1,$2,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER') OR has_any_column_privilege($1,$2,'SELECT,INSERT,UPDATE,REFERENCES')`, role, table).Scan(&access); err != nil || access {
				t.Fatalf("unexpected direct table or column privilege: role=%s table=%s err=%v", role, table, err)
			}
		}
		// 0023 deliberately grants the non-login auth owner a precise SELECT
		// column set for the token-authenticated inverse reader. All other
		// column/table powers, and all application-role access above, stay denied.
		allowed := []string{"tenant_id", "store_id", "published"}
		if table == "control.storefront_domains" {
			allowed = []string{"tenant_id", "store_id", "origin", "state", "ownership_verified_at", "tls_verified_at", "valid_until"}
		}
		var exact bool
		if err := f.owner.QueryRow(ctx, `SELECT
			NOT has_table_privilege('commerce_auth',$1,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')
			AND NOT has_any_column_privilege('commerce_auth',$1,'INSERT,UPDATE,REFERENCES')
			AND (SELECT bool_and(has_column_privilege('commerce_auth',a.attrelid,a.attnum,'SELECT')=(a.attname=ANY($2::text[])))
			FROM pg_attribute a WHERE a.attrelid=$1::regclass AND a.attnum>0 AND NOT a.attisdropped)`, table, allowed).Scan(&exact); err != nil || !exact {
			t.Fatalf("auth owner column whitelist changed: table=%s err=%v", table, err)
		}
		for _, query := range []string{"SELECT * FROM " + table, "DELETE FROM " + table + " WHERE false", "UPDATE " + table + " SET version=version WHERE false"} {
			_, err := a.issuer.Exec(ctx, query)
			requirePGCode(t, err, "42501", "issuer direct table access")
		}
	}
	// commerce_ads_writer: meta-ads-v1 §4.4 (0080) — ads.feed_rows resolves the store from the verified origin through this
	// function only; the ACL stays a closed list.
	var safe bool
	err := f.owner.QueryRow(ctx, `SELECT p.prosecdef AND p.proowner='commerce_buyer_writer'::regrole
		AND p.proconfig=ARRAY['search_path=pg_catalog']::text[] AND NOT EXISTS (
		 SELECT 1 FROM aclexplode(p.proacl) a WHERE a.privilege_type='EXECUTE'
		 AND a.grantee NOT IN ('commerce_buyer_writer'::regrole,'commerce_buyer_issuer'::regrole,'commerce_ads_writer'::regrole))
		FROM pg_proc p WHERE p.oid='buyer.resolve_published_store(text)'::regprocedure`).Scan(&safe)
	if err != nil || !safe {
		t.Fatalf("function owner/ACL/search_path: %v", err)
	}
	for _, table := range []string{"control.storefront_publications", "control.storefront_domains"} {
		if err = f.owner.QueryRow(ctx, `SELECT relrowsecurity AND relforcerowsecurity FROM pg_class WHERE oid=$1::regclass`, table).Scan(&safe); err != nil || !safe {
			t.Fatal("missing forced RLS")
		}
	}
	_, err = f.owner.Exec(ctx, `INSERT INTO control.storefront_domains(tenant_id,store_id,origin) VALUES($1,$2,$3)`, tenant, other, origin)
	requirePGCode(t, err, "23505", "globally unique origin")
	_, err = f.owner.Exec(ctx, `INSERT INTO control.storefront_domains(tenant_id,store_id,origin) VALUES($1,$2,'https://cross-owner.test')`, f.tenantB, store)
	requirePGCode(t, err, "23503", "composite tenant/store FK")
	for _, change := range []string{"version=0", "ownership_verified_at=NULL", "tls_verified_at=NULL", "valid_until=NULL", "evidence_ref=NULL", "evidence_ref=' '", "valid_until='infinity'", "ownership_verified_at='-infinity'", "tls_verified_at='infinity'", "origin='https://untrusted.test/'", "state='VERIFIED'"} {
		_, err = f.owner.Exec(ctx, `UPDATE control.storefront_domains SET `+change+` WHERE id=$1`, id)
		requirePGCode(t, err, "23514", "domain fact constraint")
	}
	_, err = f.owner.Exec(ctx, `UPDATE control.storefront_domains SET evidence_ref=$2 WHERE id=$1`, id, "\t\n")
	requirePGCode(t, err, "23514", "blank control-character evidence")
	// A domain points to a store, not to a customer. Rebinding this SYNTHETIC
	// fact cannot promote an old-store credential into new-store authority.
	issuer, err := buyer.New(a.issuer, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	capability := mustIssue(t, issuer, store)
	mustExec(t, f.owner, `INSERT INTO control.storefront_publications(tenant_id,store_id,published) VALUES($1,$2,true)`, tenant, other)
	mustExec(t, f.owner, `UPDATE control.storefront_domains SET store_id=$2,version=version+1 WHERE id=$1`, id, other)
	route, err := publishedResolver(t, a.issuer).Resolve(ctx, origin)
	if err != nil || route.StoreID != other {
		t.Fatal("synthetic remap not visible")
	}
	called := false
	err = buyer.WithScope(ctx, a.runtime, capability.Token, route.StoreID, func(context.Context, pgx.Tx, buyer.Scope) error { called = true; return nil })
	if !errors.Is(err, buyer.ErrUnauthorized) || called {
		t.Fatal("domain remap bypassed buyer store authority")
	}
}

func TestPublishedStorefrontDeadlinesAndSanitization(t *testing.T) {
	f := fixture(t)
	a := openBuyerTestPools(t, f)
	r := publishedResolver(t, a.issuer)
	tenant, store, _ := seedBuyerStores(t, f)
	origin := "https://errors-" + strings.ReplaceAll(store, "-", "") + ".test"
	publishedFixture(t, f, tenant, store, origin)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Resolve(ctx, origin); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	holder, err := f.owner.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(context.Background())
	if _, err = holder.Exec(context.Background(), `LOCK control.storefront_domains IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	deadline, stop := context.WithTimeout(context.Background(), 150*time.Millisecond)
	_, err = r.Resolve(deadline, origin)
	stop()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked lookup ignored deadline: %v", err)
	}
	if err = holder.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Resolve(context.Background(), origin); err != nil {
		t.Fatalf("lookup did not recover after cancellation: %v", err)
	}
	a.issuer.Close()
	if _, err = r.Resolve(context.Background(), origin); !errors.Is(err, domains.ErrDatabase) || err.Error() != domains.ErrDatabase.Error() {
		t.Fatalf("driver error was not sanitized: %v", err)
	}
	if _, err = r.Resolve(context.Background(), "http://invalid.test"); !errors.Is(err, domains.ErrInvalid) {
		t.Fatal("invalid input reached closed DB")
	}
}

func TestPublishedStorefrontFinalClockAfterObservedLock(t *testing.T) {
	f := fixture(t)
	a := openBuyerTestPools(t, f)
	r := publishedResolver(t, a.issuer)
	tenant, store, _ := seedBuyerStores(t, f)
	origin := "https://expiry-" + strings.ReplaceAll(store, "-", "") + ".test"
	publishedFixture(t, f, tenant, store, origin)
	ctx := context.Background()
	mustExec(t, f.owner, `UPDATE control.storefront_domains SET valid_until=clock_timestamp()+interval '500 milliseconds' WHERE origin=$1`, origin)
	if _, err := r.Resolve(ctx, origin); err != nil {
		t.Fatalf("short proof not initially eligible: %v", err)
	}
	holder, err := f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if _, err = holder.Exec(ctx, `LOCK control.storefront_domains IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := r.Resolve(ctx, origin); done <- err }()
	until := time.Now().Add(2 * time.Second)
	for {
		var waiting bool
		if err = f.owner.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE usename=$1 AND wait_event_type='Lock' AND query LIKE '%buyer.resolve_published_store%')`, a.issuerRole).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(until) {
			t.Fatal("resolver never observed waiting on real database lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for {
		var expired bool
		if err = holder.QueryRow(ctx, `SELECT valid_until <= clock_timestamp() FROM control.storefront_domains WHERE origin=$1`, origin).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if expired {
			break
		}
		if time.Now().After(until) {
			t.Fatal("synthetic proof did not expire")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err = waitError(t, done); !errors.Is(err, domains.ErrUnavailable) {
		t.Fatalf("expired proof admitted after observed lock wait: %v", err)
	}
}
