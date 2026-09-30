package identity

// Password primitives for merchant password auth (contracts/merchant-password-auth-v1.md PD1-PD4,
// PD11): Argon2id hashing under the PD3 limiter, email normalization, password policy and the
// emailed code. Everything here is pure (no database, no network) except the process-wide limiter
// and the Argon2 call counter; the flows that use it live in challenge.go.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
	"golang.org/x/text/unicode/norm"
)

var (
	// ErrInvalidEmail: not a syntactically valid ASCII email (HTTP 422 invalid_email).
	ErrInvalidEmail = errors.New("invalid email")
	// ErrInvalidCredentials: unknown email, wrong password or disabled credential, deliberately
	// indistinguishable (PD6; HTTP 401 invalid_credentials).
	ErrInvalidCredentials = errors.New("invalid credentials")
	// ErrInvalidCode: wrong, expired, exhausted, over-budget or unknown challenge, deliberately
	// indistinguishable (HTTP 401 invalid_code).
	ErrInvalidCode = errors.New("invalid code")
	// ErrAccountExists: a sign-up code was correct but the email was registered meanwhile (HTTP 409).
	ErrAccountExists = errors.New("account already exists")
	// ErrMailUnavailable: a deployment-wide mail budget is exhausted or the login mail failed
	// (HTTP 503 mail_unavailable, fail closed, O-B).
	ErrMailUnavailable = errors.New("mail unavailable")
	// ErrBusy: the PD3 limiter did not free a slot within 2 s (HTTP 503 busy).
	ErrBusy = errors.New("busy")
	// errBadHash: the stored string is not an argon2id PHC with the PD1 parameters.
	errBadHash = errors.New("identity: unsupported password hash")
)

// PolicyError is a password policy violation. Reason is one of too_short, too_long, breached,
// equals_email. Error() never contains the password (I11).
type PolicyError struct{ Reason string }

func (e PolicyError) Error() string { return "password policy: " + e.Reason }

// PD1: OWASP minimum Argon2id (m=19 MiB, t=2, p=1), https://cheatsheetseries.owasp.org/cheatsheets/
// Password_Storage_Cheat_Sheet.html and https://pkg.go.dev/golang.org/x/crypto/argon2 (retrieved 2026-09-29,
// contract F1/F2). Parameters are code constants: a change means a new hash version, never config.
const (
	argonMemoryKiB = 19456
	argonTime      = 2
	argonThreads   = 1
	argonSaltLen   = 16
	argonKeyLen    = 32
	phcParams      = "m=19456,t=2,p=1"

	minPasswordRunes = 12  // ruling Q2 (NIST 15 gap recorded in the contract §11)
	maxPasswordRunes = 128 // PD11
	maxEmailLen      = 254
)

var argonCalls atomic.Int64

// ArgonCount is the process-wide number of Argon2 key derivations (PA01/PA05: proves the dummy
// verify path really runs Argon2).
func ArgonCount() int64 { return argonCalls.Load() }

func deriveKey(password, salt []byte) []byte {
	argonCalls.Add(1)
	return argon2.IDKey(password, salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
}

// limiter is the PD3 semaphore: a fixed number of slots, acquire waits at most `wait`.
type limiter struct {
	slots chan struct{}
	wait  time.Duration
}

func newLimiter(n int, wait time.Duration) *limiter {
	return &limiter{slots: make(chan struct{}, n), wait: wait}
}

// acquire returns a release func, ErrBusy after the wait, or ctx.Err() when ctx ends first.
func (l *limiter) acquire(ctx context.Context) (func(), error) {
	release := func() { <-l.slots }
	select {
	case l.slots <- struct{}{}:
		return release, nil
	default:
	}
	t := time.NewTimer(l.wait)
	defer t.Stop()
	select {
	case l.slots <- struct{}{}:
		return release, nil
	case <-t.C:
		return nil, ErrBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// hashLimiter guards every Argon2 computation and every HIBP call: 4 x 19 MiB ~ 76 MiB (PD3, I23).
var hashLimiter = newLimiter(4, 2*time.Second)

// HashPassword returns the PD1 PHC string. It waits for the PD3 limiter (ErrBusy after 2 s).
func HashPassword(password string) (string, error) {
	release, err := hashLimiter.acquire(context.Background())
	if err != nil {
		return "", err
	}
	defer release()
	salt := make([]byte, argonSaltLen)
	_, _ = rand.Read(salt) // Go >= 1.24: Read always fills the buffer; entropy failure terminates the process
	key := deriveKey([]byte(password), salt)
	return fmt.Sprintf("$argon2id$v=19$%s$%s$%s", phcParams,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword checks password against a PD1 PHC string. It rejects anything that is not exactly
// argon2id v19 with the PD1 parameters (argon2i/argon2d/bcrypt strings, other m/t/p, wrong salt or
// key length) with an error and never runs a derivation for them. The comparison is constant time.
func VerifyPassword(phc, password string) (bool, error) {
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" || parts[3] != phcParams {
		return false, errBadHash
	}
	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil || len(salt) != argonSaltLen {
		return false, errBadHash
	}
	want, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil || len(want) != argonKeyLen {
		return false, errBadHash
	}
	release, err := hashLimiter.acquire(context.Background())
	if err != nil {
		return false, err
	}
	defer release()
	got := deriveKey([]byte(password), salt)
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// NormalizeEmail trims, lower-cases and validates an email: ASCII only (contract §4.1: the CHECK
// makes lower() collation independent), one '@', at most 254 bytes. The character classes are the
// RFC 5322 atext set for the local part and LDH labels for the domain, which also keeps SMTP
// command and header injection characters (space, CR, LF, '<', '>', ',', '"') out of RCPT/To.
func NormalizeEmail(raw string) (string, error) {
	e := strings.ToLower(strings.TrimSpace(raw))
	if len(e) < 3 || len(e) > maxEmailLen || strings.Count(e, "@") != 1 {
		return "", ErrInvalidEmail
	}
	local, domain, _ := strings.Cut(e, "@")
	if local == "" || domain == "" || strings.HasPrefix(local, ".") || strings.HasSuffix(local, ".") || strings.Contains(local, "..") {
		return "", ErrInvalidEmail
	}
	for i := 0; i < len(local); i++ {
		c := local[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte(".!#$%&'*+/=?^_`{|}~-", c) >= 0) {
			return "", ErrInvalidEmail
		}
	}
	for _, label := range strings.Split(domain, ".") {
		if label == "" || label[0] == '-' || label[len(label)-1] == '-' {
			return "", ErrInvalidEmail
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", ErrInvalidEmail
			}
		}
	}
	return e, nil
}

// CheckPasswordPolicy applies PD11 to the NFC form of password: 12-128 code points and, when email
// is non-empty, not equal to it (sign-up only; at reset Go holds only the binding, so equals_email
// is NOT_IMPLEMENTED there, contract §11). The breach check is separate (network).
func CheckPasswordPolicy(password, email string) error {
	pw := norm.NFC.String(password)
	n := utf8.RuneCountInString(pw)
	if n < minPasswordRunes {
		return PolicyError{Reason: "too_short"}
	}
	if n > maxPasswordRunes {
		return PolicyError{Reason: "too_long"}
	}
	if email != "" && strings.EqualFold(pw, strings.TrimSpace(email)) {
		return PolicyError{Reason: "equals_email"}
	}
	return nil
}

// NewCode returns a uniform 6-digit code from crypto/rand.
func NewCode() (string, error) { return newCodeFrom(rand.Reader) }

// newCodeFrom draws uint32 values and rejects the biased tail so every digit string is equally
// likely (a plain `% 10^6` would favour low codes). The reader seam lets PA01 feed a value from
// the biased tail and prove it is rejected.
func newCodeFrom(r io.Reader) (string, error) {
	const limit = (uint64(1) << 32) / 1_000_000 * 1_000_000 // 4_294_000_000
	var b [4]byte
	for {
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return "", err
		}
		v := uint64(binary.BigEndian.Uint32(b[:]))
		if v < limit {
			return fmt.Sprintf("%06d", v%1_000_000), nil
		}
	}
}

// CodeHMAC is HMAC-SHA256(pepper, bindingHash || code): what the challenge row stores instead of
// the code (PD4). The binding is in the input so a code phished from the mail is useless without
// the browser cookie.
func CodeHMAC(pepper, bindingHash []byte, code string) []byte {
	m := hmac.New(sha256.New, pepper)
	m.Write(bindingHash)
	m.Write([]byte(code))
	return m.Sum(nil)
}

// validCode reports whether code is exactly six ASCII digits.
func validCode(code string) bool {
	if len(code) != 6 {
		return false
	}
	for i := 0; i < 6; i++ {
		if code[i] < '0' || code[i] > '9' {
			return false
		}
	}
	return true
}
