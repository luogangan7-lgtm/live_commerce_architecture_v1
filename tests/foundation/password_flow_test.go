package foundation_test

// PA04 TestPasswordPA04Signup and PA05 TestPasswordPA05Login (REAL_PG + HTTP_PG) —
// contracts/merchant-password-auth-v1.md §2 (Sign-up, Login), §4.2 start_signup_challenge /
// password_login_material / record_password_failure / start_login_challenge, §4.3
// complete_email_challenge, §8, PD4, PD6, PD9, PD12 and the §9 PA04/PA05 rows (incl. round-2 R2-1:
// the 101st/150th wrong password). Tables: identity.{principals,password_credentials,
// email_challenges,sessions,session_events,auth_events,memberships}; functions: the §4.2 definers
// through internal/identity; HTTP through identityhttp.NewPasswordHandler. Owner-pool use
// (disclosed per test): fault-injection triggers, ageing an expiry, bumping password_version /
// deactivating a principal, membership toggles. Codes come from the mailtest mailbox only.

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"livecommerce/internal/identity"
)

// pwaFaultTrigger installs an owner-pool trigger that raises on INSERT (into `table`, optionally when
// `when` holds), so a definer's transaction fails after the earlier statements ran. Removed by the
// returned func (also on cleanup). Why the owner pool: the definers have no fault seam (auth-core build
// step 1), so faults are injected from outside, as the brief prescribes.
func (e *pwaEnv) faultTrigger(table, when string) func() {
	e.t.Helper()
	tag := strings.ToLower(pwaLetters(10))
	fn := "public.pwa_fault_" + tag
	e.exec(`CREATE FUNCTION ` + fn + `() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'pwa injected fault' USING ERRCODE = 'P0001'; END $$`)
	cond := ""
	if when != "" {
		cond = " WHEN (" + when + ")"
	}
	e.exec(`CREATE TRIGGER pwa_fault_` + tag + ` AFTER INSERT ON identity.` + table + ` FOR EACH ROW` + cond + ` EXECUTE FUNCTION ` + fn + `()`)
	done := false
	drop := func() {
		if done {
			return
		}
		done = true
		_, _ = e.f.owner.Exec(pwaBG, `DROP TRIGGER IF EXISTS pwa_fault_`+tag+` ON identity.`+table)
		_, _ = e.f.owner.Exec(pwaBG, `DROP FUNCTION IF EXISTS `+fn+`()`)
	}
	e.t.Cleanup(drop)
	return drop
}

func (e *pwaEnv) counts(email string) (principals, creds, sessions, sessEvents, authEvents int64) {
	e.t.Helper()
	principals = e.q1(`SELECT count(*) FROM identity.principals`)
	creds = e.q1(`SELECT count(*) FROM identity.password_credentials WHERE email=$1`, email)
	sessions = e.q1(`SELECT count(*) FROM identity.sessions`)
	sessEvents = e.q1(`SELECT count(*) FROM identity.session_events`)
	authEvents = e.q1(`SELECT count(*) FROM identity.auth_events WHERE action='signup.verified'`)
	return
}

func TestPasswordPA04Signup(t *testing.T) {
	e := newPwa(t)
	logs := pwaCaptureLogs(t)

	t.Run("happy_path_is_one_transaction", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		email, password := pwaEmail(), pwaSecret()
		before, _, sessBefore, evBefore, _ := e.counts(email)
		ip := pwaIP()
		c := e.signupFrom(ip, email, password)
		if c.Binding == "" || len(c.Binding) != 43 {
			t.Fatalf("binding %q: want 43 base64url characters (32 random bytes)", c.Binding)
		}
		if d := time.Until(c.ExpiresAt); d < 9*time.Minute || d > 10*time.Minute+5*time.Second {
			t.Errorf("step-1 expires_at is %s away, want about 10 minutes (PD4)", d)
		}
		// Step 1 alone creates no principal, credential or session (sign-up verifies the email first).
		if p, cr, s, _, _ := e.counts(email); p != before || cr != 0 || s != sessBefore {
			t.Fatalf("step 1 created rows: principals %d->%d credentials %d sessions %d->%d", before, p, cr, sessBefore, s)
		}
		var purpose string
		var attempts int
		var created, expires time.Time
		var pending *string
		if err := e.f.owner.QueryRow(pwaBG, `SELECT purpose, attempts, created_at, expires_at, pending_password_hash FROM identity.email_challenges WHERE email=$1`, email).Scan(&purpose, &attempts, &created, &expires, &pending); err != nil {
			t.Fatal(err)
		}
		if purpose != "signup" || attempts != 0 || pending == nil {
			t.Fatalf("challenge purpose=%s attempts=%d pending=%v", purpose, attempts, pending)
		}
		if d := expires.Sub(created); d < 10*time.Minute-2*time.Second || d > 10*time.Minute+2*time.Second {
			t.Errorf("stored expires_at - created_at = %s, want 10 minutes (§4.1)", d)
		}
		if ok, err := identity.VerifyPassword(*pending, password); err != nil || !ok {
			t.Errorf("pending hash does not verify the password: %v %v", ok, err)
		}
		code := e.code(email, 1)
		s, err := e.pw.Complete(pwaBG, ip, c.Binding, "signup", code, "")
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		p, cr, sess, ev, signupEvents := e.counts(email)
		if p != before+1 || cr != 1 || sess != sessBefore+1 || ev != evBefore+1 {
			t.Fatalf("after complete: principals %d->%d credentials %d sessions %d->%d session_events %d->%d", before, p, cr, sessBefore, sess, evBefore, ev)
		}
		if signupEvents < 1 {
			t.Error("no signup.verified audit event")
		}
		principal := e.principalOf(email)
		if got := e.sessionPrincipal(s.Token); got != principal {
			t.Errorf("session belongs to principal %s, credential to %s", got, principal)
		}
		if !e.sessionLive(s.Token) {
			t.Error("issued session is not live")
		}
		var version int64
		var failed int
		var verified *time.Time
		var phc string
		if err := e.f.owner.QueryRow(pwaBG, `SELECT password_version, failed_count, email_verified_at, password_hash FROM identity.password_credentials WHERE email=$1`, email).Scan(&version, &failed, &verified, &phc); err != nil {
			t.Fatal(err)
		}
		if version != 1 || failed != 0 || verified == nil {
			t.Errorf("credential version=%d failed=%d verified=%v", version, failed, verified)
		}
		if ok, err := identity.VerifyPassword(phc, password); err != nil || !ok {
			t.Errorf("stored hash does not verify: %v %v", ok, err)
		}
		if e.q1(`SELECT count(*) FROM identity.session_events WHERE principal_id=$1::uuid AND action='session.issued'`, principal) != 1 {
			t.Error("want exactly one session.issued event")
		}
		var audience string
		var sessExpires time.Time
		if err := e.f.owner.QueryRow(pwaBG, `SELECT audience, expires_at FROM identity.sessions WHERE principal_id=$1::uuid`, principal).Scan(&audience, &sessExpires); err != nil {
			t.Fatal(err)
		}
		if audience != "merchant" || time.Until(sessExpires) < 59*time.Minute || time.Until(sessExpires) > 61*time.Minute {
			t.Errorf("session audience=%s expires in %s (policy TTL is 1h)", audience, time.Until(sessExpires))
		}
		var nullPending bool
		if err := e.f.owner.QueryRow(pwaBG, `SELECT consumed_reason, pending_password_hash IS NULL FROM identity.email_challenges WHERE email=$1`, email).Scan(&purpose, &nullPending); err != nil {
			t.Fatal(err)
		}
		if purpose != "verified" || !nullPending {
			t.Errorf("consumed challenge: reason=%s pending hash cleared=%v (§4.1: NULL on consume)", purpose, nullPending)
		}
		e.canaryScanDB(map[string]string{"password": password, "code": "x" + code})
		var plain int64
		if err := e.f.owner.QueryRow(pwaBG, `SELECT count(*) FROM identity.email_challenges WHERE provider_message_id ~ $1`, "(^|[^0-9])"+code+"([^0-9]|$)").Scan(&plain); err != nil || plain != 0 {
			t.Errorf("code stored in provider_message_id: %d %v", plain, err)
		}
	})

	t.Run("fault_after_credential_insert_leaves_nothing", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		email, password := pwaEmail(), pwaSecret()
		ip := pwaIP()
		c := e.signupFrom(ip, email, password)
		code := e.code(email, 1)
		p0, _, s0, ev0, se0 := e.counts(email)
		drop := e.faultTrigger("password_credentials", "")
		pwaDisclose(t, "owner-pool AFTER INSERT trigger on identity.password_credentials raises, so the sign-up transaction fails after the credential insert")
		if _, err := e.pw.Complete(pwaBG, ip, c.Binding, "signup", code, ""); err == nil {
			t.Fatal("Complete succeeded although the transaction was sabotaged")
		}
		p1, cr, s1, ev1, se1 := e.counts(email)
		if p1 != p0 || cr != 0 || s1 != s0 || ev1 != ev0 || se1 != se0 {
			t.Fatalf("rows left behind: principals %d->%d credentials %d sessions %d->%d session_events %d->%d signup.verified %d->%d", p0, p1, cr, s0, s1, ev0, ev1, se0, se1)
		}
		drop()
		// The rollback also un-consumed the challenge: the same correct code now works.
		if _, err := e.pw.Complete(pwaBG, ip, c.Binding, "signup", code, ""); err != nil {
			t.Fatalf("challenge was burnt by the rolled-back transaction: %v", err)
		}
	})

	t.Run("fault_at_last_insert_leaves_nothing", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		email, password := pwaEmail(), pwaSecret()
		ip := pwaIP()
		c := e.signupFrom(ip, email, password)
		code := e.code(email, 1)
		p0, _, s0, ev0, se0 := e.counts(email)
		drop := e.faultTrigger("auth_events", "NEW.action='signup.verified'")
		pwaDisclose(t, "owner-pool trigger raises on the signup.verified audit insert, the last write of the sign-up transaction")
		if _, err := e.pw.Complete(pwaBG, ip, c.Binding, "signup", code, ""); err == nil {
			t.Fatal("Complete succeeded although the last insert was sabotaged")
		}
		p1, cr, s1, ev1, se1 := e.counts(email)
		if p1 != p0 || cr != 0 || s1 != s0 || ev1 != ev0 || se1 != se0 {
			t.Fatalf("rows left behind: principals %d->%d credentials %d sessions %d->%d session_events %d->%d", p0, p1, cr, s0, s1, ev0, ev1)
		}
		drop()
	})

	t.Run("taken_email_exists_no_row", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		email, password := pwaEmail(), pwaSecret()
		e.register(email, password)
		phc, err := identity.HashPassword(pwaSecret())
		if err != nil {
			t.Fatal(err)
		}
		var outcome string
		var exp *time.Time
		if err := e.pool.QueryRow(pwaBG, `SELECT outcome, expires_at FROM identity.start_signup_challenge($1::uuid,$2,$3,$4,$5,'en',$6)`, randomUUID(), email, phc, randomBytes(32), randomBytes(32), randomBytes(32)).Scan(&outcome, &exp); err != nil {
			t.Fatal(err)
		}
		if outcome != "EXISTS" {
			t.Errorf("start_signup_challenge for a taken email = %q, want EXISTS", outcome)
		}
		if n := e.q1(`SELECT count(*) FROM identity.email_challenges WHERE email=$1 AND purpose='signup' AND consumed_at IS NULL`, email); n != 0 {
			t.Errorf("EXISTS left %d open sign-up challenge rows", n)
		}
		// The Go path answers a challenge-shaped result, sends a code-free notice and has no row to complete.
		ip := pwaIP()
		c, err := e.pw.Signup(pwaBG, ip, email, pwaSecret(), "en")
		if err != nil {
			t.Fatalf("Signup of a taken email must answer like a new one (PD6): %v", err)
		}
		if len(c.Binding) != 43 {
			t.Errorf("taken-email binding %q has a different shape", c.Binding)
		}
		mails := e.awaitMails(email, 2)
		if pwaCodeRE.MatchString(mails[1].Text) {
			t.Error("the account-exists notice carries a 6-digit code")
		}
		if _, err := e.pw.Complete(pwaBG, ip, c.Binding, "signup", "123456", ""); !errors.Is(err, identity.ErrInvalidCode) {
			t.Errorf("Complete on the taken-email binding: %v, want ErrInvalidCode (indistinguishable from a wrong code)", err)
		}
		if n := e.q1(`SELECT count(*) FROM identity.password_credentials WHERE email=$1`, email); n != 1 {
			t.Errorf("credentials for the taken email = %d", n)
		}
	})

	t.Run("concurrent_signups_two_tx_witness", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		email := pwaEmail()
		// Two sources hold two open sign-up challenges for the same email (PD4 keeps both).
		ipA, ipB := pwaIP(), pwaIP()
		cA := e.signupFrom(ipA, email, pwaSecret())
		cB := e.signupFrom(ipB, email, pwaSecret())
		if e.q1(`SELECT count(*) FROM identity.email_challenges WHERE email=$1 AND purpose='signup' AND consumed_at IS NULL`, email) != 2 {
			t.Fatal("two-source sign-ups must leave two open challenges (PD4)")
		}
		_, _ = cA, cB
		bindA, codeA := e.challengeHashes(email, "signup", 0)
		bindB, codeB := e.challengeHashes(email, "signup", 1)
		var outA, outB string
		errA, errB := e.pwaWitness(
			func(ctx context.Context, tx pgx.Tx) (err error) {
				outA, err = sqlComplete(ctx, tx, bindA, codeA, "signup", nil)
				return
			},
			func(ctx context.Context, tx pgx.Tx) (err error) {
				outB, err = sqlComplete(ctx, tx, bindB, codeB, "signup", nil)
				return
			},
		)
		if errA != nil || errB != nil {
			t.Fatalf("witness errors: first=%v second=%v (a unique violation here means the email lock is missing)", errA, errB)
		}
		if outA != "SESSION" || outB != "EXISTS" {
			t.Errorf("outcomes first=%q second=%q, want SESSION then EXISTS", outA, outB)
		}
		if n := e.q1(`SELECT count(*) FROM identity.password_credentials WHERE email=$1`, email); n != 1 {
			t.Errorf("credentials for the email = %d, want 1", n)
		}
	})

	t.Run("five_wrong_codes_exhaust", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		email, password := pwaEmail(), pwaSecret()
		ip := pwaIP()
		c := e.signupFrom(ip, email, password)
		code := e.code(email, 1)
		for i := 1; i <= 5; i++ {
			if _, err := e.pw.Complete(pwaBG, ip, c.Binding, "signup", pwaWrongCode(code), ""); !errors.Is(err, identity.ErrInvalidCode) {
				t.Fatalf("wrong code %d: %v, want ErrInvalidCode", i, err)
			}
		}
		var attempts int
		var reason *string
		if err := e.f.owner.QueryRow(pwaBG, `SELECT attempts, consumed_reason FROM identity.email_challenges WHERE email=$1`, email).Scan(&attempts, &reason); err != nil {
			t.Fatal(err)
		}
		if attempts != 5 || reason == nil || *reason != "exhausted" {
			t.Errorf("after 5 wrong codes attempts=%d reason=%v, want 5/exhausted", attempts, reason)
		}
		// Even the correct code is now INVALID, and there is no principal (no signal beyond invalid_code).
		if _, err := e.pw.Complete(pwaBG, ip, c.Binding, "signup", code, ""); !errors.Is(err, identity.ErrInvalidCode) {
			t.Errorf("correct code on an exhausted challenge: %v, want ErrInvalidCode", err)
		}
		if n := e.q1(`SELECT count(*) FROM identity.password_credentials WHERE email=$1`, email); n != 0 {
			t.Errorf("exhausted challenge produced %d credentials", n)
		}
	})

	t.Run("expired_replay_and_wrong_binding", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		// expired
		email, password := pwaEmail(), pwaSecret()
		ip := pwaIP()
		c := e.signupFrom(ip, email, password)
		code := e.code(email, 1)
		pwaDisclose(t, "owner pool sets expires_at one second into the past (the clock cannot be moved)")
		e.exec(`UPDATE identity.email_challenges SET expires_at = now() - interval '1 second' WHERE email=$1`, email)
		if _, err := e.pw.Complete(pwaBG, ip, c.Binding, "signup", code, ""); !errors.Is(err, identity.ErrInvalidCode) {
			t.Errorf("expired challenge: %v, want ErrInvalidCode", err)
		}
		if n := e.q1(`SELECT count(*) FROM identity.password_credentials WHERE email=$1`, email); n != 0 {
			t.Errorf("expired challenge produced %d credentials", n)
		}
		// replay after success
		email2 := pwaEmail()
		ip2 := pwaIP()
		c2 := e.signupFrom(ip2, email2, pwaSecret())
		code2 := e.code(email2, 1)
		if _, err := e.pw.Complete(pwaBG, ip2, c2.Binding, "signup", code2, ""); err != nil {
			t.Fatal(err)
		}
		sessions := e.q1(`SELECT count(*) FROM identity.sessions`)
		if _, err := e.pw.Complete(pwaBG, ip2, c2.Binding, "signup", code2, ""); !errors.Is(err, identity.ErrInvalidCode) {
			t.Errorf("replayed correct code: %v, want ErrInvalidCode", err)
		}
		if e.q1(`SELECT count(*) FROM identity.sessions`) != sessions {
			t.Error("replay issued another session")
		}
		// wrong binding: a well-formed but unknown binding never touches the real challenge
		email3 := pwaEmail()
		ip3 := pwaIP()
		e.signupFrom(ip3, email3, pwaSecret())
		code3 := e.code(email3, 1)
		other := identity.Challenge{}
		other.Binding = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		if _, err := e.pw.Complete(pwaBG, ip3, other.Binding, "signup", code3, ""); !errors.Is(err, identity.ErrInvalidCode) {
			t.Errorf("unknown binding with a valid code: %v, want ErrInvalidCode", err)
		}
		if a := e.q1(`SELECT attempts FROM identity.email_challenges WHERE email=$1`, email3); a != 0 {
			t.Errorf("an unknown binding changed the real challenge's attempts to %d", a)
		}
		// the purpose is part of the binding: a signup binding cannot complete as login or reset
		c3, _ := e.pw.Signup(pwaBG, pwaIP(), pwaEmail(), pwaSecret(), "en")
		if _, err := e.pw.Complete(pwaBG, pwaIP(), c3.Binding, "login", "000000", ""); !errors.Is(err, identity.ErrInvalidCode) {
			t.Errorf("signup binding completed as login: %v", err)
		}
	})

	t.Run("newer_challenge_from_same_source_supersedes", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		pwaStableHour(t)
		email := pwaEmail()
		ip := pwaIP()
		c1 := e.signupFrom(ip, email, pwaSecret())
		pwaDisclose(t, "owner pool moves the 60 s email-mail-unauth window into the past so the same source can request again")
		e.elapse("email-mail-unauth", email+"‖"+netip.PrefixFrom(ip, 32).String(), "minute")
		c2 := e.signupFrom(ip, email, pwaSecret())
		code1, code2 := e.code(email, 1), e.code(email, 2)
		if _, err := e.pw.Complete(pwaBG, ip, c1.Binding, "signup", code1, ""); !errors.Is(err, identity.ErrInvalidCode) {
			t.Errorf("superseded challenge: %v, want ErrInvalidCode", err)
		}
		if n := e.q1(`SELECT count(*) FROM identity.email_challenges WHERE email=$1 AND consumed_reason='superseded'`, email); n != 1 {
			t.Errorf("superseded rows = %d, want 1", n)
		}
		if _, err := e.pw.Complete(pwaBG, ip, c2.Binding, "signup", code2, ""); err != nil {
			t.Errorf("newest challenge: %v", err)
		}
	})

	pwaNoCanary(t, "captured logs", logs.String(), map[string]string{"smtp secret": e.smtp.Password})
}

// ---------------------------------------------------------------------------------------------------

func (e *pwaEnv) wrongLogin(email string) error {
	_, err := e.pw.Login(pwaBG, pwaIP(), email, pwaSecret(), "en")
	return err
}

// disableByFailures drives the credential to disabled through the real login path: 100 wrong passwords,
// each from a fresh source (the per-source email-pw bucket is not what is under test here).
func (e *pwaEnv) disableByFailures(email string) {
	e.t.Helper()
	for i := 0; i < 100; i++ {
		if err := e.wrongLogin(email); !errors.Is(err, identity.ErrInvalidCredentials) {
			e.t.Fatalf("wrong password %d: %v, want ErrInvalidCredentials", i+1, err)
		}
	}
}

func (e *pwaEnv) credState(email string) (failed int, disabled bool, version int64) {
	e.t.Helper()
	if err := e.f.owner.QueryRow(pwaBG, `SELECT failed_count, disabled_at IS NOT NULL, password_version FROM identity.password_credentials WHERE email=$1`, email).Scan(&failed, &disabled, &version); err != nil {
		e.t.Fatal(err)
	}
	return
}

func TestPasswordPA05Login(t *testing.T) {
	e := newPwa(t)
	logs := pwaCaptureLogs(t)

	t.Run("happy_path_and_counter_reset", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		email, password := pwaEmail(), pwaSecret()
		e.register(email, password)
		for range 5 {
			if err := e.wrongLogin(email); !errors.Is(err, identity.ErrInvalidCredentials) {
				t.Fatal(err)
			}
		}
		if f, d, _ := e.credState(email); f != 5 || d {
			t.Fatalf("after 5 wrong passwords failed_count=%d disabled=%v", f, d)
		}
		ip := pwaIP()
		// mixed case and surrounding spaces normalize to the same credential
		c, err := e.pw.Login(pwaBG, ip, "  "+strings.ToUpper(email)+" ", password, "en")
		if err != nil {
			t.Fatalf("Login with an upper-cased, padded email: %v", err)
		}
		s, err := e.pw.Complete(pwaBG, ip, c.Binding, "login", e.code(email, 2), "")
		if err != nil {
			t.Fatalf("Complete login: %v", err)
		}
		if !e.sessionLive(s.Token) {
			t.Error("login session not live")
		}
		if f, d, _ := e.credState(email); f != 0 || d {
			t.Errorf("success did not reset the counter: failed_count=%d disabled=%v", f, d)
		}
		principal := e.principalOf(email)
		if e.sessionPrincipal(s.Token) != principal {
			t.Error("login session belongs to another principal")
		}
		if e.q1(`SELECT count(*) FROM identity.session_events WHERE principal_id=$1::uuid AND action='session.issued'`, principal) != 2 {
			t.Error("want session.issued for the sign-up and the login sessions")
		}
		// exactly one mail per login start (sign-up mail + login mail)
		if n := len(e.mailsTo(email)); n != 2 {
			t.Errorf("mails to the address = %d, want 2", n)
		}
	})

	t.Run("version_bound_and_inactive_principal", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		email, password := pwaEmail(), pwaSecret()
		e.register(email, password)
		ip := pwaIP()
		c, err := e.pw.Login(pwaBG, ip, email, password, "en")
		if err != nil {
			t.Fatal(err)
		}
		code := e.code(email, 2)
		pwaDisclose(t, "owner pool bumps password_version between login start and complete (a reset would also supersede the challenge, hiding the version check)")
		e.exec(`UPDATE identity.password_credentials SET password_version = password_version + 1 WHERE email=$1`, email)
		if _, err := e.pw.Complete(pwaBG, ip, c.Binding, "login", code, ""); !errors.Is(err, identity.ErrInvalidCode) {
			t.Errorf("login complete after a version change: %v, want ErrInvalidCode", err)
		}
		// inactive principal: complete refuses, and a fresh login answers like an unknown email without mail
		email2, password2 := pwaEmail(), pwaSecret()
		e.register(email2, password2)
		ip2 := pwaIP()
		c2, err := e.pw.Login(pwaBG, ip2, email2, password2, "en")
		if err != nil {
			t.Fatal(err)
		}
		code2 := e.code(email2, 2)
		pwaDisclose(t, "owner pool deactivates the principal (identity.principals.active=false), the operator runbook action")
		e.exec(`UPDATE identity.principals SET active=false WHERE id=$1::uuid`, e.principalOf(email2))
		if _, err := e.pw.Complete(pwaBG, ip2, c2.Binding, "login", code2, ""); !errors.Is(err, identity.ErrInvalidCode) {
			t.Errorf("complete for an inactive principal: %v, want ErrInvalidCode", err)
		}
		mails := len(e.mailsTo(email2))
		if _, err := e.pw.Login(pwaBG, pwaIP(), email2, password2, "en"); !errors.Is(err, identity.ErrInvalidCredentials) {
			t.Errorf("login of an inactive principal: %v, want ErrInvalidCredentials", err)
		}
		time.Sleep(300 * time.Millisecond)
		if len(e.mailsTo(email2)) != mails {
			t.Error("a login mail was sent for an inactive principal")
		}
	})

	t.Run("disabled_after_100_failures_and_101st_150th_are_401", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		email, password := pwaEmail(), pwaSecret()
		e.register(email, password)
		e.disableByFailures(email)
		if f, d, _ := e.credState(email); f != 100 || !d {
			t.Fatalf("after 100 wrong passwords failed_count=%d disabled=%v, want 100/true", f, d)
		}
		mails := len(e.mailsTo(email))
		// the correct password on a disabled credential is still 401 and sends nothing
		before := identity.ArgonCount()
		if _, err := e.pw.Login(pwaBG, pwaIP(), email, password, "en"); !errors.Is(err, identity.ErrInvalidCredentials) {
			t.Errorf("correct password on a disabled credential: %v, want ErrInvalidCredentials", err)
		}
		if identity.ArgonCount() <= before {
			t.Error("no Argon2 run for the disabled path with the correct password (PD6/A4: always verify)")
		}
		unknown := pwaEmail()
		// HTTP comparison: disabled+wrong (101st), disabled+right, unknown+wrong must look identical.
		reqOf := func(em, pw string) pwaResp {
			return e.post("login", pwaIP().String(), map[string]string{"email": em, "password": pw, "locale": "en"})
		}
		argon := identity.ArgonCount()
		r101 := reqOf(email, pwaSecret())
		if identity.ArgonCount() <= argon {
			t.Error("101st wrong password on the disabled credential did not run Argon2")
		}
		argon = identity.ArgonCount()
		rUnknown := reqOf(unknown, pwaSecret())
		if identity.ArgonCount() <= argon {
			t.Error("unknown email did not run Argon2 against the dummy PHC")
		}
		rRight := reqOf(email, password)
		for name, r := range map[string]pwaResp{"101st wrong": r101, "unknown": rUnknown, "disabled+correct": rRight} {
			if r.Status != 401 || r.code() != "invalid_credentials" {
				t.Errorf("%s: status %d code %q, want 401 invalid_credentials (never 5xx)", name, r.Status, r.code())
			}
		}
		if r101.shape() != rUnknown.shape() || rRight.shape() != rUnknown.shape() {
			t.Errorf("disabled and unknown answers differ:\n disabled-wrong %s\n disabled-right %s\n unknown        %s", r101.shape(), rRight.shape(), rUnknown.shape())
		}
		if f, _, _ := e.credState(email); f != 100 {
			t.Errorf("failed_count after the 101st = %d, want it to stay 100", f)
		}
		// ... through the 150th attempt
		var r150 pwaResp
		for i := 102; i <= 150; i++ {
			r150 = reqOf(email, pwaSecret())
			if r150.Status != 401 {
				t.Fatalf("attempt %d on a disabled credential: status %d (%s)", i, r150.Status, r150.code())
			}
		}
		if r150.shape() != rUnknown.shape() {
			t.Errorf("150th answer differs from unknown: %s vs %s", r150.shape(), rUnknown.shape())
		}
		if f, d, _ := e.credState(email); f != 100 || !d {
			t.Errorf("after 150 attempts failed_count=%d disabled=%v, want 100/true", f, d)
		}
		if len(e.mailsTo(email)) != mails {
			t.Error("a login mail was sent for a disabled credential")
		}
		if e.q1(`SELECT count(*) FROM identity.auth_events WHERE principal_id=$1::uuid AND action='login.disabled'`, e.principalOf(email)) < 1 {
			t.Error("no login.disabled audit event")
		}
	})

	t.Run("oidc_principal_is_never_merged_or_touched", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		oidcSession := identityLogin(t, e.oidc) // a real OIDC-created principal + session
		email, password := pwaEmail(), pwaSecret()
		s := e.register(email, password)
		if e.principalOf(email) == oidcSession.PrincipalID || e.sessionPrincipal(s.Token) == oidcSession.PrincipalID {
			t.Fatal("password principal merged into the OIDC principal (PD9)")
		}
		extBefore := e.q1(`SELECT count(*) FROM identity.external_identities WHERE principal_id=$1::uuid`, oidcSession.PrincipalID)
		// a full reset and a login, plus 3 failures on the password credential
		e.loginSession(email, password, 2)
		for range 3 {
			_ = e.wrongLogin(email)
		}
		if !e.sessionLive(oidcSession.Token) {
			t.Error("the OIDC principal's session was revoked or changed by password flows")
		}
		if n := e.q1(`SELECT count(*) FROM identity.password_credentials WHERE principal_id=$1::uuid`, oidcSession.PrincipalID); n != 0 {
			t.Errorf("OIDC principal gained %d password credentials", n)
		}
		if e.q1(`SELECT count(*) FROM identity.external_identities WHERE principal_id=$1::uuid`, oidcSession.PrincipalID) != extBefore {
			t.Error("OIDC external identity rows changed")
		}
		if n := e.q1(`SELECT count(*) FROM identity.external_identities WHERE principal_id=$1::uuid`, e.principalOf(email)); n != 0 {
			t.Errorf("password principal gained %d external identities", n)
		}
	})

	t.Run("has_membership_follows_the_real_membership", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		email, password := pwaEmail(), pwaSecret()
		s := e.register(email, password)
		hasMembership := func() bool {
			t.Helper()
			var version int64
			if err := e.f.owner.QueryRow(pwaBG, `SELECT password_version FROM identity.password_credentials WHERE email=$1`, email).Scan(&version); err != nil {
				t.Fatal(err)
			}
			var exp time.Time
			var has bool
			if err := e.pool.QueryRow(pwaBG, `SELECT expires_at, has_membership FROM identity.start_login_challenge($1::uuid,$2,$3,$4,$5,'en',$6)`, randomUUID(), email, version, randomBytes(32), randomBytes(32), randomBytes(32)).Scan(&exp, &has); err != nil {
				t.Fatalf("start_login_challenge: %v", err)
			}
			return has
		}
		if hasMembership() {
			t.Fatal("has_membership true before any store exists")
		}
		store := e.onboard(s) // the real /v1/identity/initial-store path (0065)
		if !hasMembership() {
			t.Fatal("has_membership false after initial-store")
		}
		pwaDisclose(t, "owner pool deactivates the membership, then the tenant (operator actions); each is restored")
		e.exec(`UPDATE identity.memberships SET active=false WHERE principal_id=$1::uuid`, e.principalOf(email))
		if hasMembership() {
			t.Error("has_membership true with an inactive membership")
		}
		e.exec(`UPDATE identity.memberships SET active=true WHERE principal_id=$1::uuid`, e.principalOf(email))
		if !hasMembership() {
			t.Error("has_membership false after restoring the membership")
		}
		e.exec(`UPDATE control.tenants SET active=false WHERE id=$1::uuid`, store.TenantID)
		if hasMembership() {
			t.Error("has_membership true with an inactive tenant")
		}
		e.exec(`UPDATE control.tenants SET active=true WHERE id=$1::uuid`, store.TenantID)
	})

	pwaNoCanary(t, "captured logs", logs.String(), map[string]string{"smtp secret": e.smtp.Password})
}
