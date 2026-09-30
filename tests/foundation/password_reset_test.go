package foundation_test

// PA06 TestPasswordPA06Reset (REAL_PG) — contracts/merchant-password-auth-v1.md §2 Reset, §4.2
// start_reset_challenge, §4.3 complete_email_challenge (reset branch), PD4 (supersede rules), PD5
// (reset-code budget per (principal, ip bucket) = 10, per principal = 50), PD10, §8 and the §9 PA06 row.
// Tables: identity.{email_challenges,password_credentials,sessions,session_events,principals}.
// Owner-pool use (disclosed per subtest): moving a 60 s throttle window into the past so one source
// can request twice, inserting a buyer-audience session, cloning an open challenge row for the
// concurrency witness. Codes are read from the mailtest mailbox. Two-tx witness = pg_blocking_pids.

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/jackc/pgx/v5"
	"livecommerce/internal/identity"
)

// resetFrom runs step 1 from ip, waits for the (background) mail and returns the challenge and its code.
func (e *pwaEnv) resetFrom(ip netip.Addr, email string) (identity.Challenge, string) {
	e.t.Helper()
	n := len(e.mailsTo(email)) + 1
	c, err := e.pw.Reset(pwaBG, ip, email, "en")
	if err != nil {
		e.t.Fatalf("Reset: %v", err)
	}
	return c, e.code(email, n)
}

func (e *pwaEnv) elapseMail(email string, ip netip.Addr) {
	e.t.Helper()
	e.elapse("email-mail-unauth", email+"‖"+netip.PrefixFrom(ip, 32).String(), "minute")
}

func (e *pwaEnv) resetComplete(ip netip.Addr, c identity.Challenge, code, newPassword string) (identity.Session, error) {
	return e.pw.Complete(pwaBG, ip, c.Binding, "reset", code, newPassword)
}

// attemptsByOrdinal reads attempts of the nth (0-based) reset challenge of the email.
func (e *pwaEnv) attemptsByOrdinal(email, purpose string, nth int) int64 {
	e.t.Helper()
	return e.q1(`SELECT attempts FROM identity.email_challenges WHERE email=$1 AND purpose=$2 ORDER BY created_at, id OFFSET $3 LIMIT 1`, email, purpose, nth)
}

func (e *pwaEnv) openCount(email, purpose string) int64 {
	e.t.Helper()
	return e.q1(`SELECT count(*) FROM identity.email_challenges WHERE email=$1 AND purpose=$2 AND consumed_at IS NULL`, email, purpose)
}

func TestPasswordPA06Reset(t *testing.T) {
	e := newPwa(t)
	pwaStableHour(t) // several subtests move 60 s windows; keep hour/day windows stable

	t.Run("reset_effects", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		email, oldPassword := pwaEmail(), pwaSecret()
		s1 := e.register(email, oldPassword)
		s2 := e.loginSession(email, oldPassword, 2)
		principal := e.principalOf(email)
		bystander := e.register(pwaEmail(), pwaSecret()) // another principal's merchant session must survive
		pwaDisclose(t, "owner pool inserts a 'buyer' audience session for the principal (no password flow can create one)")
		buyer := randomToken()
		e.exec(`INSERT INTO identity.sessions(id,token_hash,principal_id,audience,expires_at) VALUES ($1::uuid,$2,$3::uuid,'buyer',now()+interval '1 hour')`, randomUUID(), tokenHash(buyer), principal)
		// a disabled credential is re-enabled by a successful reset
		e.disableByFailures(email)
		if f, d, _ := e.credState(email); f != 100 || !d {
			t.Fatalf("setup: failed_count=%d disabled=%v", f, d)
		}
		// an open login challenge must die on reset: start one via SQL (the disabled credential cannot start one
		// through Login), so re-enable is verified after reset and the supersede check uses a second principal below.
		_, _, versionBefore := e.credState(email)
		ipR := pwaIP()
		c, code := e.resetFrom(ipR, email)
		newPassword := pwaSecret()
		ns, err := e.resetComplete(ipR, c, code, newPassword)
		if err != nil {
			t.Fatalf("reset complete: %v", err)
		}
		if e.sessionPrincipal(ns.Token) != principal || !e.sessionLive(ns.Token) {
			t.Errorf("reset must sign in: session principal match=%v live=%v", e.sessionPrincipal(ns.Token) == principal, e.sessionLive(ns.Token))
		}
		for name, tok := range map[string]string{"sign-up session": s1.Token, "login session": s2.Token} {
			if e.sessionLive(tok) {
				t.Errorf("%s survived the reset (PD10: every merchant session is revoked)", name)
			}
		}
		if !e.sessionLive(bystander.Token) {
			t.Error("another principal's session was revoked")
		}
		if n := e.q1(`SELECT count(*) FROM identity.sessions WHERE principal_id=$1::uuid AND audience='buyer' AND revoked_at IS NULL AND token_hash=$2`, principal, tokenHash(buyer)); n != 1 {
			t.Error("the buyer-audience session was revoked by a merchant password reset")
		}
		if n := e.q1(`SELECT count(*) FROM identity.session_events WHERE principal_id=$1::uuid AND action='session.revoked'`, principal); n != 2 {
			t.Errorf("session.revoked events = %d, want 2 (one per revoked merchant session)", n)
		}
		if n := e.q1(`SELECT count(*) FROM identity.session_events se JOIN identity.sessions s ON s.id=se.session_id WHERE se.principal_id=$1::uuid AND se.action='session.revoked' AND s.audience<>'merchant'`, principal); n != 0 {
			t.Errorf("revoked events for %d non-merchant sessions", n)
		}
		if n := e.q1(`SELECT count(*) FROM identity.session_events WHERE principal_id=$1::uuid AND action='session.issued'`, principal); n != 3 {
			t.Errorf("session.issued events = %d, want 3 (sign-up, login, reset)", n)
		}
		f, d, v := e.credState(email)
		if v != versionBefore+1 || f != 0 || d {
			t.Errorf("after reset version %d->%d failed_count=%d disabled=%v, want +1 / 0 / false", versionBefore, v, f, d)
		}
		var changed, created bool
		if err := e.f.owner.QueryRow(pwaBG, `SELECT password_changed_at > created_at, email_verified_at <= password_changed_at FROM identity.password_credentials WHERE email=$1`, email).Scan(&changed, &created); err != nil || !changed || !created {
			t.Errorf("password_changed_at not advanced: %v %v %v", changed, created, err)
		}
		// the new password logs in; the old one no longer does
		if _, err := e.pw.Login(pwaBG, pwaIP(), email, oldPassword, "en"); !errors.Is(err, identity.ErrInvalidCredentials) {
			t.Errorf("old password after reset: %v, want ErrInvalidCredentials", err)
		}
		n := len(e.mailsTo(email))
		pwaDisclose(t, "owner pool moves the 60 s email-mail-login window: the earlier login of this test used it")
		e.elapse("email-mail-login", email, "minute")
		lc, err := e.pw.Login(pwaBG, pwaIP(), email, newPassword, "en")
		if err != nil {
			t.Fatalf("new password login: %v", err)
		}
		if _, err := e.pw.Complete(pwaBG, pwaIP(), lc.Binding, "login", e.code(email, n+1), ""); err != nil {
			// login complete uses any source ip for throttling only
			t.Errorf("login complete with the new password: %v", err)
		}
		// replaying the reset code is INVALID
		if _, err := e.resetComplete(ipR, c, code, pwaSecret()); !errors.Is(err, identity.ErrInvalidCode) {
			t.Errorf("replayed reset code: %v, want ErrInvalidCode", err)
		}
	})

	t.Run("reset_supersedes_open_login_challenges", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		email, password := pwaEmail(), pwaSecret()
		e.register(email, password)
		ipL := pwaIP()
		lc, err := e.pw.Login(pwaBG, ipL, email, password, "en")
		if err != nil {
			t.Fatal(err)
		}
		loginCode := e.code(email, 2)
		ipR := pwaIP()
		c, code := e.resetFrom(ipR, email)
		if _, err := e.resetComplete(ipR, c, code, pwaSecret()); err != nil {
			t.Fatal(err)
		}
		if _, err := e.pw.Complete(pwaBG, ipL, lc.Binding, "login", loginCode, ""); !errors.Is(err, identity.ErrInvalidCode) {
			t.Errorf("login challenge opened before the reset: %v, want ErrInvalidCode", err)
		}
	})

	t.Run("unknown_and_inactive_email_give_NONE_and_no_row", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		unknown := pwaEmail()
		var outcome string
		var exp any
		if err := e.pool.QueryRow(pwaBG, `SELECT outcome, expires_at FROM identity.start_reset_challenge($1::uuid,$2,$3,$4,'en',$5)`, randomUUID(), unknown, randomBytes(32), randomBytes(32), randomBytes(32)).Scan(&outcome, &exp); err != nil {
			t.Fatal(err)
		}
		if outcome != "NONE" {
			t.Errorf("start_reset_challenge for an unknown email = %q, want NONE", outcome)
		}
		ip := pwaIP()
		c, err := e.pw.Reset(pwaBG, ip, unknown, "en")
		if err != nil || len(c.Binding) != 43 {
			t.Fatalf("Reset of an unknown email must answer like a known one (PD6): %v %q", err, c.Binding)
		}
		if _, err := e.resetComplete(ip, c, "123456", pwaSecret()); !errors.Is(err, identity.ErrInvalidCode) {
			t.Errorf("complete on an unknown-email binding: %v, want ErrInvalidCode", err)
		}
		if e.q1(`SELECT count(*) FROM identity.email_challenges WHERE email=$1`, unknown) != 0 {
			t.Error("a challenge row exists for an unknown email")
		}
		// inactive principal: same NONE, no mail
		email := pwaEmail()
		e.register(email, pwaSecret())
		pwaDisclose(t, "owner pool deactivates the principal (operator runbook action)")
		e.exec(`UPDATE identity.principals SET active=false WHERE id=$1::uuid`, e.principalOf(email))
		if err := e.pool.QueryRow(pwaBG, `SELECT outcome, expires_at FROM identity.start_reset_challenge($1::uuid,$2,$3,$4,'en',$5)`, randomUUID(), email, randomBytes(32), randomBytes(32), randomBytes(32)).Scan(&outcome, &exp); err != nil {
			t.Fatal(err)
		}
		if outcome != "NONE" {
			t.Errorf("start_reset_challenge for an inactive principal = %q, want NONE", outcome)
		}
	})

	t.Run("policy_errors_and_missing_new_password_do_not_burn_the_code", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		email := pwaEmail()
		e.register(email, pwaSecret())
		ip := pwaIP()
		c, code := e.resetFrom(ip, email)
		breached := pwaSecret()
		e.hibp.breach(breached, 9001)
		for name, pw := range map[string]string{"too_short": "short-1234", "too_long": pwaLetters(129), "breached": breached} {
			_, err := e.resetComplete(ip, c, code, pw)
			var pe identity.PolicyError
			if !errors.As(err, &pe) || pe.Reason != name {
				t.Errorf("new password %s: %v, want PolicyError{%s}", name, err, name)
			}
		}
		if _, err := e.resetComplete(ip, c, code, ""); err == nil {
			t.Error("reset complete without new_password succeeded")
		}
		if a := e.attemptsByOrdinal(email, "reset", 0); a != 0 {
			t.Errorf("policy failures changed attempts to %d (the code was never compared)", a)
		}
		if e.openCount(email, "reset") != 1 {
			t.Error("the challenge was consumed by a rejected request")
		}
		// new_password on a login challenge is a contract violation (only for reset)
		if _, err := e.pw.Complete(pwaBG, ip, c.Binding, "login", code, pwaSecret()); err == nil {
			t.Error("a login/binding mismatch with new_password succeeded")
		}
		if _, err := e.resetComplete(ip, c, code, pwaSecret()); err != nil {
			t.Errorf("the code still has to work after policy failures: %v", err)
		}
	})

	t.Run("budget_10_per_bucket_other_bucket_still_verifies", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		email := pwaEmail()
		e.register(email, pwaSecret())
		ipA, ipB := pwaIP(), pwaIP()
		wrongs := func(c identity.Challenge, code string, n int) {
			for i := 0; i < n; i++ {
				if _, err := e.resetComplete(ipA, c, pwaWrongCode(code), pwaSecret()); !errors.Is(err, identity.ErrInvalidCode) {
					t.Fatalf("wrong reset code: %v", err)
				}
			}
		}
		c1, code1 := e.resetFrom(ipA, email)
		wrongs(c1, code1, 5) // exhausts challenge 1 (attempts 5)
		pwaDisclose(t, "owner pool moves the 60 s email-mail-unauth window so source A can request again")
		e.elapseMail(email, ipA)
		c2, code2 := e.resetFrom(ipA, email)
		wrongs(c2, code2, 5) // 10 wrong codes in bucket A
		e.elapseMail(email, ipA)
		c3, code3 := e.resetFrom(ipA, email)
		// Source B's challenge is created while A's budget is used up.
		cB, codeB := e.resetFrom(ipB, email)
		attemptsBefore := e.q1(`SELECT sum(attempts) FROM identity.email_challenges WHERE email=$1 AND purpose='reset'`, email)
		// Over budget for bucket A: even the CORRECT code is INVALID and nothing is counted.
		if _, err := e.resetComplete(ipA, c3, code3, pwaSecret()); !errors.Is(err, identity.ErrInvalidCode) {
			t.Errorf("correct code from an over-budget bucket: %v, want ErrInvalidCode", err)
		}
		for range 3 {
			_, _ = e.resetComplete(ipA, c3, pwaWrongCode(code3), pwaSecret())
		}
		if got := e.q1(`SELECT sum(attempts) FROM identity.email_challenges WHERE email=$1 AND purpose='reset'`, email); got != attemptsBefore {
			t.Errorf("over-budget completes changed the attempt sum %d -> %d (§4.3 step 2: no compare, no increment)", attemptsBefore, got)
		}
		if e.attemptsByOrdinal(email, "reset", 2) != 0 {
			t.Error("the over-budget challenge's attempts moved")
		}
		// Another bucket still verifies (round-2 P1: one source cannot block the victim's reset).
		if _, err := e.resetComplete(ipB, cB, codeB, pwaSecret()); err != nil {
			t.Errorf("bucket B reset while A is over budget: %v", err)
		}
	})

	t.Run("budget_50_per_principal_blocks_every_bucket", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		email := pwaEmail()
		e.register(email, pwaSecret())
		// Ten sources, five wrong codes each: no bucket reaches 10, the principal reaches 50.
		for i := 0; i < 10; i++ {
			ip := pwaIP()
			c, code := e.resetFrom(ip, email)
			for j := 0; j < 5; j++ {
				if _, err := e.resetComplete(ip, c, pwaWrongCode(code), pwaSecret()); !errors.Is(err, identity.ErrInvalidCode) {
					t.Fatalf("source %d wrong code %d: %v", i, j, err)
				}
			}
		}
		if got := e.q1(`SELECT sum(attempts) FROM identity.email_challenges WHERE email=$1 AND purpose='reset'`, email); got != 50 {
			t.Fatalf("attempt sum %d, want 50", got)
		}
		ipK := pwaIP()
		c, code := e.resetFrom(ipK, email)
		if _, err := e.resetComplete(ipK, c, code, pwaSecret()); !errors.Is(err, identity.ErrInvalidCode) {
			t.Errorf("correct code once the principal budget (50) is spent: %v, want ErrInvalidCode", err)
		}
		if a := e.attemptsByOrdinal(email, "reset", 10); a != 0 {
			t.Errorf("the over-budget challenge attempts moved to %d", a)
		}
	})

	t.Run("concurrent_completes_two_tx_witness", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		email := pwaEmail()
		e.register(email, pwaSecret())
		ipA := pwaIP()
		wrong := func(c identity.Challenge, code string, n int) {
			for i := 0; i < n; i++ {
				_, _ = e.resetComplete(ipA, c, pwaWrongCode(code), pwaSecret())
			}
		}
		c1, code1 := e.resetFrom(ipA, email)
		wrong(c1, code1, 5)
		pwaDisclose(t, "owner pool moves the 60 s window; then clones the open challenge row so two open challenges share one ip bucket (the API supersedes, so it cannot create that state)")
		e.elapseMail(email, ipA)
		c2, code2 := e.resetFrom(ipA, email)
		wrong(c2, code2, 4) // bucket A has 9 wrong codes: one more is allowed, a second would exceed 10
		e.exec(`INSERT INTO identity.email_challenges(id,purpose,email,locale,principal_id,binding_hash,code_hmac,ip_hmac,expires_at)
		        SELECT gen_random_uuid(),purpose,email,locale,principal_id,$2,$3,ip_hmac,expires_at FROM identity.email_challenges WHERE email=$1 AND purpose='reset' AND consumed_at IS NULL LIMIT 1`, email, randomBytes(32), randomBytes(32))
		if e.openCount(email, "reset") != 2 {
			t.Fatal("setup must leave two open challenges in one ip bucket")
		}
		bind := func(nth int) []byte {
			var b []byte
			if err := e.f.owner.QueryRow(pwaBG, `SELECT binding_hash FROM identity.email_challenges WHERE email=$1 AND purpose='reset' AND consumed_at IS NULL ORDER BY created_at, id OFFSET $2 LIMIT 1`, email, nth).Scan(&b); err != nil {
				t.Fatal(err)
			}
			return b
		}
		b1, b2 := bind(0), bind(1)
		var o1, o2 string
		e1, e2 := e.pwaWitness(
			func(ctx context.Context, tx pgx.Tx) (err error) {
				o1, err = sqlComplete(ctx, tx, b1, randomBytes(32), "reset", strPtr(pwaValidPHC))
				return
			},
			func(ctx context.Context, tx pgx.Tx) (err error) {
				o2, err = sqlComplete(ctx, tx, b2, randomBytes(32), "reset", strPtr(pwaValidPHC))
				return
			},
		)
		if e1 != nil || e2 != nil || o1 != "INVALID" || o2 != "INVALID" {
			t.Fatalf("witness outcomes %q %q errors %v %v", o1, o2, e1, e2)
		}
		if got := e.q1(`SELECT sum(attempts) FROM identity.email_challenges WHERE email=$1 AND purpose='reset'`, email); got != 10 {
			t.Errorf("wrong-code attempts in one bucket = %d after two concurrent wrong completes, want exactly 10 (never above the budget)", got)
		}
	})

	t.Run("supersede_rules_reset", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		email := pwaEmail()
		e.register(email, pwaSecret())
		ipA, ipB, ipC, ipD := pwaIP(), pwaIP(), pwaIP(), pwaIP()
		// same source supersedes
		cA1, codeA1 := e.resetFrom(ipA, email)
		e.elapseMail(email, ipA)
		cA2, codeA2 := e.resetFrom(ipA, email)
		if _, err := e.resetComplete(ipA, cA1, codeA1, pwaSecret()); !errors.Is(err, identity.ErrInvalidCode) {
			t.Errorf("same-source superseded reset challenge: %v, want ErrInvalidCode", err)
		}
		if n := e.q1(`SELECT count(*) FROM identity.email_challenges WHERE email=$1 AND purpose='reset' AND consumed_reason='superseded'`, email); n != 1 {
			t.Errorf("superseded rows = %d, want 1", n)
		}
		// a different source keeps A's open challenge
		cB, codeB := e.resetFrom(ipB, email)
		if e.openCount(email, "reset") != 2 {
			t.Errorf("open reset challenges after a second source = %d, want 2 (cross-source keep)", e.openCount(email, "reset"))
		}
		// the 4th open challenge for email+purpose supersedes the oldest (A's)
		_, _ = e.resetFrom(ipC, email)
		if e.openCount(email, "reset") != 3 {
			t.Errorf("open after 3 sources = %d, want 3", e.openCount(email, "reset"))
		}
		_, _ = e.resetFrom(ipD, email)
		if e.openCount(email, "reset") != 3 {
			t.Errorf("open after 4 sources = %d, want 3 (at most 3 open)", e.openCount(email, "reset"))
		}
		if _, err := e.resetComplete(ipA, cA2, codeA2, pwaSecret()); !errors.Is(err, identity.ErrInvalidCode) {
			t.Errorf("oldest challenge after the 4th open: %v, want ErrInvalidCode (superseded)", err)
		}
		if _, err := e.resetComplete(ipB, cB, codeB, pwaSecret()); err != nil {
			t.Errorf("second-oldest challenge must survive: %v", err)
		}
	})

	t.Run("supersede_rules_signup", func(t *testing.T) {
		e := e.sub(t)
		_ = e
		email := pwaEmail()
		ips := []netip.Addr{pwaIP(), pwaIP(), pwaIP(), pwaIP()}
		var cs []identity.Challenge
		for i, ip := range ips {
			n := len(e.mailsTo(email)) + 1
			cs = append(cs, e.signupFrom(ip, email, pwaSecret()))
			e.awaitMails(email, n)
			if want := int64(min(i+1, 3)); e.openCount(email, "signup") != want {
				t.Errorf("open sign-up challenges after source %d = %d, want %d", i+1, e.openCount(email, "signup"), want)
			}
		}
		if _, err := e.pw.Complete(pwaBG, ips[0], cs[0].Binding, "signup", e.code(email, 1), ""); !errors.Is(err, identity.ErrInvalidCode) {
			t.Errorf("oldest sign-up challenge after the 4th: %v, want ErrInvalidCode", err)
		}
		if _, err := e.pw.Complete(pwaBG, ips[1], cs[1].Binding, "signup", e.code(email, 2), ""); err != nil {
			t.Errorf("second-oldest sign-up challenge must survive: %v", err)
		}
	})
}

func strPtr(s string) *string { return &s }
