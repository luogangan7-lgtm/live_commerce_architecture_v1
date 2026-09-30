package foundation_test

// REAL_PG gate for the Stripe pool constructors (contracts/stripe-psp-v1.md
// §0.2 and §6.3). Written from the contract and the exported platform API only.
//
// Two halves, both required:
//   - positive controls: a correctly provisioned LOGIN (INHERIT TRUE, SET FALSE,
//     no ADMIN) must OPEN the pool and do its real job through it. Without these
//     a validator that rejects every login passes every negative test.
//   - negative matrix: each subtest first proves the clean login opens, then
//     applies exactly one extra authority and requires the fixed masked error.
//
// Every login/role/function is randomly named and dropped in t.Cleanup. The
// matrices mutate role graphs and PUBLIC ACLs, so they run on the isolated
// per-test PG container (pwIsolatedFixture), never on the shared fixture.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"livecommerce/internal/platform"
)

const (
	saIngressRole   = "commerce_stripe_ingress"
	saRegistrarRole = "commerce_payment_registrar"
	saIngressErr    = "stripe ingress database unavailable"
	saRegistrarErr  = "stripe registrar database unavailable"
)

type saLogin struct{ ident, password, dsn string }

type saOpener func(context.Context, string) (*pgxpool.Pool, error)

func saQuote(name string) string { return pgx.Identifier{name}.Sanitize() }

// saCleanupSQL runs owner SQL at cleanup; failure is reported, never fatal.
func saCleanupSQL(t *testing.T, f *testFixture, sql string) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := f.owner.Exec(context.Background(), sql); err != nil {
			t.Errorf("authority fixture cleanup %q: %v", sql, err)
		}
	})
}

// saDropRole removes a synthetic role and every privilege/object it holds.
func saDropRole(t *testing.T, f *testFixture, ident string) {
	t.Helper()
	t.Cleanup(func() {
		for _, q := range []string{`DROP OWNED BY ` + ident, `DROP ROLE ` + ident} {
			if _, err := f.owner.Exec(context.Background(), q); err != nil {
				t.Errorf("authority fixture cleanup %q: %v", q, err)
			}
		}
	})
}

// saNewLogin creates a random LOGIN with each membership granted as the
// production provisioning does: WITH INHERIT TRUE, SET FALSE.
func saNewLogin(t *testing.T, f *testFixture, members ...string) saLogin {
	t.Helper()
	name := "sa_" + t04Tag()
	l := saLogin{ident: saQuote(name), password: fmt.Sprintf("%x", randomBytes(24))}
	mustExec(t, f.owner, `CREATE ROLE `+l.ident+` LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION PASSWORD '`+l.password+`'`)
	saDropRole(t, f, l.ident)
	for _, m := range members {
		mustExec(t, f.owner, `GRANT `+saQuote(m)+` TO `+l.ident+` WITH INHERIT TRUE, SET FALSE`)
	}
	l.dsn = roleURL(t, f.databaseURL, name, l.password)
	return l
}

// saNewGroup creates a random NOLOGIN group, optionally granted to the login.
func saNewGroup(t *testing.T, f *testFixture, login saLogin, opts string) string {
	t.Helper()
	ident := saQuote("sa_grp_" + t04Tag())
	mustExec(t, f.owner, `CREATE ROLE `+ident+` NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION`)
	saDropRole(t, f, ident)
	mustExec(t, f.owner, `GRANT `+ident+` TO `+login.ident+` WITH `+opts)
	return ident
}

// saRegrant replaces a login's membership so the new options apply regardless
// of how the server merges an existing grant.
func saRegrant(t *testing.T, f *testFixture, role string, login saLogin, opts string) {
	t.Helper()
	mustExec(t, f.owner, `REVOKE `+saQuote(role)+` FROM `+login.ident)
	mustExec(t, f.owner, `GRANT `+saQuote(role)+` TO `+login.ident+` WITH `+opts)
}

// saAssertRejected requires the fixed masked error, with no DSN material.
func saAssertRejected(t *testing.T, l saLogin, err error, want, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s admitted", what)
	}
	if err.Error() != want {
		t.Fatalf("%s: error %q is not the fixed masked error %q", what, err.Error(), want)
	}
	if strings.Contains(err.Error(), l.password) {
		t.Fatalf("%s: error leaks the login password", what)
	}
}

type saCase struct {
	name  string
	apply func(t *testing.T, f *testFixture, l saLogin)
}

// saRunMatrix: clean login opens (and validates, when a borrowed-pool validator
// exists), then one mutation must flip both to the masked rejection.
func saRunMatrix(t *testing.T, f *testFixture, self, want string, open saOpener,
	validate func(context.Context, *pgxpool.Pool) error, cases []saCase) {
	t.Helper()
	ctx := context.Background()
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			l := saNewLogin(t, f, self)
			p, err := open(ctx, l.dsn)
			if err != nil {
				t.Fatalf("clean %s login denied before mutation: %v", self, err)
			}
			p.Close()
			if validate != nil {
				b, err := pgxpool.New(ctx, l.dsn)
				if err != nil {
					t.Fatal(err)
				}
				verr := validate(ctx, b)
				b.Close()
				if verr != nil {
					t.Fatalf("clean %s login denied by validator before mutation: %v", self, verr)
				}
			}
			c.apply(t, f, l)
			p, err = open(ctx, l.dsn)
			if p != nil {
				p.Close()
			}
			saAssertRejected(t, l, err, want, c.name+" (open)")
			if validate != nil {
				b, err := pgxpool.New(ctx, l.dsn)
				if err != nil {
					t.Fatal(err)
				}
				defer b.Close()
				saAssertRejected(t, l, validate(ctx, b), want, c.name+" (borrowed validate)")
				// Borrowed pools are the caller's: a failed validation must not close them.
				if _, err := b.Exec(ctx, `SELECT 1`); err != nil {
					t.Fatalf("failed validation closed the borrowed pool: %v", err)
				}
			}
		})
	}
}

// saCommerceRoles lists every commerce_* role except self, from the catalog,
// so a role added later is covered without editing this file.
func saCommerceRoles(t *testing.T, f *testFixture, self string, mustHave ...string) []string {
	t.Helper()
	rows, err := f.owner.Query(context.Background(), `SELECT rolname FROM pg_roles WHERE rolname LIKE 'commerce\_%' AND rolname<>$1 ORDER BY 1`, self)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		seen[r] = true
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, r := range mustHave {
		if !seen[r] {
			t.Fatalf("expected role %s missing from catalog; the matrix would silently skip it", r)
		}
	}
	return out
}

// saMatrix builds the negative matrix shared by both Stripe roles.
// crossFn is a catalog function of the OTHER Stripe role, for a direct EXECUTE row.
func saMatrix(t *testing.T, f *testFixture, self, other, crossFn string) []saCase {
	t.Helper()
	must := []string{"commerce_worker", "commerce_runtime", "commerce_integration_writer", "commerce_payment_registry_writer",
		"commerce_hosted_runtime", "commerce_meta_ingress", "commerce_meta_consumer", "commerce_meta_worker",
		"commerce_meta_registrar", "commerce_media_worker", "commerce_media_executor", other}
	var cases []saCase
	roles := saCommerceRoles(t, f, self, must...)
	roles = append(roles, "pg_read_all_data", "pg_write_all_data")
	for _, r := range roles {
		r := r
		cases = append(cases,
			saCase{"extra member " + r + " inherit", func(t *testing.T, f *testFixture, l saLogin) {
				mustExec(t, f.owner, `GRANT `+saQuote(r)+` TO `+l.ident+` WITH INHERIT TRUE, SET FALSE`)
			}},
			saCase{"extra member " + r + " set-only", func(t *testing.T, f *testFixture, l saLogin) {
				mustExec(t, f.owner, `GRANT `+saQuote(r)+` TO `+l.ident+` WITH INHERIT FALSE, SET TRUE`)
			}})
	}
	// §6.3 "in either direction": the Stripe group itself holding another
	// authority is inherited by every login in it.
	for _, r := range []string{"commerce_worker", "commerce_runtime", "commerce_integration_writer", "commerce_payment_registry_writer",
		"commerce_hosted_runtime", "commerce_meta_ingress", "commerce_media_worker", other} {
		r := r
		cases = append(cases, saCase{"reverse: " + r + " granted into " + self, func(t *testing.T, f *testFixture, l saLogin) {
			mustExec(t, f.owner, `GRANT `+saQuote(r)+` TO `+saQuote(self)+` WITH INHERIT TRUE, SET FALSE`)
			saCleanupSQL(t, f, `REVOKE `+saQuote(r)+` FROM `+saQuote(self))
		}})
	}
	cases = append(cases,
		saCase{"SET TRUE on own role", func(t *testing.T, f *testFixture, l saLogin) {
			saRegrant(t, f, self, l, `INHERIT TRUE, SET TRUE`)
		}},
		saCase{"ADMIN option on own role", func(t *testing.T, f *testFixture, l saLogin) {
			saRegrant(t, f, self, l, `ADMIN TRUE, INHERIT TRUE, SET FALSE`)
		}},
		saCase{"ADMIN on schema-owner role, no inherit no set", func(t *testing.T, f *testFixture, l saLogin) {
			var owner string
			if err := f.owner.QueryRow(context.Background(), `SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname='payments'`).Scan(&owner); err != nil {
				t.Fatal(err)
			}
			mustExec(t, f.owner, `GRANT `+saQuote(owner)+` TO `+l.ident+` WITH ADMIN TRUE, INHERIT FALSE, SET FALSE`)
		}},
		saCase{"ADMIN on table-owner role, no inherit no set", func(t *testing.T, f *testFixture, l saLogin) {
			ownerRole, owned := saQuote("sa_owner_"+t04Tag()), saQuote("sa_owned_"+t04Tag())
			mustExec(t, f.owner, `CREATE ROLE `+ownerRole+` NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION`)
			saDropRole(t, f, ownerRole)
			mustExec(t, f.owner, `CREATE TABLE public.`+owned+`(id integer)`)
			mustExec(t, f.owner, `ALTER TABLE public.`+owned+` OWNER TO `+ownerRole)
			mustExec(t, f.owner, `GRANT `+ownerRole+` TO `+l.ident+` WITH ADMIN TRUE, INHERIT FALSE, SET FALSE`)
		}},
		saCase{"inherited table-owner role", func(t *testing.T, f *testFixture, l saLogin) {
			ownerRole, owned := saQuote("sa_owner_"+t04Tag()), saQuote("sa_owned_"+t04Tag())
			mustExec(t, f.owner, `CREATE ROLE `+ownerRole+` NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION`)
			saDropRole(t, f, ownerRole)
			mustExec(t, f.owner, `CREATE TABLE public.`+owned+`(id integer)`)
			mustExec(t, f.owner, `ALTER TABLE public.`+owned+` OWNER TO `+ownerRole)
			mustExec(t, f.owner, `GRANT `+ownerRole+` TO `+l.ident+` WITH INHERIT TRUE, SET FALSE`)
		}},
		saCase{"BYPASSRLS", func(t *testing.T, f *testFixture, l saLogin) {
			mustExec(t, f.owner, `ALTER ROLE `+l.ident+` BYPASSRLS`)
		}},
		saCase{"CREATEROLE", func(t *testing.T, f *testFixture, l saLogin) {
			mustExec(t, f.owner, `ALTER ROLE `+l.ident+` CREATEROLE`)
		}},
		saCase{"direct table grant", func(t *testing.T, f *testFixture, l saLogin) {
			mustExec(t, f.owner, `GRANT USAGE ON SCHEMA integration TO `+l.ident)
			mustExec(t, f.owner, `GRANT SELECT ON integration.account_credentials TO `+l.ident)
		}},
		saCase{"direct column grant", func(t *testing.T, f *testFixture, l saLogin) {
			mustExec(t, f.owner, `GRANT USAGE ON SCHEMA integration TO `+l.ident)
			mustExec(t, f.owner, `GRANT UPDATE(ciphertext) ON integration.account_credentials TO `+l.ident)
		}},
		saCase{"direct EXECUTE on the other Stripe role's function", func(t *testing.T, f *testFixture, l saLogin) {
			mustExec(t, f.owner, `GRANT EXECUTE ON FUNCTION `+crossFn+` TO `+l.ident)
		}})
	for _, priv := range []string{"UPDATE", "DELETE"} {
		priv := priv
		cases = append(cases,
			saCase{"direct " + priv + " on river_payment.river_job", func(t *testing.T, f *testFixture, l saLogin) {
				mustExec(t, f.owner, `GRANT `+priv+` ON river_payment.river_job TO `+l.ident)
			}},
			saCase{"inherited " + priv + " on river_payment.river_job", func(t *testing.T, f *testFixture, l saLogin) {
				g := saNewGroup(t, f, l, `INHERIT TRUE, SET FALSE`)
				mustExec(t, f.owner, `GRANT `+priv+` ON river_payment.river_job TO `+g)
			}},
			saCase{"PUBLIC " + priv + " on river_payment.river_job", func(t *testing.T, f *testFixture, l saLogin) {
				mustExec(t, f.owner, `GRANT `+priv+` ON river_payment.river_job TO PUBLIC`)
				saCleanupSQL(t, f, `REVOKE `+priv+` ON river_payment.river_job FROM PUBLIC`)
			}})
	}
	// River InsertTx needs at most UPDATE(kind) (its ON CONFLICT DO UPDATE SET
	// kind=... clause); any other column write is state-machine authority.
	cases = append(cases,
		saCase{"direct UPDATE(state) on river_payment.river_job", func(t *testing.T, f *testFixture, l saLogin) {
			mustExec(t, f.owner, `GRANT UPDATE(state) ON river_payment.river_job TO `+l.ident)
		}},
		saCase{"inherited UPDATE(args) on river_payment.river_job", func(t *testing.T, f *testFixture, l saLogin) {
			g := saNewGroup(t, f, l, `INHERIT TRUE, SET FALSE`)
			mustExec(t, f.owner, `GRANT UPDATE(args) ON river_payment.river_job TO `+g)
		}})
	// A function created in a domain schema is EXECUTE-able by PUBLIC by default
	// (the migrations revoke it explicitly; a stray one must not be tolerated).
	for _, schema := range []string{"payments", "integration", "checkout"} {
		schema := schema
		probe := func(t *testing.T, f *testFixture) string {
			fn := schema + ".zz_authority_probe_" + t04Tag()
			mustExec(t, f.owner, `CREATE FUNCTION `+fn+`() RETURNS integer LANGUAGE sql AS 'SELECT 1'`)
			saCleanupSQL(t, f, `DROP FUNCTION `+fn+`()`)
			return fn
		}
		cases = append(cases,
			saCase{"PUBLIC EXECUTE on non-catalog function in " + schema, func(t *testing.T, f *testFixture, l saLogin) {
				probe(t, f) // default ACL already grants PUBLIC
			}},
			saCase{"direct EXECUTE on non-catalog function in " + schema, func(t *testing.T, f *testFixture, l saLogin) {
				fn := probe(t, f)
				mustExec(t, f.owner, `REVOKE ALL ON FUNCTION `+fn+`() FROM PUBLIC`)
				mustExec(t, f.owner, `GRANT USAGE ON SCHEMA `+schema+` TO `+l.ident)
				mustExec(t, f.owner, `GRANT EXECUTE ON FUNCTION `+fn+`() TO `+l.ident)
			}},
			saCase{"inherited EXECUTE on non-catalog function in " + schema, func(t *testing.T, f *testFixture, l saLogin) {
				fn := probe(t, f)
				mustExec(t, f.owner, `REVOKE ALL ON FUNCTION `+fn+`() FROM PUBLIC`)
				g := saNewGroup(t, f, l, `INHERIT TRUE, SET FALSE`)
				mustExec(t, f.owner, `GRANT USAGE ON SCHEMA `+schema+` TO `+g)
				mustExec(t, f.owner, `GRANT EXECUTE ON FUNCTION `+fn+`() TO `+g)
			}})
	}
	return cases
}

// saWrongRoleOnly: a login holding only some other authority must not open.
func saWrongRoleOnly(t *testing.T, f *testFixture, other, want string, open saOpener) {
	t.Helper()
	for _, role := range []string{other, "commerce_runtime", "commerce_worker", "commerce_integration_writer",
		"commerce_payment_registry_writer", "commerce_hosted_runtime", "commerce_meta_ingress", "postgres"} {
		role := role
		t.Run("only "+role, func(t *testing.T) {
			dsn := f.databaseURL // the fixture owner itself
			if role != "postgres" {
				dsn = saNewLogin(t, f, role).dsn
			}
			p, err := open(context.Background(), dsn)
			if p != nil {
				p.Close()
			}
			if err == nil || err.Error() != want {
				t.Fatalf("login holding only %s: err=%v want %q", role, err, want)
			}
		})
	}
	t.Run("bad password is masked", func(t *testing.T) {
		l := saNewLogin(t, f, "commerce_runtime")
		bad := strings.Replace(l.dsn, l.password, "wrong"+l.password, 1)
		p, err := open(context.Background(), bad)
		if p != nil {
			p.Close()
		}
		if err == nil || err.Error() != want || strings.Contains(err.Error(), l.password) {
			t.Fatalf("connection failure not masked: %v", err)
		}
	})
}

// ---------------------------------------------------------------- negatives

func TestStripeAuthorityIngressMatrix(t *testing.T) {
	f := pwIsolatedFixture(t)
	cases := saMatrix(t, f, saIngressRole, saRegistrarRole,
		`integration.rotate_stripe_key(uuid,uuid,uuid,uuid,bigint,text,bytea,bytea)`)
	saRunMatrix(t, f, saIngressRole, saIngressErr, platform.OpenStripeIngressPool, platform.ValidateStripeIngressPool, cases)
	saWrongRoleOnly(t, f, saRegistrarRole, saIngressErr, platform.OpenStripeIngressPool)
}

func TestStripeAuthorityRegistrarMatrix(t *testing.T) {
	f := pwIsolatedFixture(t)
	cases := saMatrix(t, f, saRegistrarRole, saIngressRole, `payments.stripe_webhook_commit(uuid,uuid,bigint)`)
	saRunMatrix(t, f, saRegistrarRole, saRegistrarErr, platform.OpenStripeRegistrarPool, nil, cases)
	saWrongRoleOnly(t, f, saIngressRole, saRegistrarErr, platform.OpenStripeRegistrarPool)
}

// Existing pool modes must keep refusing a login that also holds a Stripe
// authority; otherwise Stripe custody leaks into an ordinary runtime.
func TestStripeAuthorityLegacyPoolsRejectStripeRoles(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	for _, o := range []struct {
		name, role string
		open       saOpener
	}{
		{"OpenPool", "commerce_runtime", platform.OpenPool},
		{"OpenIdentityPool", "commerce_identity", platform.OpenIdentityPool},
		{"OpenBuyerPool", "commerce_buyer_runtime", platform.OpenBuyerPool},
		{"OpenCheckoutPool", "commerce_checkout_runtime", platform.OpenCheckoutPool},
		{"OpenHostedPool", "commerce_hosted_runtime", platform.OpenHostedPool},
		{"OpenBuyerIssuerPool", "commerce_buyer_issuer", platform.OpenBuyerIssuerPool},
		{"OpenWorkerPool", "commerce_worker", platform.OpenWorkerPool},
		{"OpenMetaIngressPool", "commerce_meta_ingress", platform.OpenMetaIngressPool},
		{"OpenMetaConsumerPool", "commerce_meta_consumer", platform.OpenMetaConsumerPool},
		{"OpenMetaWorkerPool", "commerce_meta_worker", platform.OpenMetaWorkerPool},
		{"OpenMediaWorkerPool", "commerce_media_worker", platform.OpenMediaWorkerPool},
		{"OpenMediaExecutorPool", "commerce_media_executor", platform.OpenMediaExecutorPool},
		{"OpenMediaRecoveryPool", "commerce_media_recovery", platform.OpenMediaRecoveryPool},
	} {
		for _, stripe := range []string{saIngressRole, saRegistrarRole} {
			o, stripe := o, stripe
			t.Run(o.name+" plus "+stripe, func(t *testing.T) {
				clean := saNewLogin(t, f, o.role)
				p, err := o.open(ctx, clean.dsn)
				if err != nil {
					t.Fatalf("clean %s login denied by %s: %v", o.role, o.name, err)
				}
				p.Close()
				mixed := saNewLogin(t, f, o.role, stripe)
				p, err = o.open(ctx, mixed.dsn)
				if err == nil {
					p.Close()
					t.Fatalf("%s admitted a login that also holds %s", o.name, stripe)
				}
			})
		}
	}
}

// ---------------------------------------------------------------- positives

// A correctly provisioned ingress login opens the pool and performs the whole
// §6.4 ingress transaction: prepare, River InsertTx of payment_signal_v1, commit.
func TestStripeAuthorityIngressPositive(t *testing.T) {
	h := sslStartStripe(t) // fixture provisioning only; uses its own registrar login
	f := h.p.f
	ctx := context.Background()
	endpoint := randomUUID()
	var endpointVersion int64
	if err := h.registrar.QueryRow(ctx, `SELECT payments.set_stripe_webhook_endpoint(
	 $1::uuid,$2::uuid,$3::uuid,$4::uuid,$5::uuid,'PROVIDER_MOCK',0,true,'fixture_signing_key',$6::bytea,$7::bytea)`,
		f.tenantA, f.storeA1, f.principalA, h.connection, endpoint, randomBytes(12), randomBytes(48)).Scan(&endpointVersion); err != nil || endpointVersion != 1 {
		t.Fatalf("provision webhook endpoint: version=%d err=%v", endpointVersion, err)
	}

	login := saNewLogin(t, f, saIngressRole)
	pool, err := platform.OpenStripeIngressPool(ctx, login.dsn)
	if err != nil {
		t.Fatalf("correct ingress login (INHERIT TRUE, SET FALSE) rejected: %v", err)
	}
	defer pool.Close()
	if err := platform.ValidateStripeIngressPool(ctx, pool); err != nil {
		t.Fatalf("correct ingress pool rejected by validator: %v", err)
	}

	var account, profile string
	var keyVersion int64
	if err := pool.QueryRow(ctx, `SELECT account_id,execution_profile,key_version FROM payments.stripe_webhook_material($1::uuid)`, endpoint).Scan(&account, &profile, &keyVersion); err != nil || profile != "PROVIDER_MOCK" || keyVersion != 1 {
		t.Fatalf("ingress cannot read its endpoint material: profile=%q v=%d err=%v", profile, keyVersion, err)
	}

	jobs, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: "river_payment"})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var disposition, receipt, signal string
	if err := tx.QueryRow(ctx, `SELECT disposition,receipt_id::text,signal_id::text FROM payments.stripe_webhook_prepare(
	 $1::uuid,1,$2,'checkout.session.completed',$3::bigint,'2026-08-26.dahlia',
	 'checkout.session',$4,$5,$5,false,false,false,false,$6::bytea,$3::bigint)`,
		endpoint, "evt_authority_"+t04Tag(), time.Now().Unix(), "cs_test_"+t04Tag(), h.attempt, randomBytes(32)).Scan(&disposition, &receipt, &signal); err != nil {
		t.Fatalf("prepare through ingress pool: %v", err)
	}
	if disposition != "ACCEPT_PENDING" || receipt == "" || signal == "" {
		t.Fatalf("prepare disposition=%q receipt=%q signal=%q", disposition, receipt, signal)
	}
	job, err := jobs.InsertTx(ctx, tx, sslSignalArgs{OperationID: h.attempt, SignalID: signal, Version: 1},
		&river.InsertOpts{Queue: "payment_mock_v1"})
	if err != nil {
		t.Fatalf("River InsertTx payment_signal_v1 through ingress pool: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT payments.stripe_webhook_commit($1::uuid,$2::uuid,$3::bigint)`, receipt, signal, job.Job.ID); err != nil {
		t.Fatalf("commit through ingress pool: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("ingress transaction commit: %v", err)
	}
	var linked int
	if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM payments.stripe_signals s JOIN payments.stripe_webhook_receipts r ON r.signal_id=s.id
	 WHERE s.id=$1::uuid AND s.job_id=$2 AND r.id=$3::uuid AND r.disposition='ACCEPTED'`, signal, job.Job.ID, receipt).Scan(&linked); err != nil || linked != 1 {
		t.Fatalf("signal/receipt/job not linked after ingress commit: n=%d err=%v", linked, err)
	}

	// Effective authority stays insert-only and Stripe-webhook-only.
	for name, q := range map[string]string{
		"river_job UPDATE":   `UPDATE river_payment.river_job SET state='available' WHERE id=-1`,
		"river_job DELETE":   `DELETE FROM river_payment.river_job WHERE id=-1`,
		"stripe_sessions":    `SELECT session_id FROM payments.stripe_sessions LIMIT 1`,
		"registry function":  `SELECT integration.rotate_stripe_key(gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),1,'k',''::bytea,''::bytea)`,
		"credentials table":  `SELECT ciphertext FROM integration.account_credentials LIMIT 1`,
		"old river_job read": `SELECT id FROM river.river_job LIMIT 1`,
	} {
		if _, err := pool.Exec(ctx, q); !stripeSQLState(err, "42501") {
			t.Errorf("ingress %s: want 42501 got %v", name, err)
		}
	}
}

// A correctly provisioned registrar login opens the pool and runs the operator
// registry functions it is granted (register, qualify, set method, endpoint, rotate).
func TestStripeAuthorityRegistrarPositive(t *testing.T) {
	p := psSetup(t)
	f := p.f
	ctx := context.Background()
	login := saNewLogin(t, f, saRegistrarRole)
	pool, err := platform.OpenStripeRegistrarPool(ctx, login.dsn)
	if err != nil {
		t.Fatalf("correct registrar login (INHERIT TRUE, SET FALSE) rejected: %v", err)
	}
	defer pool.Close()

	connection, binding, proof, endpoint := randomUUID(), randomUUID(), randomUUID(), randomUUID()
	account := "acct_" + t04Tag()
	var registered string
	if err := pool.QueryRow(ctx, `SELECT integration.register_stripe_account(
	 $1::uuid,$2::uuid,$3::uuid,$4::uuid,$5::uuid,'SANDBOX',$6,'fixture_key',$7::bytea,$8::bytea)`,
		f.tenantA, f.storeA1, f.principalA, connection, binding, account, randomBytes(12), randomBytes(48)).Scan(&registered); err != nil || registered == "" {
		t.Fatalf("register_stripe_account through registrar pool: %v", err)
	}
	observed := time.Now().UTC().Add(-time.Second)
	var qualified string
	if err := pool.QueryRow(ctx, `SELECT payments.qualify_stripe_method(
	 $1::uuid,$2::uuid,$3::uuid,$4::uuid,$5::uuid,1,'PROVIDER_MOCK','synthetic authority probe',$6,$7)`,
		f.tenantA, f.storeA1, f.principalA, proof, connection, observed, observed.Add(time.Hour)).Scan(&qualified); err != nil || qualified == "" {
		t.Fatalf("qualify_stripe_method through registrar pool: %v", err)
	}
	var methodVersion int64
	if err := pool.QueryRow(ctx, `SELECT payments.set_stripe_method(
	 $1::uuid,$2::uuid,$3::uuid,$4::uuid,'TW',$5::uuid,$6::uuid,0,true,true,1,2500,99999900,'Stripe','Stripe','Stripe')`,
		f.tenantA, f.storeA1, f.principalA, p.market.ID, connection, proof).Scan(&methodVersion); err != nil || methodVersion != 1 {
		t.Fatalf("set_stripe_method through registrar pool: version=%d err=%v", methodVersion, err)
	}
	var endpointVersion int64
	if err := pool.QueryRow(ctx, `SELECT payments.set_stripe_webhook_endpoint(
	 $1::uuid,$2::uuid,$3::uuid,$4::uuid,$5::uuid,'PROVIDER_MOCK',0,true,'fixture_signing_key',$6::bytea,$7::bytea)`,
		f.tenantA, f.storeA1, f.principalA, connection, endpoint, randomBytes(12), randomBytes(48)).Scan(&endpointVersion); err != nil || endpointVersion != 1 {
		t.Fatalf("set_stripe_webhook_endpoint through registrar pool: version=%d err=%v", endpointVersion, err)
	}
	var credentialVersion int64
	if err := pool.QueryRow(ctx, `SELECT integration.rotate_stripe_key($1::uuid,$2::uuid,$3::uuid,$4::uuid,1,'fixture_key_v2',$5::bytea,$6::bytea)`,
		f.tenantA, f.storeA1, f.principalA, connection, randomBytes(12), randomBytes(48)).Scan(&credentialVersion); err != nil || credentialVersion != 2 {
		t.Fatalf("rotate_stripe_key through registrar pool: version=%d err=%v", credentialVersion, err)
	}

	// Effective authority stays "registry definers only".
	for name, q := range map[string]string{
		"credentials UPDATE": `UPDATE integration.account_credentials SET ciphertext='\x00' WHERE false`,
		"credentials read":   `SELECT ciphertext FROM integration.account_credentials LIMIT 1`,
		"river_job insert":   `INSERT INTO river_payment.river_job(kind,queue,args,max_attempts) VALUES('payment_signal_v1','payment_mock_v1','{}',1)`,
		"webhook function":   `SELECT payments.stripe_webhook_commit(gen_random_uuid(),gen_random_uuid(),1)`,
		"stripe_sessions":    `SELECT session_id FROM payments.stripe_sessions LIMIT 1`,
	} {
		if _, err := pool.Exec(ctx, q); !stripeSQLState(err, "42501") {
			t.Errorf("registrar %s: want 42501 got %v", name, err)
		}
	}
}
