// registrar.go: the five registry operations (contracts/stripe-psp-v1.md §0.2 registrar SQL names,
// §13, integrator rulings 7, 8 and 10 of docs/delivery/units/stripe-b1-rulings.md).
// Ordering rule for every operation: validate input, then provider verification, then seal, then
// exactly one SQL call. Nothing is written when verification fails.

package stripeadmin

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"livecommerce/internal/command"
	"livecommerce/internal/integrations/accounts"
	"livecommerce/internal/integrations/psp/stripe"
	"livecommerce/internal/platform"
)

// Fixed errors: they never wrap a driver, Stripe or SQL message (DSNs, keys and rows can hide in them).
var (
	ErrConfig   = errors.New("stripeadmin: config")
	ErrDatabase = errors.New("stripeadmin: database")
	ErrRejected = errors.New("stripeadmin: rejected")
	ErrProvider = errors.New("stripeadmin: provider")
)

const (
	sqlBudget       = 15 * time.Second
	qualifyBudget   = 60 * time.Second // three Stripe calls of up to 10 s each plus SQL
	envSandbox      = "SANDBOX"        // Open registers SANDBOX; the SQL CHECKs accept SANDBOX and LIVE
	envLive         = "LIVE"           // OpenLive registers LIVE (stripe-live-enable-v1 §5.2)
	qualifyValidFor = "30 days"        // §0.2: expiry in (now, observed_at + 30 days]
)

var (
	accountPattern  = regexp.MustCompile(`^acct_[A-Za-z0-9]{1,59}$`)
	countryPattern  = regexp.MustCompile(`^[A-Z]{2}$`)
	currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)
	// refPattern is the approval_ref / revoke_ref grammar of payments.stripe_live_approvals (§3.2).
	refPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)
)

// Scope is the operator-chosen owner scope; the SQL validates the owner membership of Principal.
type Scope struct{ TenantID, StoreID, PrincipalID string }

// EndpointInput.AccountID is optional: the account is derived from the registered connection
// (§0.2); a non-empty AccountID is only an operator cross-check and must equal it.
type EndpointInput struct {
	ConnectionID, EndpointID, AccountID, Profile string
	ExpectedVersion                              int64 // EndpointID "" iff ExpectedVersion==0
	Enabled                                      bool
	Secrets                                      accounts.StripeWebhookSecrets
}

type QualifyInput struct {
	ConnectionID, AccountID, SecretKey, Profile, Currency, ReturnURL string
	ExpectedVersion, AmountMinor                                     int64
}

type MethodInput struct {
	MarketID, Country, ConnectionID, QualificationID string
	ExpectedVersion                                  int64
	Enabled, Visible                                 bool
	Sort                                             int32
	MinMinor, MaxMinor                               int64
	NameHans, NameHant, NameEN                       string
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Registrar owns its pool. apiKeys seals Stripe API keys, signingKeys seals webhook secrets; either
// may be nil when the command never needs it (the operation then fails with ErrConfig).
type Registrar struct {
	db          queryRower
	closePool   func()
	apiKeys     *accounts.Keyring
	signingKeys *accounts.Keyring
	transport   http.RoundTripper
	// live is the owner's pair; complete only on a registrar from OpenLive. It selects environment LIVE for
	// register/rotate/qualify/webhook and enables LiveApprove/LiveCanary. LiveRevoke and SetMethod never need it.
	live stripe.LiveApproval
	// newProvider is a test seam only (unexported): nil means the real adapter. It exists because a LIVE client
	// can never be built over a mock transport, yet LiveApprove/Qualify(LIVE) still need Go-side unit tests.
	newProvider func(stripe.Config) (provider, error)
}

// provider is the slice of *stripe.Client the registrar uses.
type provider interface {
	VerifyAccount(ctx context.Context) (stripe.CallMeta, error)
	AccountReadiness(ctx context.Context) (stripe.Readiness, stripe.CallMeta, error)
	ProbeCheckout(ctx context.Context, qualificationID, currency string, amountMinor int64,
		returnURL string) (string, stripe.CallMeta, error)
}

// environment is the Stripe environment this registrar registers and operates: LIVE only after OpenLive.
func (r *Registrar) environment() string {
	if r.live.Enabled {
		return envLive
	}
	return envSandbox
}

// profileAllowed: a SANDBOX registrar takes PROVIDER_MOCK and SANDBOX profiles, a LIVE registrar only LIVE
// (S7: Open keeps refusing LIVE; SQL also requires (account environment = LIVE) = (profile = LIVE)).
func (r *Registrar) profileAllowed(profile string) bool {
	if r.live.Enabled {
		return profile == "LIVE"
	}
	return profile == "PROVIDER_MOCK" || profile == "SANDBOX"
}

func (Registrar) String() string     { return "stripeadmin.Registrar{redacted}" }
func (r Registrar) GoString() string { return r.String() }
func (Registrar) MarshalJSON() ([]byte, error) {
	return []byte(`"stripeadmin.Registrar{redacted}"`), nil
}

// Open connects with platform.OpenStripeRegistrarPool (masked errors). mockTransport is for tests
// and the PROVIDER_MOCK assembly only: none, or exactly one non-nil (stripe.NewWithMockTransport);
// the deployable CLI never passes one, so it always dials api.stripe.com through stripe.New.
func Open(ctx context.Context, dsn string, apiKeys, signingKeys *accounts.Keyring,
	mockTransport ...http.RoundTripper) (*Registrar, error) {
	if ctx == nil || len(mockTransport) > 1 || (len(mockTransport) == 1 && mockTransport[0] == nil) {
		return nil, ErrConfig
	}
	pool, err := platform.OpenStripeRegistrarPool(ctx, dsn)
	if err != nil {
		return nil, ErrDatabase
	}
	r := newRegistrar(pool, apiKeys, signingKeys, mockTransport...)
	r.closePool = pool.Close
	return r, nil
}

// OpenLive is Open for a LIVE registrar (stripe-live-enable-v1 §5.2): it needs the owner's complete
// flag+reference pair, never takes a mock transport, and its Register/Rotate use environment LIVE (rk_live_
// keys only), Qualify/SetWebhookEndpoint use profile LIVE, and LiveApprove/LiveCanary are available.
// The pair is only a deployment gate here; the per-store gate is the SQL approval row.
func OpenLive(ctx context.Context, dsn string, apiKeys, signingKeys *accounts.Keyring,
	live stripe.LiveApproval) (*Registrar, error) {
	if ctx == nil || !live.Valid() {
		return nil, ErrConfig
	}
	pool, err := platform.OpenStripeRegistrarPool(ctx, dsn)
	if err != nil {
		return nil, ErrDatabase
	}
	r := newRegistrar(pool, apiKeys, signingKeys)
	r.live = live
	r.closePool = pool.Close
	return r, nil
}

func newRegistrar(db queryRower, apiKeys, signingKeys *accounts.Keyring, mockTransport ...http.RoundTripper) *Registrar {
	r := &Registrar{db: db, apiKeys: apiKeys, signingKeys: signingKeys}
	if len(mockTransport) == 1 {
		r.transport = mockTransport[0]
	}
	return r
}

// Close releases the owned pool; safe on nil and repeatable.
func (r *Registrar) Close() {
	if r != nil && r.closePool != nil {
		r.closePool()
		r.closePool = nil
	}
}

func validScope(s Scope) bool {
	return command.ValidID(s.TenantID) && command.ValidID(s.StoreID) && command.ValidID(s.PrincipalID)
}

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", ErrConfig
	}
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// sqlError collapses a driver error into the fixed vocabulary. 22023 (invalid input), PT409
// (state/version conflict) and 42501 (scope/ACL refusal) are the definers' rejections; class 23
// (unique/check/FK, e.g. a second account for one store) is also an operator-input rejection.
func sqlError(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		if pg.Code == "22023" || pg.Code == "PT409" || pg.Code == "42501" || (len(pg.Code) == 5 && pg.Code[:2] == "23") {
			return ErrRejected
		}
	}
	return ErrDatabase
}

func (r *Registrar) scan(ctx context.Context, dest any, sql string, args ...any) error {
	return r.scanRow(ctx, []any{dest}, sql, args...)
}

func (r *Registrar) scanRow(ctx context.Context, dest []any, sql string, args ...any) error {
	bounded, cancel := context.WithTimeout(ctx, sqlBudget)
	defer cancel()
	if err := r.db.QueryRow(bounded, sql, args...).Scan(dest...); err != nil {
		return sqlError(err)
	}
	return nil
}

// registeredAccount reads the connection's registered account (payments.stripe_endpoint_account:
// registry_writer definer, EXECUTE registrar, read-only). The registered account is immutable (no
// rebind), so it is the only value an envelope AAD or a probe may be bound to; operator env is just
// an assertion that must equal it (S5).
func (r *Registrar) registeredAccount(ctx context.Context, s Scope, connectionID string) (string, error) {
	var account string
	if err := r.scan(ctx, &account, `SELECT payments.stripe_endpoint_account($1::uuid,$2::uuid,$3::uuid,$4::uuid)`,
		s.TenantID, s.StoreID, s.PrincipalID, connectionID); err != nil {
		return "", err
	}
	if !accountPattern.MatchString(account) {
		return "", ErrDatabase
	}
	return account, nil
}

// providerClient builds the Stage-A client for the operator's key. The adapter's admission matrix (§5.1)
// decides: a live key on a SANDBOX registrar, a test key on a LIVE one, sk_live_ anywhere and a missing pair
// are all ErrLiveRefused, surfaced as ErrRejected. A mock transport is never used for LIVE (admit refuses it).
func (r *Registrar) providerClient(secretKey, accountID string) (provider, error) {
	cfg := stripe.Config{SecretKey: secretKey, AccountID: accountID, Environment: r.environment()}
	if r.live.Enabled {
		cfg.Live = r.live
	}
	var c provider
	var err error
	switch {
	case r.newProvider != nil:
		// Test seam: still run the adapter's admission matrix (no I/O) so key-mode and pair refusals hold in tests.
		if _, err = stripe.New(cfg); err == nil {
			c, err = r.newProvider(cfg)
		}
	case r.transport != nil:
		var sc *stripe.Client
		sc, err = stripe.NewWithMockTransport(cfg, r.transport)
		c = sc
	default:
		var sc *stripe.Client
		sc, err = stripe.New(cfg)
		c = sc
	}
	switch {
	case errors.Is(err, stripe.ErrLiveRefused):
		return nil, ErrRejected
	case err != nil:
		return nil, ErrConfig
	}
	return c, nil
}

// verify implements ruling 8: GET /v1/account with the supplied key must return the operator's
// account id. The adapter binds the key mode to the environment (test keys in SANDBOX, rk_live_ in LIVE).
func (r *Registrar) verify(ctx context.Context, secretKey, accountID string) error {
	c, err := r.providerClient(secretKey, accountID)
	if err != nil {
		return err
	}
	if _, err := c.VerifyAccount(ctx); err != nil {
		if errors.Is(err, stripe.ErrAuthentication) {
			return ErrRejected // key belongs to another account or is invalid: nothing may be written
		}
		return ErrProvider
	}
	return nil
}

// Register verifies the account with its key, then atomically creates the binding, account and
// credential version 1 (integration.register_stripe_account; owner membership checked in SQL).
func (r *Registrar) Register(ctx context.Context, s Scope, accountID, secretKey string) (string, error) {
	if r == nil || r.db == nil || ctx == nil || r.apiKeys == nil || !validScope(s) || !accountPattern.MatchString(accountID) {
		return "", ErrConfig
	}
	connection, err := newUUID()
	if err != nil {
		return "", err
	}
	binding, err := newUUID()
	if err != nil {
		return "", err
	}
	bounded, cancel := context.WithTimeout(ctx, qualifyBudget)
	defer cancel()
	if err := r.verify(bounded, secretKey, accountID); err != nil {
		return "", err
	}
	keyID, nonce, ciphertext, err := r.apiKeys.SealStripeAPI(accounts.StripeAPIScope{TenantID: s.TenantID,
		StoreID: s.StoreID, ConnectionID: connection, Environment: r.environment(), AccountID: accountID,
		CredentialVersion: 1}, accounts.StripeAPICredentials{SecretKey: secretKey})
	if err != nil {
		return "", ErrConfig
	}
	var out string
	if err := r.scan(bounded, &out, `SELECT integration.register_stripe_account($1::uuid,$2::uuid,$3::uuid,$4::uuid,$5::uuid,
		$6::text,$7::text,$8::text,$9::bytea,$10::bytea)::text`, s.TenantID, s.StoreID, s.PrincipalID,
		connection, binding, r.environment(), accountID, keyID, nonce, ciphertext); err != nil {
		return "", err
	}
	if out != connection {
		return "", ErrDatabase
	}
	return connection, nil
}

// storedCredential opens the connection's head credential (version must equal expectedVersion; the
// SQL answers PT409 otherwise) with the registrar's API keyring; the AAD binds the registered account.
func (r *Registrar) storedCredential(ctx context.Context, s Scope, connectionID string,
	expectedVersion int64) (account, secretKey string, err error) {
	var keyID string
	var nonce, ciphertext []byte
	if err := r.scanRow(ctx, []any{&account, &keyID, &nonce, &ciphertext},
		`SELECT account_id,key_id,nonce,ciphertext FROM payments.stripe_registrar_credential(
		 $1::uuid,$2::uuid,$3::uuid,$4::uuid,$5::bigint)`,
		s.TenantID, s.StoreID, s.PrincipalID, connectionID, expectedVersion); err != nil {
		return "", "", err
	}
	if !accountPattern.MatchString(account) {
		return "", "", ErrDatabase
	}
	creds, err := r.apiKeys.OpenStripeAPI(accounts.StripeAPIScope{TenantID: s.TenantID, StoreID: s.StoreID,
		ConnectionID: connectionID, Environment: r.environment(), AccountID: account,
		CredentialVersion: expectedVersion}, keyID, nonce, ciphertext)
	if err != nil {
		return "", "", ErrConfig // custody cannot open it: wrong keyring or a mismatching envelope
	}
	return account, creds.SecretKey, nil
}

// Rotate appends credential version expectedVersion+1 (integration.rotate_stripe_key locks the
// account and requires the exact previous version). The AAD account is the REGISTERED account read
// from the database (S5); accountID is only the operator's assertion and must equal it, and the key
// is verified against the registered account, so a foreign account's key never becomes the head.
func (r *Registrar) Rotate(ctx context.Context, s Scope, connectionID string, expectedVersion int64,
	accountID, secretKey string) (int64, error) {
	if r == nil || r.db == nil || ctx == nil || r.apiKeys == nil || !validScope(s) || !command.ValidID(connectionID) ||
		expectedVersion < 1 || !accountPattern.MatchString(accountID) {
		return 0, ErrConfig
	}
	bounded, cancel := context.WithTimeout(ctx, qualifyBudget)
	defer cancel()
	account, err := r.registeredAccount(bounded, s, connectionID)
	if err != nil {
		return 0, err
	}
	if accountID != account {
		return 0, ErrRejected
	}
	if err := r.verify(bounded, secretKey, account); err != nil {
		return 0, err
	}
	keyID, nonce, ciphertext, err := r.apiKeys.SealStripeAPI(accounts.StripeAPIScope{TenantID: s.TenantID,
		StoreID: s.StoreID, ConnectionID: connectionID, Environment: r.environment(), AccountID: account,
		CredentialVersion: expectedVersion + 1}, accounts.StripeAPICredentials{SecretKey: secretKey})
	if err != nil {
		return 0, ErrConfig
	}
	var version int64
	if err := r.scan(bounded, &version, `SELECT integration.rotate_stripe_key($1::uuid,$2::uuid,$3::uuid,$4::uuid,
		$5::bigint,$6::text,$7::bytea,$8::bytea)`, s.TenantID, s.StoreID, s.PrincipalID, connectionID,
		expectedVersion, keyID, nonce, ciphertext); err != nil {
		return 0, err
	}
	if version != expectedVersion+1 {
		return 0, ErrDatabase // the sealed AAD bound expected+1; anything else is unreadable
	}
	return version, nil
}

// SetWebhookEndpoint seals the signing secrets under key_version expected+1 and calls
// payments.set_stripe_webhook_endpoint. Disabling re-seals too, so a caller must supply secrets.
// The AAD account comes from payments.stripe_endpoint_account (the registered connection, §0.2),
// the same value set_stripe_webhook_endpoint stores, so the envelope always opens at ingress; no
// process env account is trusted here (least privilege: the CLI webhook reads no STRIPE_ACCOUNT_ID).
func (r *Registrar) SetWebhookEndpoint(ctx context.Context, s Scope, in EndpointInput) (string, int64, error) {
	if r == nil || r.db == nil || ctx == nil || r.signingKeys == nil || !validScope(s) ||
		!command.ValidID(in.ConnectionID) || (in.AccountID != "" && !accountPattern.MatchString(in.AccountID)) ||
		!r.profileAllowed(in.Profile) || in.ExpectedVersion < 0 ||
		(in.ExpectedVersion == 0) != (in.EndpointID == "") || (in.EndpointID != "" && !command.ValidID(in.EndpointID)) {
		return "", 0, ErrConfig
	}
	endpoint := in.EndpointID
	if endpoint == "" {
		var err error
		if endpoint, err = newUUID(); err != nil {
			return "", 0, err
		}
	}
	account, err := r.registeredAccount(ctx, s, in.ConnectionID)
	if err != nil {
		return "", 0, err
	}
	if in.AccountID != "" && in.AccountID != account {
		return "", 0, ErrRejected
	}
	next := in.ExpectedVersion + 1
	keyID, nonce, ciphertext, err := r.signingKeys.SealStripeWebhook(accounts.StripeWebhookScope{TenantID: s.TenantID,
		StoreID: s.StoreID, ConnectionID: in.ConnectionID, EndpointID: endpoint, Environment: r.environment(),
		AccountID: account, Profile: in.Profile, KeyVersion: next}, in.Secrets)
	if err != nil {
		return "", 0, ErrConfig
	}
	var version int64
	if err := r.scan(ctx, &version, `SELECT payments.set_stripe_webhook_endpoint($1::uuid,$2::uuid,$3::uuid,$4::uuid,
		$5::uuid,$6::text,$7::bigint,$8::boolean,$9::text,$10::bytea,$11::bytea)`, s.TenantID, s.StoreID,
		s.PrincipalID, in.ConnectionID, endpoint, in.Profile, in.ExpectedVersion, in.Enabled, keyID,
		nonce, ciphertext); err != nil {
		return "", 0, err
	}
	if version != next {
		return "", 0, ErrDatabase
	}
	return endpoint, version, nil
}

// Qualify records method-qualification evidence for the credential version the operator expects.
// SANDBOX and LIVE: open the STORED credential at expected_version (payments.stripe_registrar_credential,
// head only) and probe the connection's REGISTERED account with it (S4): verify the account, run
// stripe.ProbeCheckout (create at now+31m, expire, retrieve expired+unpaid+livemode==environment; no charge is
// possible, nobody completes the session) and store evidence "stripe-probe:<session id>" (cs_live_… for LIVE,
// which the SQL requires). in.AccountID / in.SecretKey are optional operator
// assertions that must equal the stored values, so an old key or another account's key cannot
// qualify version N. PROVIDER_MOCK: no network, evidence "provider-mock:<qualification>". The SQL
// rechecks the head version after the probe. observed_at/expires_at come from the DB transaction
// clock (now(), now()+30 days) so operator-host clock skew cannot violate the SQL's "not in the
// future" and "<= observed+30 days" checks.
func (r *Registrar) Qualify(ctx context.Context, s Scope, in QualifyInput) (string, error) {
	if r == nil || r.db == nil || ctx == nil || !validScope(s) || !command.ValidID(in.ConnectionID) ||
		in.ExpectedVersion < 1 || !r.profileAllowed(in.Profile) {
		return "", ErrConfig
	}
	qualification, err := newUUID()
	if err != nil {
		return "", err
	}
	bounded, cancel := context.WithTimeout(ctx, qualifyBudget)
	defer cancel()
	evidence := "provider-mock:" + qualification
	if in.Profile == "SANDBOX" || in.Profile == "LIVE" {
		if r.apiKeys == nil || (in.AccountID != "" && !accountPattern.MatchString(in.AccountID)) {
			return "", ErrConfig
		}
		account, secret, err := r.storedCredential(bounded, s, in.ConnectionID, in.ExpectedVersion)
		if err != nil {
			return "", err
		}
		if (in.AccountID != "" && in.AccountID != account) ||
			(in.SecretKey != "" && subtle.ConstantTimeCompare([]byte(in.SecretKey), []byte(secret)) != 1) {
			return "", ErrRejected
		}
		client, err := r.providerClient(secret, account)
		if err != nil {
			return "", err
		}
		if _, err := client.VerifyAccount(bounded); err != nil {
			if errors.Is(err, stripe.ErrAuthentication) {
				return "", ErrRejected
			}
			return "", ErrProvider
		}
		session, _, err := client.ProbeCheckout(bounded, qualification, in.Currency, in.AmountMinor, in.ReturnURL)
		switch {
		case errors.Is(err, stripe.ErrInvalid) || errors.Is(err, stripe.ErrLiveRefused):
			return "", ErrRejected
		case err != nil:
			return "", ErrProvider
		}
		evidence = "stripe-probe:" + session
	}
	var out string
	if err := r.scan(bounded, &out, `SELECT payments.qualify_stripe_method($1::uuid,$2::uuid,$3::uuid,$4::uuid,$5::uuid,
		$6::bigint,$7::text,$8::text,now(),now()+interval '`+qualifyValidFor+`')::text`, s.TenantID, s.StoreID,
		s.PrincipalID, qualification, in.ConnectionID, in.ExpectedVersion, in.Profile, evidence); err != nil {
		return "", err
	}
	if out != qualification {
		return "", ErrDatabase
	}
	return qualification, nil
}

// SetMethod appends a stripe_checkout method revision (payments.set_stripe_method). Currency is
// derived from the locked market in SQL; no caller-supplied currency exists (§0.2).
func (r *Registrar) SetMethod(ctx context.Context, s Scope, in MethodInput) (int64, error) {
	if r == nil || r.db == nil || ctx == nil || !validScope(s) || !command.ValidID(in.MarketID) ||
		!command.ValidID(in.ConnectionID) || !command.ValidID(in.QualificationID) ||
		!countryPattern.MatchString(in.Country) || in.ExpectedVersion < 0 || in.Sort < 0 || in.Sort > 1000 {
		return 0, ErrConfig
	}
	// Amount bounds are operator input, not config: the definer raises 22023 (max<min) / PT409
	// (outside the currency range) for these, which sqlError maps to ErrRejected. Refusing early
	// must keep that class so the CLI reports the same outcome with or without the round trip.
	if in.MinMinor < 1 || in.MaxMinor < in.MinMinor {
		return 0, ErrRejected
	}
	var version int64
	if err := r.scan(ctx, &version, `SELECT payments.set_stripe_method($1::uuid,$2::uuid,$3::uuid,$4::uuid,$5::text,
		$6::uuid,$7::uuid,$8::bigint,$9::boolean,$10::boolean,$11::integer,$12::bigint,$13::bigint,
		$14::text,$15::text,$16::text)`, s.TenantID, s.StoreID, s.PrincipalID, in.MarketID, in.Country,
		in.ConnectionID, in.QualificationID, in.ExpectedVersion, in.Enabled, in.Visible, in.Sort,
		in.MinMinor, in.MaxMinor, in.NameHans, in.NameHant, in.NameEN); err != nil {
		return 0, err
	}
	return version, nil
}

var _ queryRower = (*pgxpool.Pool)(nil)
