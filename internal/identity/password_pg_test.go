package identity_test

// REAL_PG smoke of migration 0070 + the Passwords flows, written by the implementer (auth-core) to prove
// the SQL applies and the happy/negative paths hold. It is NOT the independent PA03-PA08 gates (those
// belong to auth-tests). Needs the disposable database from scripts/dev/test-focused.sh; without
// LC_TEST_DATABASE_ALLOWED=1 every test here skips (= NOT_RUN, never PASS).

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"livecommerce/internal/identity"
	"livecommerce/internal/mail"
	"livecommerce/internal/platform"
	"livecommerce/migrations"
)

// fakeMailer records messages; the real SMTP adapter is exercised by auth-mail/auth-tests.
type fakeMailer struct {
	mu    sync.Mutex
	msgs  []mail.Message
	err   error
	calls atomic.Int32
}

func (f *fakeMailer) Send(ctx context.Context, m mail.Message) (string, error) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, m)
	if f.err != nil {
		return "", f.err
	}
	return "250 queued", nil
}

func (f *fakeMailer) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.msgs) }

var digits6 = regexp.MustCompile(`\b[0-9]{6}\b`)

// waitCode waits until n messages exist and returns the 6-digit code of the n-th (1-based).
func (f *fakeMailer) waitCode(t *testing.T, n int) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		if len(f.msgs) >= n {
			m := f.msgs[n-1]
			f.mu.Unlock()
			return digits6.FindString(m.Text)
		}
		f.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("mail %d never arrived", n)
	return ""
}

type pgEnv struct {
	owner *pgxpool.Pool
	pool  *pgxpool.Pool
}

var (
	envOnce sync.Once
	envVal  *pgEnv
	envSkip string
	envErr  error
)

func pgFixture(t *testing.T) *pgEnv {
	t.Helper()
	envOnce.Do(func() { envVal, envSkip, envErr = newPGEnv() })
	if envSkip != "" {
		t.Skip(envSkip)
	}
	if envErr != nil {
		t.Fatal(envErr)
	}
	return envVal
}

func newPGEnv() (*pgEnv, string, error) {
	if os.Getenv("LC_TEST_DATABASE_ALLOWED") != "1" || os.Getenv("LC_TEST_DATABASE_URL") == "" {
		return nil, "LC_TEST_DATABASE_ALLOWED=1 and LC_TEST_DATABASE_URL are required; real-PG password test NOT_RUN", nil
	}
	cfg, err := pgxpool.ParseConfig(os.Getenv("LC_TEST_DATABASE_URL"))
	if err != nil {
		return nil, "", errors.New("LC_TEST_DATABASE_URL is invalid")
	}
	if cfg.ConnConfig.Host != "127.0.0.1" || cfg.ConnConfig.Database != "lc_foundation_test" {
		return nil, "", errors.New("refusing database outside 127.0.0.1/lc_foundation_test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	owner, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, "", err
	}
	if err := migrations.Apply(ctx, owner); err != nil {
		return nil, "", fmt.Errorf("apply migrations: %w", err)
	}
	if err := migrations.Apply(ctx, owner); err != nil { // idempotent second run
		return nil, "", fmt.Errorf("second apply: %w", err)
	}
	var raw [12]byte
	_, _ = rand.Read(raw[:])
	role := "pwcore_" + hex.EncodeToString(raw[:])
	secret := hex.EncodeToString(raw[:]) + hex.EncodeToString(raw[:])
	if _, err := owner.Exec(ctx, `CREATE ROLE `+pgx.Identifier{role}.Sanitize()+` LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE IN ROLE commerce_identity PASSWORD '`+secret+`'`); err != nil {
		return nil, "", err
	}
	u, _ := url.Parse(os.Getenv("LC_TEST_DATABASE_URL"))
	u.User = url.UserPassword(role, secret)
	pool, err := platform.OpenIdentityPool(ctx, u.String())
	if err != nil {
		return nil, "", err
	}
	return &pgEnv{owner: owner, pool: pool}, "", nil
}

var seq atomic.Int64

func uniqueEmail() string { return fmt.Sprintf("pw%d.%d@example.test", os.Getpid(), seq.Add(1)) }

func ipN(n int) netip.Addr { return netip.MustParseAddr(fmt.Sprintf("203.0.113.%d", 1+n%250)) }

var ipCounter atomic.Int64

func freshIP() netip.Addr { return ipN(int(ipCounter.Add(1))) }

func newPasswords(t *testing.T, env *pgEnv, m identity.Mailer, mutate func(*identity.PasswordPolicy)) *identity.Passwords {
	t.Helper()
	pepper := make([]byte, 32) // a fresh pepper = fresh throttle buckets for this test
	_, _ = rand.Read(pepper)
	p := identity.PasswordPolicy{Pepper: pepper, SessionTTL: time.Hour, BreachCheck: "off", AllowLoopback: true}
	if mutate != nil {
		mutate(&p)
	}
	pw, err := identity.NewPasswords(env.pool, m, p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pw.Close(context.Background()) })
	return pw
}

func tokenHash(token string) []byte { h := sha256.Sum256([]byte(token)); return h[:] }

const samplePhrase = "correct horse battery 1"

// signupVerified registers email and completes the code step; returns the session token.
func signupVerified(t *testing.T, env *pgEnv, p *identity.Passwords, f *fakeMailer, email, pw string) string {
	t.Helper()
	before := f.count()
	ch, err := p.Signup(context.Background(), freshIP(), email, pw, "en")
	if err != nil {
		t.Fatalf("signup: %v", err)
	}
	code := f.waitCode(t, before+1)
	s, err := p.Complete(context.Background(), freshIP(), ch.Binding, "signup", code, "")
	if err != nil {
		t.Fatalf("complete signup: %v", err)
	}
	return s.Token
}

func TestPasswordCoreSignupLoginResetRealPG(t *testing.T) {
	env := pgFixture(t)
	ctx := context.Background()
	f := &fakeMailer{}
	p := newPasswords(t, env, f, nil)
	email := uniqueEmail()

	// Sign-up: 202-shaped challenge, background code mail, complete creates principal+credential+session.
	ch, err := p.Signup(ctx, freshIP(), email, samplePhrase, "zh-CN")
	if err != nil || len(ch.Binding) != 43 || time.Until(ch.ExpiresAt) < 9*time.Minute {
		t.Fatalf("signup = %+v %v", ch, err)
	}
	code := f.waitCode(t, 1)
	if _, err := p.Complete(ctx, freshIP(), ch.Binding, "signup", wrongCode(code), ""); !errors.Is(err, identity.ErrInvalidCode) {
		t.Fatalf("wrong code err = %v", err)
	}
	s1, err := p.Complete(ctx, freshIP(), ch.Binding, "signup", code, "")
	if err != nil || len(s1.Token) != 43 {
		t.Fatalf("complete signup = %+v %v", s1, err)
	}
	if _, err := p.Complete(ctx, freshIP(), ch.Binding, "signup", code, ""); !errors.Is(err, identity.ErrInvalidCode) {
		t.Fatalf("replay err = %v", err)
	}
	var principals, creds, sessions, events int
	must(t, env.owner.QueryRow(ctx, `SELECT count(*) FROM identity.password_credentials WHERE email=$1`, email).Scan(&creds))
	must(t, env.owner.QueryRow(ctx, `SELECT count(*) FROM identity.principals p JOIN identity.password_credentials c ON c.principal_id=p.id WHERE c.email=$1`, email).Scan(&principals))
	must(t, env.owner.QueryRow(ctx, `SELECT count(*) FROM identity.sessions WHERE token_hash=$1 AND audience='merchant' AND revoked_at IS NULL`, tokenHash(s1.Token)).Scan(&sessions))
	must(t, env.owner.QueryRow(ctx, `SELECT count(*) FROM identity.session_events e JOIN identity.sessions s ON s.id=e.session_id WHERE s.token_hash=$1 AND e.action='session.issued'`, tokenHash(s1.Token)).Scan(&events))
	if creds != 1 || principals != 1 || sessions != 1 || events != 1 {
		t.Fatalf("rows creds=%d principals=%d sessions=%d events=%d", creds, principals, sessions, events)
	}
	// The mail carried the code in the body, none in the subject, and no URL.
	f.mu.Lock()
	m0 := f.msgs[0]
	f.mu.Unlock()
	if strings.Contains(m0.Subject, code) || strings.Contains(m0.Text+m0.HTML, "http") || m0.To != email {
		t.Fatalf("mail shape wrong: %+v", m0)
	}
	waitMailState(t, env, email, "SENT")

	// Sign-up of the taken email from another source: same 202 shape, notice mail, no new challenge row.
	var rowsBefore, rowsAfter int
	must(t, env.owner.QueryRow(ctx, `SELECT count(*) FROM identity.email_challenges WHERE email=$1`, email).Scan(&rowsBefore))
	ch2, err := p.Signup(ctx, freshIP(), email, samplePhrase+"x", "en")
	if err != nil || len(ch2.Binding) != 43 {
		t.Fatalf("signup taken = %+v %v", ch2, err)
	}
	f.waitCode(t, 2) // notice mail arrives (contains no code, FindString returns "")
	must(t, env.owner.QueryRow(ctx, `SELECT count(*) FROM identity.email_challenges WHERE email=$1`, email).Scan(&rowsAfter))
	if rowsAfter != rowsBefore {
		t.Fatalf("taken email created a challenge row (%d -> %d)", rowsBefore, rowsAfter)
	}
	if _, err := p.Complete(ctx, freshIP(), ch2.Binding, "signup", "123456", ""); !errors.Is(err, identity.ErrInvalidCode) {
		t.Fatalf("taken-email binding must look like a wrong code, got %v", err)
	}

	// Login: password + code. Unknown email and wrong password both run Argon2 and answer identically.
	ipL := freshIP()
	before := identity.ArgonCount()
	if _, err := p.Login(ctx, ipL, email, "not the password 123", "en"); !errors.Is(err, identity.ErrInvalidCredentials) {
		t.Fatalf("wrong password err = %v", err)
	}
	if _, err := p.Login(ctx, ipL, uniqueEmail(), samplePhrase, "en"); !errors.Is(err, identity.ErrInvalidCredentials) {
		t.Fatalf("unknown email err = %v", err)
	}
	if _, err := p.Login(ctx, ipL, "not an email", samplePhrase, "en"); !errors.Is(err, identity.ErrInvalidCredentials) {
		t.Fatalf("malformed email err = %v", err)
	}
	if got := identity.ArgonCount() - before; got != 3 {
		t.Fatalf("argon runs on the three failing logins = %d, want 3", got)
	}
	lc, err := p.Login(ctx, ipL, email, samplePhrase, "en")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	lcode := f.waitCode(t, 3)
	s2, err := p.Complete(ctx, ipL, lc.Binding, "login", lcode, "")
	if err != nil || s2.Token == "" || s2.Token == s1.Token {
		t.Fatalf("complete login = %+v %v", s2, err)
	}
	if _, err := p.Complete(ctx, ipL, lc.Binding, "reset", lcode, "another good password 9"); !errors.Is(err, identity.ErrInvalidCode) {
		t.Fatalf("purpose mismatch err = %v", err)
	}

	// Reset for an unknown email: identical shape, no mail, complete = invalid_code.
	msgs := f.count()
	rc, err := p.Reset(ctx, freshIP(), uniqueEmail(), "en")
	if err != nil || len(rc.Binding) != 43 {
		t.Fatalf("reset unknown = %+v %v", rc, err)
	}
	time.Sleep(150 * time.Millisecond)
	if f.count() != msgs {
		t.Fatal("reset of an unknown email sent mail")
	}
	if _, err := p.Complete(ctx, freshIP(), rc.Binding, "reset", "123456", "another good password 9"); !errors.Is(err, identity.ErrInvalidCode) {
		t.Fatalf("unknown reset complete err = %v", err)
	}

	// Reset for the real email: sessions revoked, version+1, old password dead, new one signs in.
	rc, err = p.Reset(ctx, freshIP(), email, "en")
	if err != nil {
		t.Fatal(err)
	}
	rcode := f.waitCode(t, msgs+1)
	if _, err := p.Complete(ctx, freshIP(), rc.Binding, "reset", rcode, "short"); !isPolicy(err, "too_short") {
		t.Fatalf("short new password err = %v", err)
	}
	s3, err := p.Complete(ctx, freshIP(), rc.Binding, "reset", rcode, "another good password 9")
	if err != nil {
		t.Fatalf("complete reset: %v", err)
	}
	var revoked, live, version int
	must(t, env.owner.QueryRow(ctx, `SELECT count(*) FROM identity.sessions s JOIN identity.password_credentials c ON c.principal_id=s.principal_id WHERE c.email=$1 AND s.revoked_at IS NOT NULL`, email).Scan(&revoked))
	must(t, env.owner.QueryRow(ctx, `SELECT count(*) FROM identity.sessions WHERE token_hash=$1 AND revoked_at IS NULL`, tokenHash(s3.Token)).Scan(&live))
	must(t, env.owner.QueryRow(ctx, `SELECT password_version FROM identity.password_credentials WHERE email=$1`, email).Scan(&version))
	if revoked != 2 || live != 1 || version != 2 {
		t.Fatalf("after reset revoked=%d live=%d version=%d", revoked, live, version)
	}
	if _, err := p.Login(ctx, freshIP(), email, samplePhrase, "en"); !errors.Is(err, identity.ErrInvalidCredentials) {
		t.Fatalf("old password still works: %v", err)
	}
	// email-mail-login is 1/60 s per email, so a second mail-reaching login needs fresh buckets (new pepper).
	if _, err := newPasswords(t, env, f, nil).Login(ctx, freshIP(), email, "another good password 9", "en"); err != nil {
		t.Fatalf("new password login: %v", err)
	}

	// Least privilege: the identity login cannot read the credential table directly (definers only).
	if _, err := env.pool.Exec(ctx, `SELECT 1 FROM identity.password_credentials`); err == nil {
		t.Fatal("commerce_identity can read password_credentials directly")
	}
}

func TestPasswordCoreLockoutExhaustionAndVersionBindingRealPG(t *testing.T) {
	env := pgFixture(t)
	ctx := context.Background()
	f := &fakeMailer{}
	p := newPasswords(t, env, f, nil)
	email := uniqueEmail()
	signupVerified(t, env, p, f, email, samplePhrase)

	// 100 consecutive failures disable; the 101st and 150th answer 401 and the counter stays 100.
	if _, err := env.owner.Exec(ctx, `UPDATE identity.password_credentials SET failed_count=98 WHERE email=$1`, email); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := p.Login(ctx, freshIP(), email, "definitely wrong 123", "en"); !errors.Is(err, identity.ErrInvalidCredentials) {
			t.Fatalf("wrong login %d err = %v", i, err)
		}
	}
	var failed int
	var disabled bool
	must(t, env.owner.QueryRow(ctx, `SELECT failed_count, disabled_at IS NOT NULL FROM identity.password_credentials WHERE email=$1`, email).Scan(&failed, &disabled))
	if failed != 100 || !disabled {
		t.Fatalf("failed=%d disabled=%v", failed, disabled)
	}
	if _, err := p.Login(ctx, freshIP(), email, samplePhrase, "en"); !errors.Is(err, identity.ErrInvalidCredentials) {
		t.Fatalf("correct password on disabled credential err = %v", err)
	}
	must(t, env.owner.QueryRow(ctx, `SELECT failed_count FROM identity.password_credentials WHERE email=$1`, email).Scan(&failed))
	if failed != 100 {
		t.Fatalf("counter moved past 100: %d", failed)
	}
	// A reset re-enables.
	msgs := f.count()
	rc, err := p.Reset(ctx, freshIP(), email, "en")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Complete(ctx, freshIP(), rc.Binding, "reset", f.waitCode(t, msgs+1), "reset after lockout 55"); err != nil {
		t.Fatal(err)
	}
	must(t, env.owner.QueryRow(ctx, `SELECT failed_count, disabled_at IS NOT NULL FROM identity.password_credentials WHERE email=$1`, email).Scan(&failed, &disabled))
	if failed != 0 || disabled {
		t.Fatalf("reset did not re-enable: %d %v", failed, disabled)
	}

	// Five wrong codes exhaust a challenge; the right code afterwards is INVALID.
	msgs = f.count()
	lc, err := newPasswords(t, env, f, nil).Login(ctx, freshIP(), email, "reset after lockout 55", "en")
	if err != nil {
		t.Fatal(err)
	}
	code := f.waitCode(t, msgs+1)
	ipC := freshIP()
	for i := 0; i < 5; i++ {
		if _, err := p.Complete(ctx, ipC, lc.Binding, "login", wrongCode(code), ""); !errors.Is(err, identity.ErrInvalidCode) {
			t.Fatalf("wrong code %d err = %v", i, err)
		}
	}
	if _, err := p.Complete(ctx, ipC, lc.Binding, "login", code, ""); !errors.Is(err, identity.ErrInvalidCode) {
		t.Fatalf("exhausted challenge accepted the right code: %v", err)
	}

	// Version binding: a login challenge started before a reset is INVALID after it.
	msgs = f.count()
	lc, err = newPasswords(t, env, f, nil).Login(ctx, freshIP(), email, "reset after lockout 55", "en")
	if err != nil {
		t.Fatal(err)
	}
	lcode := f.waitCode(t, msgs+1)
	msgs = f.count()
	rc, err = p.Reset(ctx, freshIP(), email, "en")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Complete(ctx, freshIP(), rc.Binding, "reset", f.waitCode(t, msgs+1), "second reset password 77"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Complete(ctx, freshIP(), lc.Binding, "login", lcode, ""); !errors.Is(err, identity.ErrInvalidCode) {
		t.Fatalf("stale login challenge err = %v", err)
	}
}

func TestPasswordCoreThrottlesAndGlobalBudgetsRealPG(t *testing.T) {
	env := pgFixture(t)
	ctx := context.Background()

	// Per-(email, source) 1/60 s: the second unauthenticated mail from the same source is 429 with Retry-After.
	f := &fakeMailer{}
	p := newPasswords(t, env, f, nil)
	ip := freshIP()
	email := uniqueEmail()
	if _, err := p.Reset(ctx, ip, email, "en"); err != nil {
		t.Fatal(err)
	}
	_, err := p.Reset(ctx, ip, email, "en")
	var te identity.ThrottleError
	if !errors.As(err, &te) || te.RetryAfter < time.Second || te.RetryAfter > 60*time.Second {
		t.Fatalf("second reset err = %v", err)
	}
	// Another source for the same email still passes (keyed per source).
	if _, err := p.Reset(ctx, freshIP(), email, "en"); err != nil {
		t.Fatalf("other source blocked: %v", err)
	}

	// global-mail-unauth = 40% of the daily cap (cap 20 => 8): the 9th unauthenticated mail fails closed.
	p2 := newPasswords(t, env, &fakeMailer{}, func(pp *identity.PasswordPolicy) { pp.MailDailyCap = 20 })
	for i := 0; i < 8; i++ {
		if _, err := p2.Reset(ctx, freshIP(), uniqueEmail(), "en"); err != nil {
			t.Fatalf("reset %d: %v", i, err)
		}
	}
	if _, err := p2.Reset(ctx, freshIP(), uniqueEmail(), "en"); !errors.Is(err, identity.ErrMailUnavailable) {
		t.Fatalf("9th unauth mail err = %v", err)
	}

	// global-mail-login-new = 15% (3): accounts without a membership hit it; a member still logs in.
	f3 := &fakeMailer{}
	p3 := newPasswords(t, env, f3, func(pp *identity.PasswordPolicy) { pp.MailDailyCap = 20 })
	member := uniqueEmail()
	signupVerified(t, env, p3, f3, member, samplePhrase)
	var principal string
	must(t, env.owner.QueryRow(ctx, `SELECT principal_id::text FROM identity.password_credentials WHERE email=$1`, member).Scan(&principal))
	var tenant string
	must(t, env.owner.QueryRow(ctx, `INSERT INTO control.tenants(id,name) VALUES(gen_random_uuid(),'pwcore') RETURNING id::text`).Scan(&tenant))
	if _, err := env.owner.Exec(ctx, `INSERT INTO identity.memberships(tenant_id,principal_id) VALUES($1,$2)`, tenant, principal); err != nil {
		t.Fatal(err)
	}
	var lastErr error
	newAccounts := []string{uniqueEmail(), uniqueEmail(), uniqueEmail(), uniqueEmail()}
	for _, e := range newAccounts {
		signupVerified(t, env, p3, f3, e, samplePhrase)
	}
	for i, e := range newAccounts {
		_, lastErr = p3.Login(ctx, freshIP(), e, samplePhrase, "en")
		if i < 3 && lastErr != nil {
			t.Fatalf("no-membership login %d: %v", i, lastErr)
		}
	}
	if !errors.Is(lastErr, identity.ErrMailUnavailable) {
		t.Fatalf("4th no-membership login err = %v", lastErr)
	}
	if _, err := p3.Login(ctx, freshIP(), member, samplePhrase, "en"); err != nil {
		t.Fatalf("member login blocked by the no-membership share: %v", err)
	}
	// The fail-closed challenge was recorded FAILED, not left PENDING.
	var pending int
	must(t, env.owner.QueryRow(ctx, `SELECT count(*) FROM identity.email_challenges WHERE email=$1 AND mail_state='PENDING'`, newAccounts[3]).Scan(&pending))
	if pending != 0 {
		t.Fatalf("global-cap challenge stayed PENDING")
	}
}

func TestPasswordCoreConcurrentSignupOnePrincipalRealPG(t *testing.T) {
	env := pgFixture(t)
	ctx := context.Background()
	f := &fakeMailer{}
	p := newPasswords(t, env, f, nil)
	email := uniqueEmail()
	c1, err := p.Signup(ctx, freshIP(), email, samplePhrase, "en")
	if err != nil {
		t.Fatal(err)
	}
	c2, err := p.Signup(ctx, freshIP(), email, samplePhrase, "en")
	if err != nil {
		t.Fatal(err)
	}
	code1, code2 := f.waitCode(t, 1), f.waitCode(t, 2)
	if code1 == code2 {
		t.Skip("codes collided (1e-6); rerun")
	}
	// Mails may arrive in either order: map codes to bindings by trying both.
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, c := range []struct{ b string }{{c1.Binding}, {c2.Binding}} {
		wg.Add(1)
		go func(binding string) {
			defer wg.Done()
			var err error
			for _, code := range []string{code1, code2} {
				if _, err = p.Complete(ctx, freshIP(), binding, "signup", code, ""); err == nil || errors.Is(err, identity.ErrAccountExists) {
					break
				}
			}
			results <- err
		}(c.b)
	}
	wg.Wait()
	close(results)
	var ok, exists int
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, identity.ErrAccountExists):
			exists++
		}
	}
	var principals int
	must(t, env.owner.QueryRow(ctx, `SELECT count(*) FROM identity.password_credentials WHERE email=$1`, email).Scan(&principals))
	if principals != 1 || ok != 1 || exists != 1 {
		t.Fatalf("principals=%d ok=%d exists=%d", principals, ok, exists)
	}
}

func TestPasswordCoreMailOutcomesRealPG(t *testing.T) {
	env := pgFixture(t)
	ctx := context.Background()
	email := uniqueEmail()
	ok := &fakeMailer{}
	p := newPasswords(t, env, ok, nil)
	signupVerified(t, env, p, ok, email, samplePhrase)

	// Login waits for the send: FAILED => ErrMailUnavailable, UNKNOWN => 202-shaped success; never re-sent.
	failing := &fakeMailer{err: fmt.Errorf("%w: rejected", mail.ErrFailed)}
	pf, err := identity.NewPasswords(env.pool, failing, identity.PasswordPolicy{Pepper: bytes32(), SessionTTL: time.Hour, BreachCheck: "off", AllowLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pf.Login(ctx, freshIP(), email, samplePhrase, "en"); !errors.Is(err, identity.ErrMailUnavailable) {
		t.Fatalf("FAILED send err = %v", err)
	}
	if failing.calls.Load() != 1 {
		t.Fatalf("sends = %d, want exactly 1", failing.calls.Load())
	}
	unknown := &fakeMailer{err: fmt.Errorf("%w: dropped", mail.ErrUnknown)}
	pu, err := identity.NewPasswords(env.pool, unknown, identity.PasswordPolicy{Pepper: bytes32(), SessionTTL: time.Hour, BreachCheck: "off", AllowLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pu.Login(ctx, freshIP(), email, samplePhrase, "en"); err != nil {
		t.Fatalf("UNKNOWN send must still answer: %v", err)
	}
	if unknown.calls.Load() != 1 {
		t.Fatalf("sends = %d, want exactly 1", unknown.calls.Load())
	}
	waitMailState(t, env, email, "UNKNOWN")
}

func bytes32() []byte { b := make([]byte, 32); _, _ = rand.Read(b); return b }

func wrongCode(code string) string {
	if code == "000000" {
		return "000001"
	}
	return "000000"
}

func isPolicy(err error, reason string) bool {
	var pe identity.PolicyError
	return errors.As(err, &pe) && pe.Reason == reason
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// waitMailState polls until some challenge of email has the given mail_state (the record is written by
// the detached sender after the response).
func waitMailState(t *testing.T, env *pgEnv, email, state string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		must(t, env.owner.QueryRow(context.Background(), `SELECT count(*) FROM identity.email_challenges WHERE email=$1 AND mail_state=$2`, email, state).Scan(&n))
		if n > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no challenge reached mail_state %s", state)
}

func TestPasswordCoreThrottleWindowsAndPurgeBoundRealPG(t *testing.T) {
	env := pgFixture(t)
	ctx := context.Background()
	hit := func(bucket []byte, window, offset int) int {
		var hits int
		must(t, env.pool.QueryRow(ctx, `SELECT identity.auth_throttle_hit($1,$2,$3)`, bucket, window, offset).Scan(&hits))
		return hits
	}
	b := bytes32()
	if hit(b, 86400, 28800) != 1 || hit(b, 86400, 28800) != 2 {
		t.Fatal("hits do not count up within a window")
	}
	// UTC+8 day: window_start is 16:00 UTC (= 00:00 UTC+8) and contains now.
	var aligned bool
	var startAge time.Duration
	var ageSecs float64
	must(t, env.owner.QueryRow(ctx, `SELECT (extract(epoch FROM window_start)::bigint + 28800) % 86400 = 0,
		extract(epoch FROM clock_timestamp() - window_start) FROM identity.auth_throttle WHERE bucket=sha256($1::bytea||int4send(86400)||int4send(28800))`, b).Scan(&aligned, &ageSecs)) // F1: stored key is derived per window
	startAge = time.Duration(ageSecs * float64(time.Second))
	if !aligned || startAge < 0 || startAge >= 24*time.Hour {
		t.Fatalf("UTC+8 window misaligned: aligned=%v age=%v", aligned, startAge)
	}
	c := bytes32()
	hit(c, 900, 0)
	must(t, env.owner.QueryRow(ctx, `SELECT extract(epoch FROM window_start)::bigint % 900 = 0 FROM identity.auth_throttle WHERE bucket=sha256($1::bytea||int4send(900)||int4send(0))`, c).Scan(&aligned))
	if !aligned {
		t.Fatal("15 min window misaligned")
	}
	// Purge is bounded to 100 rows per call: 150 expired rows -> 50 left after one hit.
	tag := bytes32()
	if _, err := env.owner.Exec(ctx, `INSERT INTO identity.auth_throttle(bucket,window_start,hits)
		SELECT sha256(($1::bytea || int4send(g))), clock_timestamp() - interval '3 days' - g * interval '1 second', 1 FROM generate_series(1,150) g`, tag); err != nil {
		t.Fatal(err)
	}
	var old int
	count := `SELECT count(*) FROM identity.auth_throttle WHERE window_start < clock_timestamp() - interval '2 days'`
	must(t, env.owner.QueryRow(ctx, count).Scan(&old))
	hit(bytes32(), 60, 0)
	var after int
	must(t, env.owner.QueryRow(ctx, count).Scan(&after))
	if old-after != 100 {
		t.Fatalf("purge removed %d rows in one call, want exactly 100 (before %d after %d)", old-after, old, after)
	}
	// Argument validation raises PT400 instead of writing.
	if _, err := env.pool.Exec(ctx, `SELECT identity.auth_throttle_hit($1,0,0)`, b); err == nil {
		t.Fatal("window 0 accepted")
	}
}
