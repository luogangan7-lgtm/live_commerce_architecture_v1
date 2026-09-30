package identity

// Merchant password auth flows (contracts/merchant-password-auth-v1.md §2): sign-up, login and reset
// each start an emailed 6-digit challenge; Complete consumes it and is the ONLY path that issues a
// password-principal session (PD8), inside identity.complete_email_challenge.
//
// Trust boundary: this package runs as commerce_identity (EXECUTE on the identity.* definers only).
// Tenant and store are never read here: they are unknown before authentication (PD13) and must never
// come from the client (I01). Every rate-limit check (throttle.go) runs before hashing, HIBP, mail or
// any existence lookup (§6), so the answers for known and unknown emails are identical.

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/text/unicode/norm"
)

// PasswordPolicy configures Passwords (all values from cmd/api/identity.go, never from a request).
type PasswordPolicy struct {
	Pepper        []byte        // 32 bytes (COMMERCE_AUTH_PEPPER_FILE)
	SessionTTL    time.Duration // 5 min-24 h (existing COMMERCE_SESSION_TTL)
	MailDailyCap  int           // 20-100000, default 200 (§6 shares 40/15/45 %)
	BreachCheck   string        // "hibp" | "off" (off only with AllowLoopback)
	HIBPBaseURL   string        // "" = https://api.pwnedpasswords.com; override only with AllowLoopback
	AllowLoopback bool
}

// Challenge is the step-1 result. Binding is a secret for the BFF's HttpOnly challenge cookie
// (A1: 32 random bytes, base64url, 43 chars); it must never reach client JavaScript or a log.
type Challenge struct {
	Binding   string    `json:"binding"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Passwords owns the password flows. Construct once per process with NewPasswords.
type Passwords struct {
	pool     *pgxpool.Pool
	mailer   Mailer
	policy   PasswordPolicy
	hibpBase string
	hibp     *http.Client
	dummyPHC string // A4: unknown emails verify against this so Argon2 always runs (PA05)

	bgSlots  chan struct{} // A5: 8 background mail sends
	bgMu     sync.Mutex
	bgClosed bool
	bgWG     sync.WaitGroup

	recordHook func(id [16]byte, state, reply string) // tests only; nil in production
}

const challengeTTL = 10 * time.Minute // PD4; the SQL sets the real expires_at

// NewPasswords validates the policy and precomputes the dummy PHC (one Argon2 run).
func NewPasswords(pool *pgxpool.Pool, mailer Mailer, p PasswordPolicy) (*Passwords, error) {
	if p.MailDailyCap == 0 {
		p.MailDailyCap = 200
	}
	if p.BreachCheck == "" {
		p.BreachCheck = "hibp"
	}
	if pool == nil || mailer == nil || len(p.Pepper) != 32 || p.SessionTTL < 5*time.Minute || p.SessionTTL > 24*time.Hour ||
		p.MailDailyCap < 20 || p.MailDailyCap > 100000 || (p.BreachCheck != "hibp" && p.BreachCheck != "off") ||
		(p.BreachCheck == "off" && !p.AllowLoopback) {
		return nil, ErrInvalid
	}
	base := hibpDefaultBase
	if p.HIBPBaseURL != "" {
		u, err := url.Parse(p.HIBPBaseURL)
		if err != nil || !p.AllowLoopback || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !loopbackHost(u.Hostname()) {
			return nil, ErrInvalid // production uses the fixed host; only loopback tests may override it
		}
		base = strings.TrimRight(p.HIBPBaseURL, "/")
	}
	random := make([]byte, 32)
	_, _ = rand.Read(random)
	dummy, err := HashPassword(string(random))
	if err != nil {
		return nil, err
	}
	p.Pepper = append([]byte(nil), p.Pepper...)
	return &Passwords{pool: pool, mailer: mailer, policy: p, hibpBase: base, hibp: newHIBPClient(), dummyPHC: dummy,
		bgSlots: make(chan struct{}, asyncSlots)}, nil
}

func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.Zone() == "" && a.IsLoopback()
}

// material is the per-challenge secret set generated in Go: the id and binding never repeat, the code
// is only ever sent by mail, the database sees only the two hashes (A1).
type material struct {
	id          [16]byte
	binding     string
	bindingHash []byte
	code        string
	codeHMAC    []byte
}

func (p *Passwords) newMaterial() (material, error) {
	var m material
	_, _ = rand.Read(m.id[:])
	m.id[6] = m.id[6]&0x0f | 0x40 // UUID v4
	m.id[8] = m.id[8]&0x3f | 0x80
	m.binding = randomToken()
	m.bindingHash = digest(m.binding) // A1: sha256 of the 43-char binding string, like session tokens
	code, err := NewCode()
	if err != nil {
		return m, ErrUnavailable
	}
	m.code = code
	m.codeHMAC = CodeHMAC(p.policy.Pepper, m.bindingHash, code)
	return m, nil
}

// validClient rejects a zero/invalid address before it can become a bucket key.
func validClient(ip netip.Addr) error {
	if !ip.IsValid() {
		return ErrInvalid
	}
	return nil
}

// Signup starts an email-verified registration: policy, HIBP and Argon2 always run (also when the email
// exists, PD6), then start_signup_challenge either inserts a challenge (code mail) or reports the email
// taken (notice mail). Both answer 202 before any SMTP (X3).
func (p *Passwords) Signup(ctx context.Context, ip netip.Addr, email, password, locale string) (Challenge, error) {
	if !validLocale(locale) || validClient(ip) != nil {
		return Challenge{}, ErrInvalid
	}
	ip = ip.Unmap()
	sign := append(ipBuckets(ip), bucket{kind: "ip-signup", value: ipPrefix(ip).String(), wins: winIPSignup})
	if p48, ok := ip48Prefix(ip); ok {
		sign = append(sign, bucket{kind: "ip48-signup", value: p48.String(), wins: winIP48Sign})
	}
	if err := p.throttle(ctx, sign...); err != nil {
		return Challenge{}, err
	}
	em, err := NormalizeEmail(email)
	if err != nil {
		return Challenge{}, err
	}
	pw := norm.NFC.String(password)
	// Pure policy first: a mistyped short password must not burn the 1/60 s per-email mail bucket.
	if err := CheckPasswordPolicy(pw, em); err != nil {
		return Challenge{}, err
	}
	if err := p.throttle(ctx, p.unauthMailBuckets(em, ip)...); err != nil { // before any existence lookup
		return Challenge{}, err
	}
	if err := p.checkBreach(ctx, pw); err != nil {
		return Challenge{}, err
	}
	hash, err := HashPassword(pw)
	if err != nil {
		return Challenge{}, err
	}
	m, err := p.newMaterial()
	if err != nil {
		return Challenge{}, err
	}
	var outcome string
	var expires *time.Time
	err = withTx(ctx, p.pool, func(ctx context.Context, tx pgx.Tx) error {
		// identity.start_signup_challenge: EXISTS (no row) or a new signup challenge holding the pending hash.
		return tx.QueryRow(ctx, `SELECT outcome,expires_at FROM identity.start_signup_challenge($1,$2,$3,$4,$5,$6,$7)`,
			m.id, em, hash, m.bindingHash, m.codeHMAC, locale, p.bucketKey("ip", ipPrefix(ip).String())).Scan(&outcome, &expires)
	})
	if err != nil {
		return Challenge{}, ErrUnavailable
	}
	switch outcome {
	case "CHALLENGE":
		p.sendAsync(ctx, m.id, true, codeMail(em, locale, "signup", m.code))
	case "EXISTS":
		p.sendAsync(ctx, m.id, false, existsMail(em, locale)) // no challenge row: nothing to record
	default:
		return Challenge{}, ErrUnavailable
	}
	return challengeResult(m.binding, expires), nil
}

// Login verifies the password (always running Argon2, against the dummy PHC when the email is unknown),
// then emails a code and WAITS for the send (the caller proved the password; FAILED => 503).
func (p *Passwords) Login(ctx context.Context, ip netip.Addr, email, password, locale string) (Challenge, error) {
	if !validLocale(locale) || validClient(ip) != nil {
		return Challenge{}, ErrInvalid
	}
	ip = ip.Unmap()
	if err := p.throttle(ctx, ipBuckets(ip)...); err != nil {
		return Challenge{}, err
	}
	pw := norm.NFC.String(password)
	if len(pw) > 1024 { // policy max is 128 code points (<= 512 bytes): longer can never match; cap the copy cost
		pw = pw[:1024]
	}
	em, emErr := NormalizeEmail(email)
	if emErr != nil {
		// Same cost as an unknown email; a malformed address is just another unknown one (401, not 422).
		_, _ = VerifyPassword(p.dummyPHC, pw)
		return Challenge{}, ErrInvalidCredentials
	}
	pfx := ipPrefix(ip).String()
	if err := p.throttle(ctx, bucket{kind: "email-pw", value: em + "‖" + pfx, wins: winEmailPW}); err != nil {
		return Challenge{}, err
	}
	var stored string
	var version int64
	var disabled, found bool
	err := withTx(ctx, p.pool, func(ctx context.Context, tx pgx.Tx) error {
		// identity.password_login_material: PHC, version and disabled flag; zero rows for unknown/inactive.
		err := tx.QueryRow(ctx, `SELECT password_hash,password_version,disabled FROM identity.password_login_material($1)`, em).Scan(&stored, &version, &disabled)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	})
	if err != nil {
		return Challenge{}, ErrUnavailable
	}
	target := stored
	if !found {
		target = p.dummyPHC // A4/PA05: Argon2 runs whether or not the email exists
	}
	ok, err := VerifyPassword(target, pw) // also runs when the credential is disabled (R2-1)
	if err != nil {
		if errors.Is(err, ErrBusy) {
			return Challenge{}, ErrBusy
		}
		return Challenge{}, ErrUnavailable
	}
	if !found || !ok {
		// identity.record_password_failure: counter saturates at 100, no-op for unknown emails, never raises.
		if err := withTx(ctx, p.pool, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT identity.record_password_failure($1)`, em)
			return err
		}); err != nil {
			return Challenge{}, ErrUnavailable
		}
		return Challenge{}, ErrInvalidCredentials
	}
	if disabled {
		return Challenge{}, ErrInvalidCredentials // right password, disabled credential: same 401 (PD12)
	}
	if err := p.throttle(ctx, bucket{kind: "email-mail-login", value: em, wins: winMailLogin}); err != nil {
		return Challenge{}, err
	}
	m, err := p.newMaterial()
	if err != nil {
		return Challenge{}, err
	}
	var expires time.Time
	var member bool
	err = withTx(ctx, p.pool, func(ctx context.Context, tx pgx.Tx) error {
		// identity.start_login_challenge: binds the challenge to the credential version, supersedes older
		// login challenges, reports has_membership for the mail budget split (PD13).
		return tx.QueryRow(ctx, `SELECT expires_at,has_membership FROM identity.start_login_challenge($1,$2,$3,$4,$5,$6,$7)`,
			m.id, em, version, m.bindingHash, m.codeHMAC, locale, p.bucketKey("ip", pfx)).Scan(&expires, &member)
	})
	if errors.Is(translateError(err), ErrUnauthorized) { // credential changed or disabled between verify and start
		return Challenge{}, ErrInvalidCredentials
	}
	if err != nil {
		return Challenge{}, ErrUnavailable
	}
	_, loginNew, login := mailShares(p.policy.MailDailyCap)
	global := p.globalBucket("global-mail-login", login)
	if !member {
		global = p.globalBucket("global-mail-login-new", loginNew)
	}
	if err := p.throttle(ctx, global); err != nil {
		if errors.Is(err, ErrMailUnavailable) {
			p.recordMail(ctx, m.id, "FAILED", "") // nothing was sent; the row must not stay PENDING
		}
		return Challenge{}, err
	}
	switch p.sendSync(ctx, m.id, codeMail(em, locale, "login", m.code)) {
	case "FAILED":
		return Challenge{}, ErrMailUnavailable
	default: // SENT, or UNKNOWN: the user can press resend (new code), never an automatic retry (I06)
		return challengeResult(m.binding, &expires), nil
	}
}

// Reset starts a password reset: always answers with a challenge shape (PD6); a code mail is sent in the
// background only when a usable credential exists.
func (p *Passwords) Reset(ctx context.Context, ip netip.Addr, email, locale string) (Challenge, error) {
	if !validLocale(locale) || validClient(ip) != nil {
		return Challenge{}, ErrInvalid
	}
	ip = ip.Unmap()
	if err := p.throttle(ctx, ipBuckets(ip)...); err != nil {
		return Challenge{}, err
	}
	em, err := NormalizeEmail(email)
	if err != nil {
		return Challenge{}, err
	}
	if err := p.throttle(ctx, p.unauthMailBuckets(em, ip)...); err != nil { // before any existence lookup
		return Challenge{}, err
	}
	m, err := p.newMaterial()
	if err != nil {
		return Challenge{}, err
	}
	var outcome string
	var expires *time.Time
	err = withTx(ctx, p.pool, func(ctx context.Context, tx pgx.Tx) error {
		// identity.start_reset_challenge: NONE for unknown/inactive emails, else same-source supersede + insert.
		return tx.QueryRow(ctx, `SELECT outcome,expires_at FROM identity.start_reset_challenge($1,$2,$3,$4,$5,$6)`,
			m.id, em, m.bindingHash, m.codeHMAC, locale, p.bucketKey("ip", ipPrefix(ip).String())).Scan(&outcome, &expires)
	})
	if err != nil {
		return Challenge{}, ErrUnavailable
	}
	switch outcome {
	case "CHALLENGE":
		p.sendAsync(ctx, m.id, true, codeMail(em, locale, "reset", m.code))
	case "NONE": // the returned binding has no row: complete answers invalid_code like a wrong code
	default:
		return Challenge{}, ErrUnavailable
	}
	return challengeResult(m.binding, expires), nil
}

// Complete consumes a challenge. The code shape and the binding/ip throttles come before any hashing or
// HIBP (§7.1); for reset the new password is checked, breach-checked and hashed only after they pass.
func (p *Passwords) Complete(ctx context.Context, ip netip.Addr, binding, purpose, code, newPassword string) (Session, error) {
	if validClient(ip) != nil || (purpose != "signup" && purpose != "login" && purpose != "reset") ||
		(purpose == "reset") != (newPassword != "") {
		return Session{}, ErrInvalid
	}
	if !validCode(code) {
		return Session{}, ErrInvalidCode // no throttle, no work: nothing to guess
	}
	ip = ip.Unmap()
	if err := p.throttle(ctx, ipBuckets(ip)...); err != nil {
		return Session{}, err
	}
	if !validToken(binding) {
		return Session{}, ErrInvalidCode
	}
	if err := p.throttle(ctx, bucket{kind: "binding", value: binding, wins: winBinding}); err != nil {
		return Session{}, err
	}
	var newHash *string
	if purpose == "reset" {
		pw := norm.NFC.String(newPassword)
		if err := CheckPasswordPolicy(pw, ""); err != nil { // equals_email is NOT_IMPLEMENTED at reset (contract §11)
			return Session{}, err
		}
		if err := p.checkBreach(ctx, pw); err != nil {
			return Session{}, err
		}
		h, err := HashPassword(pw)
		if err != nil {
			return Session{}, err
		}
		newHash = &h
	}
	bindingHash := digest(binding)
	token := randomToken()
	var outcome string
	var expires *time.Time
	err := withTx(ctx, p.pool, func(ctx context.Context, tx pgx.Tx) error {
		// identity.complete_email_challenge: the only issuer of a password-principal session (PD8); INVALID is
		// returned, not raised, so a wrong-code attempt counter commits.
		return tx.QueryRow(ctx, `SELECT outcome,expires_at FROM identity.complete_email_challenge($1,$2,$3,$4,$5,$6)`,
			bindingHash, purpose, CodeHMAC(p.policy.Pepper, bindingHash, code), newHash, digest(token), int64(p.policy.SessionTTL/time.Second)).Scan(&outcome, &expires)
	})
	if err != nil {
		return Session{}, ErrUnavailable
	}
	switch outcome {
	case "SESSION":
		if expires == nil {
			return Session{}, ErrUnavailable
		}
		return Session{Token: token, ExpiresAt: *expires}, nil // PrincipalID stays empty: the frozen definer returns none
	case "EXISTS":
		return Session{}, ErrAccountExists
	default: // INVALID: wrong, expired, exhausted, over budget or unknown are never told apart
		return Session{}, ErrInvalidCode
	}
}

// challengeResult picks the SQL expiry when there is one; for "no row" answers (taken email, unknown
// reset email) it fabricates the same 10-minute shape so the response cannot tell them apart (PD6).
// Both paths are normalised to whole UTC seconds: a real value decodes from timestamptz with
// microsecond precision while time.Now() on Linux carries nanoseconds, and the differing string
// length would tell the two apart. The SQL expiry stays authoritative in the row.
func challengeResult(binding string, expires *time.Time) Challenge {
	t := time.Now().Add(challengeTTL)
	if expires != nil {
		t = *expires
	}
	return Challenge{Binding: binding, ExpiresAt: t.UTC().Truncate(time.Second)}
}
