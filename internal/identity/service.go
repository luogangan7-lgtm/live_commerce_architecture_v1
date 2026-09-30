// Package identity owns merchant login and first-store bootstrap: the OIDC login (Service) and the
// email + password + emailed-code login (Passwords, contracts/merchant-password-auth-v1.md). Its
// dedicated database pool is an authentication authority (role commerce_identity, EXECUTE on the
// identity.* definers only), never a business runtime pool.
//
// It never links an OIDC principal and a password principal by email (PD9), never retries or
// queues mail (one send per challenge, PD7/I06), never persists a plaintext code or password, and
// never takes a tenant or store id from the client (I01).
//
// External hosts: api.pwnedpasswords.com (Have I Been Pwned k-anonymity range API, sign-up and
// reset only, so a breached password is refused without ever sending the password or its full
// hash; fail-open on any error, ruling Q3). The SMTP host is dialled only by internal/mail, via the
// Mailer seam. The OIDC issuer is dialled only by internal/oidclogin.
package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"livecommerce/internal/oidclogin"
)

var (
	ErrInvalid      = errors.New("invalid identity request")
	ErrUnauthorized = errors.New("unauthorized")
	ErrUnavailable  = errors.New("identity service unavailable")
	ErrConflict     = errors.New("initial store already requested")
	ErrDisabled     = errors.New("onboarding is disabled")
)

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{8,128}$`)
var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

// Provider is the verified OIDC adapter, replaceable only for isolated tests.
// Never construct it from user input or accept unverified issuer/subject in HTTP.
type Provider interface {
	AuthorizationURL(state, nonce, verifier string) (string, error)
	Exchange(ctx context.Context, code, nonce, verifier string) (oidclogin.Identity, error)
}

type Policy struct {
	// PasswordLogin relaxes New so a nil Provider is accepted (ruling R-4, A8): OIDC becomes optional
	// when password login is enabled; Start/Complete then return ErrDisabled.
	PasswordLogin     bool
	ProviderKey       string
	SessionTTL        time.Duration
	OnboardingEnabled bool
	Currencies        []string
}

type Service struct {
	pool       *pgxpool.Pool
	provider   Provider
	policy     Policy
	currencies map[string]bool
}

// New requires an OpenIdentityPool result. Production wiring remains fail-closed
// until its IdP/registration policy is provisioned; there is no fixture fallback.
func New(pool *pgxpool.Pool, provider Provider, policy Policy) (*Service, error) {
	oidcConfigured := provider != nil
	if pool == nil || (!oidcConfigured && !policy.PasswordLogin) || (oidcConfigured && len(policy.ProviderKey) == 0) || len(policy.ProviderKey) > 128 || policy.SessionTTL < 5*time.Minute || policy.SessionTTL > 24*time.Hour {
		return nil, ErrInvalid
	}
	currencies := make(map[string]bool)
	for _, c := range policy.Currencies {
		if !currencyPattern.MatchString(c) {
			return nil, ErrInvalid
		}
		currencies[c] = true
	}
	if policy.OnboardingEnabled && len(currencies) == 0 {
		return nil, ErrInvalid
	}
	return &Service{pool: pool, provider: provider, policy: policy, currencies: currencies}, nil
}

// Flow.Binding is a secret for the server's HttpOnly binding cookie. It must
// not be returned to client JavaScript, persisted to localStorage or logged.
type Flow struct {
	URL       string
	State     string
	Binding   string `json:"-"`
	ExpiresAt time.Time
}

func (s *Service) Start(ctx context.Context) (Flow, error) {
	if s.provider == nil { // password-only deployment (A8): the OIDC routes stay mounted but disabled
		return Flow{}, ErrDisabled
	}
	state, binding, nonce, verifier := randomToken(), randomToken(), randomToken(), randomToken()
	authURL, err := s.provider.AuthorizationURL(state, nonce, verifier)
	if err != nil {
		return Flow{}, ErrUnavailable
	}
	flow := Flow{URL: authURL, State: state, Binding: binding}
	err = s.transaction(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT identity.start_login_flow($1,$2,$3,$4,$5)`, digest(state), digest(binding), s.policy.ProviderKey, nonce, verifier).Scan(&flow.ExpiresAt)
	})
	if err != nil {
		return Flow{}, ErrUnavailable
	}
	return flow, nil
}

// Session.Token leaves this package once, for server-to-server use and a secure
// cookie. Only its SHA256 hash persists; tokens/claims must not enter diagnostics.
type Session struct {
	Token       string `json:"-"`
	PrincipalID string
	ExpiresAt   time.Time
}

func (s *Service) Complete(ctx context.Context, state, binding, code string) (Session, error) {
	if s.provider == nil {
		return Session{}, ErrDisabled
	}
	if !validToken(state) || !validToken(binding) || len(code) == 0 || len(code) > 4096 {
		return Session{}, ErrUnauthorized
	}
	var nonce, verifier string
	// Commit consumption BEFORE external exchange. A timeout/lost response is not
	// safely retryable: the user starts a fresh flow rather than reusing the code.
	err := s.transaction(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT nonce,verifier FROM identity.consume_login_flow($1,$2,$3)`, digest(state), digest(binding), s.policy.ProviderKey).Scan(&nonce, &verifier)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrUnauthorized
	}
	if err != nil {
		return Session{}, ErrUnavailable
	}
	subject, err := s.provider.Exchange(ctx, code, nonce, verifier)
	if err != nil || len(subject.Issuer) == 0 || len(subject.Issuer) > 2048 || len(subject.Subject) == 0 || len(subject.Subject) > 255 || !utf8.ValidString(subject.Issuer) || !utf8.ValidString(subject.Subject) {
		return Session{}, ErrUnauthorized
	}
	result := Session{Token: randomToken()}
	err = s.transaction(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT principal_id::text,expires_at FROM identity.issue_merchant_session($1,$2,$3,$4)`, subject.Issuer, subject.Subject, digest(result.Token), int64(s.policy.SessionTTL/time.Second)).Scan(&result.PrincipalID, &result.ExpiresAt)
	})
	err = translateError(err)
	if errors.Is(err, ErrUnauthorized) {
		return Session{}, ErrUnauthorized
	}
	if err != nil {
		return Session{}, ErrUnavailable
	}
	return result, nil
}

type StoreRequest struct {
	TenantName    string `json:"tenant_name"`
	StoreName     string `json:"store_name"`
	WarehouseName string `json:"warehouse_name"`
	Currency      string `json:"currency"`
}

type Store struct {
	TenantID    string `json:"tenant_id"`
	StoreID     string `json:"store_id"`
	WarehouseID string `json:"warehouse_id"`
}

func (s *Service) CreateInitialStore(ctx context.Context, token, key string, input StoreRequest) (Store, error) {
	if !s.policy.OnboardingEnabled {
		return Store{}, ErrDisabled
	}
	if !validToken(token) {
		return Store{}, ErrUnauthorized
	}
	input.TenantName = strings.TrimSpace(input.TenantName)
	input.StoreName = strings.TrimSpace(input.StoreName)
	input.WarehouseName = strings.TrimSpace(input.WarehouseName)
	if !keyPattern.MatchString(key) || !validName(input.TenantName) || !validName(input.StoreName) || !validName(input.WarehouseName) || !s.currencies[input.Currency] {
		return Store{}, ErrInvalid
	}
	canonical, _ := json.Marshal(input)
	hash := sha256.Sum256(canonical)
	var result Store
	err := s.transaction(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT tenant_id::text,store_id::text,warehouse_id::text FROM identity.create_initial_store($1,$2,$3,$4,$5,$6,$7)`, digest(token), key, hash[:], input.TenantName, input.StoreName, input.WarehouseName, input.Currency).Scan(&result.TenantID, &result.StoreID, &result.WarehouseID)
	})
	err = translateError(err)
	if errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrConflict) {
		return Store{}, err
	}
	if err != nil {
		return Store{}, ErrUnavailable
	}
	return result, nil
}

func (s *Service) Logout(ctx context.Context, token string) error {
	if !validToken(token) {
		return ErrUnauthorized
	}
	err := s.transaction(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT identity.revoke_merchant_session($1)`, digest(token))
		return err
	})
	if err != nil {
		return ErrUnavailable
	}
	return nil
}

func (s *Service) transaction(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
	return withTx(ctx, s.pool, fn)
}

// withTx runs fn in one bounded transaction (5 s statement/lock/idle limits) on the identity pool.
// Shared by Service (OIDC) and Passwords so both keep the same fail-fast limits.
func withTx(ctx context.Context, pool *pgxpool.Pool, fn func(context.Context, pgx.Tx) error) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		_ = tx.Rollback(cleanup)
	}()
	if _, err = tx.Exec(ctx, `SET LOCAL statement_timeout='5s'; SET LOCAL lock_timeout='1s'; SET LOCAL idle_in_transaction_session_timeout='5s'`); err != nil {
		return err
	}
	if err = fn(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func translateError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "PT401":
			return ErrUnauthorized
		case "PT409":
			return ErrConflict
		case "PT400":
			return ErrInvalid
		}
	}
	return err
}

func randomToken() string {
	// Go >=1.24 Read always fills the buffer and never returns an error. An
	// underlying entropy failure terminates the process, not a zero-token path.

	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
func digest(s string) []byte { hash := sha256.Sum256([]byte(s)); return hash[:] }
func validToken(s string) bool {
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	return len(s) == 43 && err == nil && len(b) == 32
}
func validName(s string) bool {
	return utf8.ValidString(s) && utf8.RuneCountInString(s) >= 1 && utf8.RuneCountInString(s) <= 120 && strings.IndexFunc(s, unicode.IsControl) == -1
}
