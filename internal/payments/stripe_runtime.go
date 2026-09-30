// stripe_runtime.go owns lease-bound Stripe client material for payment workers.
// It never reads environment credentials, stores a process-global Stripe key, or calls Stripe at startup.
// LIVE (contracts/stripe-live-enable-v1.md §5.2): only NewLiveStripeRuntime builds a LIVE runtime, with the
// owner's flag+reference pair and the real transport; NewStripeRuntime keeps refusing LIVE (S7).
// External: api.stripe.com through internal/integrations/psp/stripe, one client per claimed operation.

package payments

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"livecommerce/internal/command"
	"livecommerce/internal/integrations/accounts"
	"livecommerce/internal/integrations/core"
	"livecommerce/internal/integrations/psp/stripe"
	"livecommerce/internal/platform"
)

var (
	errStripeRuntimeConfig   = errors.New("stripe_runtime_invalid_config")
	errStripeRuntimeDatabase = errors.New("stripe_runtime_database")
	errStripeMaterial        = errors.New("stripe_material_unavailable")
)

type StripeRuntime struct {
	pool      *pgxpool.Pool
	keys      *accounts.Keyring
	profile   string
	transport http.RoundTripper
	live      stripe.LiveApproval // zero unless profile == "LIVE" (NewLiveStripeRuntime)
}

// ProfileEnvironment maps a payment profile to the Stripe environment its rows must carry (S6, LD1):
// PROVIDER_MOCK and SANDBOX -> SANDBOX, LIVE -> LIVE, anything else is a configuration error (ok=false).
// It is the single copy of this rule; every SANDBOX/LIVE guard in Go calls it.
func ProfileEnvironment(profile string) (string, bool) {
	switch profile {
	case "PROVIDER_MOCK", "SANDBOX":
		return "SANDBOX", true
	case "LIVE":
		return "LIVE", true
	}
	return "", false
}

// stripeClientConfig builds the adapter config for one claimed operation. The LIVE pair rides along only
// for a LIVE environment, so a SANDBOX runtime can never present it (admit() would call that a mismatch).
func (s *StripeRuntime) stripeClientConfig(secretKey, accountID, environment string) stripe.Config {
	cfg := stripe.Config{SecretKey: secretKey, AccountID: accountID, Environment: environment}
	if environment == "LIVE" {
		cfg.Live = s.live
	}
	return cfg
}

func (StripeRuntime) String() string     { return "payments.StripeRuntime{redacted}" }
func (s StripeRuntime) GoString() string { return s.String() }
func (StripeRuntime) MarshalJSON() ([]byte, error) {
	return []byte(`"payments.StripeRuntime{redacted}"`), nil
}

// NewStripeRuntime checks the worker's role, queue and SQL capabilities only.
// The exact historical credential is loaded after each operation is claimed.
func NewStripeRuntime(ctx context.Context, pool *pgxpool.Pool, keys *accounts.Keyring,
	profile string, mockTransport ...http.RoundTripper) (*StripeRuntime, error) {
	// S7: LIVE is deliberately not admitted here; it has its own constructor with the pair.
	if ctx == nil || pool == nil || keys == nil ||
		(profile != "PROVIDER_MOCK" && profile != "SANDBOX") ||
		(profile == "PROVIDER_MOCK" && (len(mockTransport) != 1 || mockTransport[0] == nil)) ||
		(profile == "SANDBOX" && len(mockTransport) != 0) {
		return nil, errStripeRuntimeConfig
	}
	var transport http.RoundTripper
	if profile == "PROVIDER_MOCK" {
		transport = mockTransport[0]
	}
	return newStripeRuntime(ctx, pool, keys, profile, transport, stripe.LiveApproval{})
}

// NewLiveStripeRuntime is the only way to a LIVE Stripe worker runtime (stripe-live-enable-v1 §5.2): profile
// LIVE, the real transport (a mock never admits a live key), and a valid flag+reference pair. It checks the
// same role/queue/SQL capabilities as NewStripeRuntime and makes no provider call.
func NewLiveStripeRuntime(ctx context.Context, pool *pgxpool.Pool, keys *accounts.Keyring,
	live stripe.LiveApproval) (*StripeRuntime, error) {
	if ctx == nil || pool == nil || keys == nil || !live.Valid() {
		return nil, errStripeRuntimeConfig
	}
	return newStripeRuntime(ctx, pool, keys, "LIVE", nil, live)
}

func newStripeRuntime(ctx context.Context, pool *pgxpool.Pool, keys *accounts.Keyring, profile string,
	transport http.RoundTripper, live stripe.LiveApproval) (*StripeRuntime, error) {
	if err := platform.ValidateWorkerPool(ctx, pool); err != nil {
		return nil, errStripeRuntimeDatabase
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var ready, capabilities bool
	// integration.payment_queue_ready audits the installed River route/guards and queues.
	if err := pool.QueryRow(bounded, `SELECT integration.payment_queue_ready(),
		(SELECT coalesce(bool_and(coalesce(has_function_privilege(current_user,
		to_regprocedure(v.signature),'EXECUTE'),false)),false) FROM (VALUES
		('integration.load_stripe_credential(uuid,bigint,bytea,text)'),
		('integration.load_stripe_session(uuid,bigint,bytea,text)'),
		('integration.mark_stripe_create_sent(uuid,bigint,bytea,text,bytea)'),
		('integration.note_stripe_expire(uuid,bigint,bytea,text)'),
		('integration.finish_stripe_query(uuid,bigint,bytea,text,text)'),
		('integration.record_stripe_observation(uuid,bigint,bytea,text,jsonb,bigint,text)'),
		('integration.load_stripe_signal(uuid,bigint,bytea,text,bigint,uuid)'),
		('integration.consume_stripe_signal(uuid,uuid,bigint,bytea,text,text)'),
		('integration.load_stripe_refund(uuid,bigint,bytea,text)'),
		('integration.mark_stripe_refund_sent(uuid,bigint,bytea,text,bytea)'),
		('integration.finish_stripe_refund(uuid,bigint,bytea,text,text)'),
		('integration.record_stripe_refund_observation(uuid,bigint,bytea,text,jsonb,bigint)'),
		('integration.record_stripe_charge_observation(uuid,bigint,bytea,text,jsonb,bigint)')
		) AS v(signature))`).Scan(&ready, &capabilities); err != nil || !ready || !capabilities {
		return nil, errStripeRuntimeDatabase
	}
	return &StripeRuntime{pool: pool, keys: keys, profile: profile, transport: transport, live: live}, nil
}

type stripeSessionSnapshot struct {
	TenantID, StoreID, AttemptID, ConnectionID string
	CredentialVersion                          int64
	Environment, AccountID, Currency, Profile  string
	AmountMinor, UnitAmount                    int64
	CreateParams                               map[string]string
	ExpiresAt, SendDeadline, HandoffCutoff     time.Time
	CreateFirstSentAt, CreateSuppressedAt      *time.Time
	SessionID, PaymentIntentID                 string
	CancelRequestedAt                          *time.Time
	CreateSendCount                            int
	Captured, ClosedUnpaid                     bool
	DBNow                                      time.Time
	FirstCompleteUnpaidAt                      *time.Time
	SignalCandidate                            string
	LatestStatus, LatestPaymentStatus          string
}

func validStripeSnapshot(s stripeSessionSnapshot, id, profile string) bool {
	// LD1: a snapshot's environment must be the runtime profile's environment (a SANDBOX row never runs
	// under a LIVE worker and vice versa); an unknown profile has no environment and is refused.
	env, known := ProfileEnvironment(profile)
	if !known || !command.ValidID(s.TenantID) || !command.ValidID(s.StoreID) || !command.ValidID(s.ConnectionID) ||
		s.AttemptID != id || s.Profile != profile || s.Environment != env ||
		s.CredentialVersion <= 0 || s.AccountID == "" || s.DBNow.IsZero() ||
		s.ExpiresAt.IsZero() || s.SendDeadline.IsZero() || s.HandoffCutoff.IsZero() ||
		!s.SendDeadline.Before(s.HandoffCutoff) || !s.HandoffCutoff.Before(s.ExpiresAt) ||
		s.AmountMinor != s.UnitAmount || s.UnitAmount < 1 || s.CreateParams == nil ||
		s.CreateSendCount < 0 || s.CreateSendCount > 200 {
		return false
	}
	_, err := stripe.EncodeCreateBody(stripe.CreateParams{Fields: s.CreateParams})
	return err == nil && s.CreateParams["metadata[lc_attempt]"] == id &&
		s.CreateParams["payment_intent_data[metadata][lc_attempt]"] == id &&
		s.CreateParams["client_reference_id"] == id &&
		s.CreateParams["metadata[lc_profile]"] == profile &&
		s.CreateParams["expires_at"] == strconv.FormatInt(s.ExpiresAt.Unix(), 10) &&
		s.CreateParams["line_items[0][price_data][currency]"] == strings.ToLower(s.Currency) &&
		s.CreateParams["line_items[0][price_data][unit_amount]"] == strconv.FormatInt(s.UnitAmount, 10)
}

func (s *StripeRuntime) loadSession(ctx context.Context, id string, claim core.ClaimResult,
	dbTimeout time.Duration) (stripeSessionSnapshot, error) {
	bounded, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	var raw []byte
	// integration.load_stripe_session is lease/profile fenced and returns no secret material.
	if err := s.pool.QueryRow(bounded, `SELECT integration.load_stripe_session($1::uuid,$2::bigint,$3::bytea,$4::text)`,
		id, claim.Generation, claim.LeaseToken, s.profile).Scan(&raw); err != nil {
		return stripeSessionSnapshot{}, errStripeMaterial
	}
	var wire struct {
		TenantID              string            `json:"tenant_id"`
		StoreID               string            `json:"store_id"`
		AttemptID             string            `json:"attempt_id"`
		ConnectionID          string            `json:"connection_id"`
		CredentialVersion     int64             `json:"credential_version"`
		Environment           string            `json:"environment"`
		AccountID             string            `json:"account_id"`
		Currency              string            `json:"currency"`
		Profile               string            `json:"profile"`
		AmountMinor           int64             `json:"amount_minor"`
		UnitAmount            int64             `json:"unit_amount"`
		CreateParams          map[string]string `json:"create_params"`
		ExpiresAt             time.Time         `json:"expires_at"`
		SendDeadline          time.Time         `json:"send_deadline"`
		HandoffCutoff         time.Time         `json:"handoff_cutoff"`
		CreateFirstSentAt     *time.Time        `json:"create_first_sent_at"`
		CreateSuppressedAt    *time.Time        `json:"create_suppressed_at"`
		SessionID             *string           `json:"session_id"`
		PaymentIntentID       *string           `json:"payment_intent_id"`
		CancelRequestedAt     *time.Time        `json:"cancel_requested_at"`
		CreateSendCount       int               `json:"create_send_count"`
		Captured              bool              `json:"captured"`
		ClosedUnpaid          bool              `json:"closed_unpaid"`
		DBNow                 time.Time         `json:"db_now"`
		FirstCompleteUnpaidAt *time.Time        `json:"first_complete_unpaid_at"`
		SignalCandidate       *string           `json:"signal_candidate"`
		LatestReport          *struct {
			Status        string `json:"Status"`
			PaymentStatus string `json:"PaymentStatus"`
		} `json:"latest_report"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return stripeSessionSnapshot{}, errStripeMaterial
	}
	snapshot := stripeSessionSnapshot{
		TenantID: wire.TenantID, StoreID: wire.StoreID, AttemptID: wire.AttemptID,
		ConnectionID: wire.ConnectionID, CredentialVersion: wire.CredentialVersion,
		Environment: wire.Environment, AccountID: wire.AccountID, Currency: wire.Currency,
		Profile: wire.Profile, AmountMinor: wire.AmountMinor, UnitAmount: wire.UnitAmount,
		CreateParams: wire.CreateParams, ExpiresAt: wire.ExpiresAt, SendDeadline: wire.SendDeadline,
		HandoffCutoff: wire.HandoffCutoff, CreateFirstSentAt: wire.CreateFirstSentAt,
		CreateSuppressedAt: wire.CreateSuppressedAt, CancelRequestedAt: wire.CancelRequestedAt,
		CreateSendCount: wire.CreateSendCount, Captured: wire.Captured, ClosedUnpaid: wire.ClosedUnpaid,
		DBNow: wire.DBNow, FirstCompleteUnpaidAt: wire.FirstCompleteUnpaidAt,
	}
	if wire.SessionID != nil {
		snapshot.SessionID = *wire.SessionID
	}
	if wire.PaymentIntentID != nil {
		snapshot.PaymentIntentID = *wire.PaymentIntentID
	}
	if wire.SignalCandidate != nil {
		snapshot.SignalCandidate = *wire.SignalCandidate
	}
	if wire.LatestReport != nil {
		snapshot.LatestStatus = wire.LatestReport.Status
		snapshot.LatestPaymentStatus = wire.LatestReport.PaymentStatus
	}
	if !validStripeSnapshot(snapshot, id, s.profile) {
		return stripeSessionSnapshot{}, errStripeMaterial
	}
	return snapshot, nil
}

func (s *StripeRuntime) clientForClaim(ctx context.Context, snapshot stripeSessionSnapshot,
	claim core.ClaimResult, dbTimeout time.Duration) (*stripe.Client, error) {
	bounded, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	var scope accounts.StripeAPIScope
	var keyID string
	var nonce, ciphertext []byte
	// integration.load_stripe_credential selects the attempt's immutable version after the lease fence.
	if err := s.pool.QueryRow(bounded, `SELECT tenant_id::text,store_id::text,connection_id::text,
		credential_version,environment,account_id,key_id,nonce,ciphertext
		FROM integration.load_stripe_credential($1::uuid,$2::bigint,$3::bytea,$4::text)`,
		snapshot.AttemptID, claim.Generation, claim.LeaseToken, s.profile).
		Scan(&scope.TenantID, &scope.StoreID, &scope.ConnectionID, &scope.CredentialVersion,
			&scope.Environment, &scope.AccountID, &keyID, &nonce, &ciphertext); err != nil {
		return nil, errStripeMaterial
	}
	if scope.TenantID != snapshot.TenantID || scope.StoreID != snapshot.StoreID ||
		scope.ConnectionID != snapshot.ConnectionID || scope.CredentialVersion != snapshot.CredentialVersion ||
		scope.Environment != snapshot.Environment || scope.AccountID != snapshot.AccountID {
		return nil, errStripeMaterial
	}
	credentials, err := s.keys.OpenStripeAPI(scope, keyID, nonce, ciphertext)
	if err != nil {
		return nil, errStripeMaterial
	}
	config := s.stripeClientConfig(credentials.SecretKey, scope.AccountID, scope.Environment)
	var client *stripe.Client
	if s.transport == nil {
		client, err = stripe.New(config)
	} else {
		client, err = stripe.NewWithMockTransport(config, s.transport)
	}
	if err != nil {
		return nil, errStripeMaterial
	}
	// GET /v1/account is the first provider call and uses the original claim deadline.
	if _, err := client.VerifyAccount(ctx); err != nil {
		return nil, errStripeMaterial
	}
	return client, nil
}
