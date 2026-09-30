// cvs.go is the merchant side of taiwan-cvs-logistics-v1 (FROZEN 2026-09-30): connect an ECPay logistics account, enable it,
// per-store chain / pay-at-pickup settings (§16.5), request one label per order through the ledger/dispatcher (§4.3), read the
// shipment, print the label form, abandon, record a pay-at-pickup collection and cancel/restock a pay-at-pickup order (§16.8).
//
// Every write is one SQL definer of migrations/0073 (the single owner of its invariant); this file validates canonical input,
// frames the transaction (platform.WithScope, READ COMMITTED), runs the provider calls OUTSIDE any database transaction
// (connect probe, pre-abandon Query/V5) and maps SQLSTATEs to CVSError (unit default C7).
//
// Non-goals: no business rule (SQL decides), no ECPay wire format (internal/integrations/shipping/ecpay), no dispatcher route,
// no buyer surface (internal/checkout BuyerCVS), no secret in a return value, log line or error text.
// Callers: internal/httpapi (registerCVSRoutes) only. External hosts: none directly (ecpay.Client owns
// logistics(-stage).ecpay.com.tw).

package fulfillment

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"livecommerce/internal/command"
	"livecommerce/internal/integrations/core"
	"livecommerce/internal/integrations/shipping/ecpay"
	"livecommerce/internal/platform"
)

// CVSConfig is what the process derived once at startup. PaymentEnvironment is the already-validated COMMERCE_PAYMENT_PROFILE
// (PROVIDER_MOCK => SANDBOX, unit default C9); "" (buyer payment off) means every ECPay path is refused in SQL while MANUAL
// and buyer-entered stores keep working.
type CVSConfig struct {
	ECPay              ecpay.Config
	PaymentEnvironment string // "SANDBOX" | "LIVE" | ""
}

// CVS is the merchant service and the provider-hook host. Its pool is the main pool (commerce_runtime): merchant routes run
// through platform.WithScope, the two unauthenticated hooks through the definers that pin their own scope.
type CVS struct {
	pool   *pgxpool.Pool
	jobs   *river.Client[pgx.Tx]
	keys   *ecpay.Keyring
	client *ecpay.Client
	cfg    CVSConfig
	gates  *endpointGates
	// mapGates caps the unauthenticated map-return hook globally (key "*"); gates caps it per selection (key "map:<id>").
	mapGates *endpointGates
}

// NewCVS wires the service. jobs is the insert-only River client (Schema "river"): the API process never runs a worker.
// keys/client may be nil while cfg.ECPay.Enabled is false (settings, collection and pay-at-pickup release need no ECPay).
func NewCVS(pool *pgxpool.Pool, jobs *river.Client[pgx.Tx], keys *ecpay.Keyring, client *ecpay.Client, cfg CVSConfig) (*CVS, error) {
	if pool == nil || jobs == nil {
		return nil, command.ErrInvalid
	}
	if cfg.PaymentEnvironment != "" && cfg.PaymentEnvironment != "SANDBOX" && cfg.PaymentEnvironment != "LIVE" {
		return nil, command.ErrInvalid
	}
	if cfg.ECPay.Enabled && (keys == nil || client == nil || cfg.PaymentEnvironment == "" || cfg.ECPay.HooksOrigin == "") {
		// CVS_ECPAY_ENABLED=1 with buyer payment off (no environment to pin) is a startup error (C9).
		return nil, command.ErrInvalid
	}
	return &CVS{pool: pool, jobs: jobs, keys: keys, client: client, cfg: cfg, gates: newEndpointGates(4), mapGates: newEndpointGates(mapGlobalCap)}, nil
}

var (
	ecpayMerchantID = regexp.MustCompile(`^[0-9]{1,10}$`) // same rule as ecpay.merchantIDRE; the SQL CHECKs stay the wider alnum superset
	ecpaySecret     = regexp.MustCompile(`^[!-~]{1,64}$`)
	cvsErrCode      = regexp.MustCompile(`^[a-z][a-z0-9_]{2,59}$`)
	cvsKey          = regexp.MustCompile(`^[A-Za-z0-9_.:-]{8,128}$`)
)

// mapCVSError turns a SQLSTATE of the 0073 definers into a domain error the HTTP layer classifies (C7): PT400/22023 invalid,
// PT401/PT403 authority, PT404 not found, PT409/PT422/PT429 a coded CVSError, PT2RP the replay sentinel. Anything else is
// returned unchanged for the generic classifier (deadlock, deadline, unknown => retryable 503).
func mapCVSError(err error) error {
	if err == nil {
		return nil
	}
	var refusal *CVSError
	if errors.As(err, &refusal) || errors.Is(err, platform.ErrUnauthorized) || errors.Is(err, platform.ErrForbidden) ||
		errors.Is(err, platform.ErrScopeNotFound) || errors.Is(err, command.ErrInvalid) || errors.Is(err, command.ErrNotFound) ||
		errors.Is(err, command.ErrConflict) || errors.Is(err, ErrECPayDisabled) || errors.Is(err, errCVSReplay) {
		return err
	}
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		return err
	}
	switch pg.Code {
	case "PT400", "22023":
		return command.ErrInvalid
	case "PT401":
		return platform.ErrUnauthorized
	case "PT403":
		return platform.ErrForbidden
	case "PT404":
		return command.ErrNotFound
	case "PT2RP":
		return errCVSReplay
	case "PT409":
		if cvsErrCode.MatchString(pg.Message) {
			return &CVSError{Status: http.StatusConflict, Code: pg.Message}
		}
		return command.ErrConflict
	case "PT422":
		if cvsErrCode.MatchString(pg.Message) {
			return &CVSError{Status: http.StatusUnprocessableEntity, Code: pg.Message}
		}
		return command.ErrInvalid
	case "PT429":
		code := "rate_limited"
		if cvsErrCode.MatchString(pg.Message) {
			code = pg.Message // C7: the message is the contract's code (pay_at_pickup_limit ...)
		}
		return &CVSError{Status: http.StatusTooManyRequests, Code: code, RetryAfter: 60}
	}
	return err
}

// decodeCVS is a strict decoder for SQL-built JSON: unknown keys are drift, not data.
func decodeCVS(raw []byte, into any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return ErrCVSProjection
	}
	return nil
}

// ErrCVSProjection is a database/Go drift on a SQL projection (never shown as data).
var ErrCVSProjection = errors.New("cvs projection unavailable")

func tokenHash(token string) [32]byte { return sha256.Sum256([]byte(token)) }

func digestOf(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, command.ErrInvalid
	}
	sum := sha256.Sum256(raw)
	return sum[:], nil
}

// newUUIDv4 mints an operation id (crypto/rand). The definer derives the trade no and the semantic key from it.
func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], nil
}

func (c *CVS) scoped(ctx context.Context, token, storeID, permission string, fn func(pgx.Tx, platform.Scope) error) error {
	return platform.WithScope(ctx, c.pool, token, storeID, permission, fn)
}

// ---- ECPay connection (§8 GET/PUT/enabled) ---------------------------------------------------------

// Profile is the merchant projection of the store's ECPay logistics connection; never keys or sender data.
type Profile struct {
	Environment string  `json:"environment"`
	Mode        string  `json:"mode"`
	MerchantID  string  `json:"merchant_id"`
	Version     int64   `json:"version"`
	Enabled     bool    `json:"enabled"`
	QualifiedAt *string `json:"qualified_at"`
	OKVerified  bool    `json:"ok_verified"`
	StatusURL   string  `json:"status_url"`
}

type profileRow struct {
	Environment string  `json:"environment"`
	Mode        string  `json:"mode"`
	MerchantID  string  `json:"merchant_id"`
	Version     int64   `json:"version"`
	Enabled     bool    `json:"enabled"`
	QualifiedAt *string `json:"qualified_at"`
	OKVerified  bool    `json:"ok_verified"`
	EndpointID  string  `json:"endpoint_id"`
	CredVersion *int64  `json:"credential_version"`
}

func (c *CVS) profileFrom(raw []byte) (Profile, error) {
	var row profileRow
	if err := decodeCVS(raw, &row); err != nil {
		return Profile{}, err
	}
	if !command.ValidID(row.EndpointID) || (row.Environment != "SANDBOX" && row.Environment != "LIVE") ||
		(row.Mode != "C2C" && row.Mode != "B2C") || !ecpayMerchantID.MatchString(row.MerchantID) || row.Version < 1 {
		return Profile{}, ErrCVSProjection
	}
	// The status URL is public, derived and not a secret (the MAC is): HooksOrigin + the route + the random endpoint id.
	return Profile{Environment: row.Environment, Mode: row.Mode, MerchantID: row.MerchantID, Version: row.Version,
		Enabled: row.Enabled, QualifiedAt: row.QualifiedAt, OKVerified: row.OKVerified,
		StatusURL: strings.TrimRight(c.cfg.ECPay.HooksOrigin, "/") + "/v1/cvs/ecpay/status/" + row.EndpointID}, nil
}

// Profile returns the connection card (integration:read); command.ErrNotFound when the store has none.
func (c *CVS) Profile(ctx context.Context, token, storeID string) (Profile, error) {
	var out Profile
	err := c.scoped(ctx, token, storeID, "integration:read", func(tx pgx.Tx, s platform.Scope) error {
		hash := tokenHash(token)
		var raw []byte
		// fulfillment.read_ecpay_logistics: runtime has no table grant on the profile; the definer re-authorizes.
		if err := tx.QueryRow(ctx, `SELECT fulfillment.read_ecpay_logistics($1,$2::uuid)`, hash[:], storeID).Scan(&raw); err != nil {
			return mapCVSError(err)
		}
		var err error
		out, err = c.profileFrom(raw)
		return err
	})
	return out, mapCVSError(err)
}

// ConnectInput is the exact PUT body. Secrets are write-only: they are sealed with the logistics keyring and never returned.
type ConnectInput struct {
	ExpectedVersion int64  `json:"expected_version"`
	Environment     string `json:"environment"`
	Mode            string `json:"mode"`
	MerchantID      string `json:"merchant_id"`
	HashKey         string `json:"hash_key"`
	HashIV          string `json:"hash_iv"`
	SenderName      string `json:"sender_name"`
	SenderCellPhone string `json:"sender_cell_phone"`
}

func (in ConnectInput) String() string { return "fulfillment.ConnectInput{redacted}" }

// GoString keeps the secrets out of %#v as well.
func (in ConnectInput) GoString() string { return in.String() }

// Connect registers or rotates the store's ECPay logistics credentials: validate -> deployment environment pin (C9, before any
// network) -> ecpay.Probe (GetStoreList, outside any transaction) -> Seal (AAD binds tenant/store/connection/environment/merchant/
// version) -> definer. 422 ecpay_probe_failed | invalid_sender | ecpay_environment_not_allowed.
func (c *CVS) Connect(ctx context.Context, token, storeID, key string, in ConnectInput) (Profile, error) {
	if !cvsKey.MatchString(key) || in.ExpectedVersion < 0 || in.ExpectedVersion >= 1<<62 || !command.ValidID(storeID) ||
		(in.Environment != "SANDBOX" && in.Environment != "LIVE") || (in.Mode != "C2C" && in.Mode != "B2C") ||
		!ecpayMerchantID.MatchString(in.MerchantID) || !ecpaySecret.MatchString(in.HashKey) || !ecpaySecret.MatchString(in.HashIV) {
		return Profile{}, command.ErrInvalid
	}
	// Sender data is merchant contact data F5 requires (SenderName 4-10 wide, SenderCellPhone 09xxxxxxxx); it is validated with
	// the recipient rule and kept only inside the ciphertext.
	phone, ok := ecpay.RecipientOK(in.SenderName, in.SenderCellPhone)
	if !ok {
		return Profile{}, &CVSError{Status: http.StatusUnprocessableEntity, Code: "invalid_sender"}
	}
	// C9: the ECPay environment must equal the deployment payment environment; checked before the probe so a LIVE deployment
	// never talks to the public stage merchant.
	if c.cfg.PaymentEnvironment == "" || in.Environment != c.cfg.PaymentEnvironment {
		return Profile{}, &CVSError{Status: http.StatusUnprocessableEntity, Code: "ecpay_environment_not_allowed"}
	}
	if !c.cfg.ECPay.Enabled || c.keys == nil || c.client == nil {
		return Profile{}, ErrECPayDisabled
	}
	var scope platform.Scope
	if err := c.scoped(ctx, token, storeID, "integration:manage", func(_ pgx.Tx, s platform.Scope) error { scope = s; return nil }); err != nil {
		return Profile{}, mapCVSError(err)
	}
	creds := ecpay.Credentials{MerchantID: in.MerchantID, HashKey: in.HashKey, HashIV: in.HashIV}
	// ecpay.Client.Probe: GetStoreList UNIMART over TLS with the plaintext keys, 10 s. A failed probe stores nothing.
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	probeErr := c.client.Probe(probeCtx, creds)
	cancel()
	if probeErr != nil {
		return Profile{}, &CVSError{Status: http.StatusUnprocessableEntity, Code: "ecpay_probe_failed"}
	}
	version := in.ExpectedVersion + 1
	keyID, nonce, ciphertext, err := c.keys.Seal(ecpay.Scope{TenantID: scope.TenantID, StoreID: storeID,
		ConnectionID: ConnectionID(scope.TenantID, storeID, in.Environment), MerchantID: in.MerchantID,
		Environment: ecpay.Environment(in.Environment), Version: version},
		ecpay.Payload{HashKey: in.HashKey, HashIV: in.HashIV, SenderName: in.SenderName, SenderCellPhone: phone})
	if err != nil {
		return Profile{}, ErrECPayDisabled
	}
	// The idempotency digest binds the secrets by hash only, so a replay with other keys is a conflict and nothing sensitive
	// is ever stored in the command receipt.
	digest, err := digestOf(struct {
		Op                                string `json:"op"`
		ExpectedVersion                   int64  `json:"expected_version"`
		Environment                       string `json:"environment"`
		Mode                              string `json:"mode"`
		MerchantID                        string `json:"merchant_id"`
		KeyDigest, IVDigest, SenderDigest string
	}{"ecpay.connect", in.ExpectedVersion, in.Environment, in.Mode, in.MerchantID,
		hexSum(in.HashKey), hexSum(in.HashIV), hexSum(in.SenderName + "|" + phone)})
	if err != nil {
		return Profile{}, err
	}
	var out Profile
	err = c.scoped(ctx, token, storeID, "integration:manage", func(tx pgx.Tx, s platform.Scope) error {
		hash := tokenHash(token)
		var raw []byte
		// integration.register_ecpay_logistics: only credential writer; the runtime never touches the tables.
		if err := tx.QueryRow(ctx, `SELECT integration.register_ecpay_logistics($1,$2::uuid,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			hash[:], storeID, key, digest, in.ExpectedVersion, in.Environment, in.MerchantID, in.Mode, keyID, nonce, ciphertext,
			true, c.cfg.PaymentEnvironment).Scan(&raw); err != nil {
			return mapCVSError(err)
		}
		var err error
		out, err = c.profileFrom(raw)
		return err
	})
	return out, mapCVSError(err)
}

func hexSum(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// EnableInput is the exact POST body of .../logistics/ecpay/enabled.
type EnableInput struct {
	ExpectedVersion int64 `json:"expected_version"`
	Enabled         bool  `json:"enabled"`
}

// Enable switches new selections/creates on or off; status ingress and query continue while disabled.
func (c *CVS) Enable(ctx context.Context, token, storeID, key string, in EnableInput) (Profile, error) {
	if !cvsKey.MatchString(key) || in.ExpectedVersion < 1 || in.ExpectedVersion >= 1<<62 {
		return Profile{}, command.ErrInvalid
	}
	digest, err := digestOf(struct {
		Op string `json:"op"`
		EnableInput
	}{"ecpay.enable", in})
	if err != nil {
		return Profile{}, err
	}
	var out Profile
	err = c.scoped(ctx, token, storeID, "integration:manage", func(tx pgx.Tx, s platform.Scope) error {
		hash := tokenHash(token)
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT integration.set_ecpay_logistics_enabled($1,$2::uuid,$3,$4,$5,$6)`,
			hash[:], storeID, key, digest, in.ExpectedVersion, in.Enabled).Scan(&raw); err != nil {
			return mapCVSError(err)
		}
		var err error
		out, err = c.profileFrom(raw)
		return err
	})
	return out, mapCVSError(err)
}

// ---- store settings (§16.5, §8 GET/PUT cvs-settings) -----------------------------------------------

// Settings is the per-store CVS configuration; a store without a row reads the defaults at version 0 (C8).
type Settings struct {
	Version            int64    `json:"version"`
	EnabledChains      []string `json:"enabled_chains"`
	PayAtPickupEnabled bool     `json:"pay_at_pickup_enabled"`
	PayAtPickupMaxTWD  *int     `json:"pay_at_pickup_max_twd"`
	PayAtPickupMaxOpen int      `json:"pay_at_pickup_max_open"`
}

// SettingsInput is the exact PUT body.
type SettingsInput struct {
	ExpectedVersion    int64    `json:"expected_version"`
	EnabledChains      []string `json:"enabled_chains"`
	PayAtPickupEnabled bool     `json:"pay_at_pickup_enabled"`
	PayAtPickupMaxTWD  *int     `json:"pay_at_pickup_max_twd"`
	PayAtPickupMaxOpen int      `json:"pay_at_pickup_max_open"`
}

func validSettings(s Settings) bool {
	if s.Version < 0 || s.PayAtPickupMaxOpen < 1 || s.PayAtPickupMaxOpen > 500 || len(s.EnabledChains) > 4 ||
		(s.PayAtPickupMaxTWD != nil && (*s.PayAtPickupMaxTWD < 1 || *s.PayAtPickupMaxTWD > 20000)) ||
		(s.PayAtPickupEnabled && s.PayAtPickupMaxTWD == nil) {
		return false
	}
	seen := map[string]bool{}
	for _, chain := range s.EnabledChains {
		if !isCVSKind(chain) || seen[chain] {
			return false
		}
		seen[chain] = true
	}
	return true
}

// Settings reads the store's CVS settings (integration:read).
func (c *CVS) Settings(ctx context.Context, token, storeID string) (Settings, error) {
	var out Settings
	err := c.scoped(ctx, token, storeID, "integration:read", func(tx pgx.Tx, s platform.Scope) error {
		hash := tokenHash(token)
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT fulfillment.read_cvs_settings($1,$2::uuid)`, hash[:], storeID).Scan(&raw); err != nil {
			return mapCVSError(err)
		}
		if err := decodeCVS(raw, &out); err != nil {
			return err
		}
		if !validSettings(out) {
			return ErrCVSProjection
		}
		return nil
	})
	return out, mapCVSError(err)
}

// SetSettings writes the settings with version CAS (0 inserts); 422 invalid_settings from SQL or from the checks here.
func (c *CVS) SetSettings(ctx context.Context, token, storeID, key string, in SettingsInput) (Settings, error) {
	if !cvsKey.MatchString(key) || in.ExpectedVersion < 0 || in.ExpectedVersion >= 1<<62 {
		return Settings{}, command.ErrInvalid
	}
	if in.EnabledChains == nil {
		in.EnabledChains = []string{}
	}
	if !validSettings(Settings{Version: in.ExpectedVersion, EnabledChains: in.EnabledChains, PayAtPickupEnabled: in.PayAtPickupEnabled,
		PayAtPickupMaxTWD: in.PayAtPickupMaxTWD, PayAtPickupMaxOpen: in.PayAtPickupMaxOpen}) {
		return Settings{}, &CVSError{Status: http.StatusUnprocessableEntity, Code: "invalid_settings"}
	}
	digest, err := digestOf(struct {
		Op string `json:"op"`
		SettingsInput
	}{"cvs.settings", in})
	if err != nil {
		return Settings{}, err
	}
	var maxTWD any
	if in.PayAtPickupMaxTWD != nil {
		maxTWD = int32(*in.PayAtPickupMaxTWD)
	}
	var out Settings
	err = c.scoped(ctx, token, storeID, "integration:manage", func(tx pgx.Tx, s platform.Scope) error {
		hash := tokenHash(token)
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT fulfillment.set_cvs_store_settings($1,$2::uuid,$3,$4,$5,$6::text[],$7,$8::integer,$9::integer)`,
			hash[:], storeID, key, digest, in.ExpectedVersion, in.EnabledChains, in.PayAtPickupEnabled, maxTWD,
			int32(in.PayAtPickupMaxOpen)).Scan(&raw); err != nil {
			return mapCVSError(err)
		}
		if err := decodeCVS(raw, &out); err != nil {
			return err
		}
		if !validSettings(out) {
			return ErrCVSProjection
		}
		return nil
	})
	return out, mapCVSError(err)
}

// ---- shipment (§8 POST/GET cvs-shipment, print-form, abandon) --------------------------------------

// RequestInput is the exact POST body of .../orders/{id}/cvs-shipment.
type RequestInput struct {
	ExpectedVersion int64 `json:"expected_version"`
}

// Requested is the 202 body: the attempt is REQUESTED and the operation planned; ECPay is not yet called.
type Requested struct {
	Attempt     int    `json:"attempt"`
	State       string `json:"state"`
	OperationID string `json:"operation_id"`
}

// CVSEvent is one history row (source local | ecpay_status | ecpay_query); no PII, no body.
type CVSEvent struct {
	Source          string  `json:"source"`
	EventCode       *string `json:"event_code"`
	ProviderCode    *string `json:"provider_code"`
	ProviderMessage *string `json:"provider_message"`
	FromState       *string `json:"from_state"`
	ToState         *string `json:"to_state"`
	ReceivedAt      string  `json:"received_at"`
}

// CVSAttempt is one shipment attempt as the merchant sees it: no trade no, no PII (TD8).
type CVSAttempt struct {
	Attempt             int        `json:"attempt"`
	State               string     `json:"state"`
	Subtype             string     `json:"subtype"`
	Environment         string     `json:"environment"`
	ReceiverStoreID     string     `json:"receiver_store_id"`
	GoodsAmount         int        `json:"goods_amount"`
	CollectionAmount    *int       `json:"collection_amount"`
	ProviderLogisticsID *string    `json:"provider_logistics_id"`
	Code                *string    `json:"code"`
	PrintAvailable      bool       `json:"print_available"`
	ResultCode          *string    `json:"result_code"`
	LastStatusCode      *string    `json:"last_status_code"`
	LastStatusAt        *string    `json:"last_status_at"`
	Alerts              []string   `json:"alerts"`
	CreatedAt           string     `json:"created_at"`
	UpdatedAt           string     `json:"updated_at"`
	Version             int64      `json:"version"`
	Events              []CVSEvent `json:"events"`
}

// ShipmentView is GET .../cvs-shipment: every attempt, the next expected_version and the validity window of the current one.
type ShipmentView struct {
	CurrentAttempt  *int         `json:"current_attempt"`
	ExpectedVersion int64        `json:"expected_version"`
	ValidityDays    *int         `json:"validity_days"`
	Attempts        []CVSAttempt `json:"attempts"`
}

var cvsAttemptStates = map[string]bool{"REQUESTED": true, "CREATED": true, "FAILED": true, "UNKNOWN": true, "AT_DC": true,
	"AT_STORE": true, "PICKED_UP": true, "UNCLAIMED": true, "ABANDONED": true}

func validShipmentView(v ShipmentView) bool {
	if v.Attempts == nil || len(v.Attempts) > 5 || v.ExpectedVersion < 0 || (v.CurrentAttempt == nil) != (len(v.Attempts) == 0) {
		return false
	}
	for i, a := range v.Attempts {
		if a.Attempt != i+1 || !cvsAttemptStates[a.State] || a.Version < 1 || a.Events == nil || a.Alerts == nil ||
			a.GoodsAmount < 1 || a.GoodsAmount > 20000 || (a.CollectionAmount != nil && *a.CollectionAmount != a.GoodsAmount) ||
			(a.Environment != "SANDBOX" && a.Environment != "LIVE") {
			return false
		}
	}
	return len(v.Attempts) == 0 || (*v.CurrentAttempt == len(v.Attempts) && v.ExpectedVersion == v.Attempts[len(v.Attempts)-1].Version)
}

// Request plans and records one label request in ONE READ COMMITTED transaction: it mints the operation id, inserts the
// external_operation_v1 River job in that transaction, then calls request_cvs_shipment (which verifies the job by xmin/args and
// inserts the operation, the shipment, the command result and the audit row). A replayed key raises PT2RP: this transaction is rolled
// back (dropping the job; post_river/0014 would refuse to commit an orphan anyway) and the stored answer is read in a second one.
func (c *CVS) Request(ctx context.Context, token, storeID, key, orderID string, in RequestInput) (Requested, error) {
	if !cvsKey.MatchString(key) || !command.ValidID(orderID) || in.ExpectedVersion < 0 || in.ExpectedVersion >= 1<<62 {
		return Requested{}, command.ErrInvalid
	}
	if !c.cfg.ECPay.Enabled {
		return Requested{}, &CVSError{Status: http.StatusUnprocessableEntity, Code: "connection_unavailable"}
	}
	digest, err := digestOf(struct {
		Op              string `json:"op"`
		OrderID         string `json:"order_id"`
		ExpectedVersion int64  `json:"expected_version"`
	}{"cvs.request", orderID, in.ExpectedVersion})
	if err != nil {
		return Requested{}, err
	}
	operation, err := newUUIDv4()
	if err != nil {
		return Requested{}, ErrCVSProjection
	}
	var out Requested
	err = c.scoped(ctx, token, storeID, "fulfillment:write", func(tx pgx.Tx, s platform.Scope) error {
		// core.InsertOperationJob: external_operation_v1 args {operation_id, version:1}, default queue, no InsertOpts (the shape
		// integration.plan_cvs_create verifies). It never runs here: only cmd/claims-worker executes it.
		job, err := core.InsertOperationJob(ctx, c.jobs, tx, operation)
		if err != nil {
			return err
		}
		hash := tokenHash(token)
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT fulfillment.request_cvs_shipment($1,$2::uuid,$3::uuid,$4,$5,$6,$7::uuid,$8::bigint)`,
			hash[:], storeID, orderID, key, digest, in.ExpectedVersion, operation, job).Scan(&raw); err != nil {
			return mapCVSError(err)
		}
		return decodeRequested(raw, &out)
	})
	if errors.Is(err, errCVSReplay) {
		// The transaction above rolled back with its job; answer from the stored command result (or 409 idempotency_conflict).
		err = c.scoped(ctx, token, storeID, "fulfillment:write", func(tx pgx.Tx, s platform.Scope) error {
			hash := tokenHash(token)
			var raw []byte
			if err := tx.QueryRow(ctx, `SELECT fulfillment.read_cvs_shipment_command($1,$2::uuid,$3,$4)`, hash[:], storeID, key, digest).Scan(&raw); err != nil {
				return mapCVSError(err)
			}
			return decodeRequested(raw, &out)
		})
	}
	return out, mapCVSError(err)
}

func decodeRequested(raw []byte, out *Requested) error {
	if err := decodeCVS(raw, out); err != nil {
		return err
	}
	if out.Attempt < 1 || out.Attempt > 5 || out.State != "REQUESTED" || !command.ValidID(out.OperationID) {
		return ErrCVSProjection
	}
	return nil
}

// Shipment reads every attempt with its events (orders:read). The definer first settles a stuck attempt from its operation.
func (c *CVS) Shipment(ctx context.Context, token, storeID, orderID string) (ShipmentView, error) {
	if !command.ValidID(orderID) {
		return ShipmentView{}, command.ErrInvalid
	}
	var out ShipmentView
	err := c.scoped(ctx, token, storeID, "orders:read", func(tx pgx.Tx, s platform.Scope) error {
		hash := tokenHash(token)
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT fulfillment.read_cvs_shipment($1,$2::uuid,$3::uuid)`, hash[:], storeID, orderID).Scan(&raw); err != nil {
			return mapCVSError(err)
		}
		if err := decodeCVS(raw, &out); err != nil {
			return err
		}
		if !validShipmentView(out) {
			return ErrCVSProjection
		}
		return nil
	})
	return out, mapCVSError(err)
}

// PrintForm is the signed F8 print form of the latest attempt (fulfillment:write); no state change, audited in SQL.
type PrintForm struct {
	Action string            `json:"action"`
	Fields map[string]string `json:"fields"`
}

type actionSource struct {
	Attempt         int     `json:"attempt"`
	Version         int64   `json:"version"`
	State           string  `json:"state"`
	Subtype         string  `json:"subtype"`
	Environment     string  `json:"environment"`
	MerchantTradeNo string  `json:"merchant_trade_no"`
	LogisticsID     *string `json:"logistics_id"`
	PaymentNo       *string `json:"payment_no"`
	ValidationNo    *string `json:"validation_no"`
}

type keyRow struct {
	tenantID, storeID, connectionID, merchantID, environment string
	version                                                  int64
	keyID                                                    string
	nonce, ciphertext                                        []byte
}

// loadMerchantCredentials runs integration.load_ecpay_key_for_merchant (fulfillment:write, fresh auth) and opens the payload.
func (c *CVS) loadMerchantCredentials(ctx context.Context, tx pgx.Tx, token, storeID string) (ecpay.Credentials, string, error) {
	if c.keys == nil {
		return ecpay.Credentials{}, "", ErrECPayDisabled
	}
	hash := tokenHash(token)
	var k keyRow
	if err := tx.QueryRow(ctx, `SELECT tenant_id::text,store_id::text,connection_id::text,merchant_id,environment,credential_version,key_id,nonce,ciphertext
		FROM integration.load_ecpay_key_for_merchant($1,$2::uuid)`, hash[:], storeID).Scan(&k.tenantID, &k.storeID, &k.connectionID,
		&k.merchantID, &k.environment, &k.version, &k.keyID, &k.nonce, &k.ciphertext); err != nil {
		return ecpay.Credentials{}, "", mapCVSError(err)
	}
	payload, err := c.keys.Open(ecpay.Scope{TenantID: k.tenantID, StoreID: k.storeID, ConnectionID: k.connectionID, MerchantID: k.merchantID,
		Environment: ecpay.Environment(k.environment), Version: k.version}, k.keyID, k.nonce, k.ciphertext)
	if err != nil {
		return ecpay.Credentials{}, "", ErrECPayDisabled
	}
	return ecpay.Credentials{MerchantID: k.merchantID, HashKey: payload.HashKey, HashIV: payload.HashIV}, k.environment, nil
}

func (c *CVS) actionSource(ctx context.Context, tx pgx.Tx, token, storeID, orderID, purpose string) (actionSource, error) {
	hash := tokenHash(token)
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT fulfillment.read_cvs_action_source($1,$2::uuid,$3::uuid,$4)`, hash[:], storeID, orderID, purpose).Scan(&raw); err != nil {
		return actionSource{}, mapCVSError(err)
	}
	var src actionSource
	if err := decodeCVS(raw, &src); err != nil {
		return actionSource{}, err
	}
	if src.Attempt < 1 || src.Version < 1 || !cvsAttemptStates[src.State] || !regexp.MustCompile(`^LC[A-Z2-7]{18}$`).MatchString(src.MerchantTradeNo) {
		return actionSource{}, ErrCVSProjection
	}
	return src, nil
}

// PrintForm builds the signed form for the latest attempt's label. thermal selects the A6 sheet (PrintMode=2) inside the adapter.
func (c *CVS) PrintForm(ctx context.Context, token, storeID, orderID string, thermal bool) (PrintForm, error) {
	if !command.ValidID(orderID) {
		return PrintForm{}, command.ErrInvalid
	}
	var out PrintForm
	err := c.scoped(ctx, token, storeID, "fulfillment:write", func(tx pgx.Tx, s platform.Scope) error {
		src, err := c.actionSource(ctx, tx, token, storeID, orderID, "print")
		if err != nil {
			return err
		}
		creds, environment, err := c.loadMerchantCredentials(ctx, tx, token, storeID)
		if err != nil {
			return err
		}
		if environment != src.Environment || src.LogisticsID == nil || src.PaymentNo == nil {
			return &CVSError{Status: http.StatusUnprocessableEntity, Code: "print_unsupported"}
		}
		validation := ""
		if src.ValidationNo != nil {
			validation = *src.ValidationNo
		}
		action, fields, err := ecpay.PrintForm(ecpay.Environment(environment), creds, src.Subtype, *src.LogisticsID, *src.PaymentNo, validation, thermal)
		if err != nil {
			return &CVSError{Status: http.StatusUnprocessableEntity, Code: "print_unsupported"}
		}
		out = PrintForm{Action: action, Fields: fields}
		return nil
	})
	return out, mapCVSError(err)
}

// AbandonInput is the exact POST body: the acknowledgement is required for an UNKNOWN attempt (the merchant checked ECPay's backend).
type AbandonInput struct {
	ExpectedVersion int64 `json:"expected_version"`
	IChecked        bool  `json:"i_checked_ecpay_backend"`
}

// Abandon gives up an attempt so a manual or new ECPay shipment is possible. Go first runs a MAC-verified Query/V5 with the
// merchant key OUTSIDE any transaction, then passes what it found to the definer (which never calls ECPay). A found trade is
// applied as CREATED and answered 409 ecpay_trade_found; a created-only status is required to abandon a lapsed CREATED label.
func (c *CVS) Abandon(ctx context.Context, token, storeID, key, orderID string, in AbandonInput) (ShipmentView, error) {
	if !cvsKey.MatchString(key) || !command.ValidID(orderID) || in.ExpectedVersion < 1 || in.ExpectedVersion >= 1<<62 {
		return ShipmentView{}, command.ErrInvalid
	}
	digest, err := digestOf(struct {
		Op              string `json:"op"`
		OrderID         string `json:"order_id"`
		ExpectedVersion int64  `json:"expected_version"`
		Acknowledged    bool   `json:"acknowledged"`
	}{"cvs.abandon", orderID, in.ExpectedVersion, in.IChecked})
	if err != nil {
		return ShipmentView{}, err
	}
	var src actionSource
	var creds ecpay.Credentials
	var dbNow, started time.Time
	err = c.scoped(ctx, token, storeID, "fulfillment:write", func(tx pgx.Tx, s platform.Scope) error {
		var err error
		if src, err = c.actionSource(ctx, tx, token, storeID, orderID, "abandon"); err != nil {
			return err
		}
		if src.Version != in.ExpectedVersion {
			return &CVSError{Status: http.StatusConflict, Code: "version_changed"}
		}
		// Only the states that need provider evidence run a query: a lapsed CREATED label, or an acknowledged UNKNOWN.
		if c.client != nil && (src.State == "CREATED" || (src.State == "UNKNOWN" && in.IChecked)) {
			if creds, _, err = c.loadMerchantCredentials(ctx, tx, token, storeID); err != nil {
				return err
			}
		}
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&dbNow); err != nil {
			return err
		}
		started = time.Now() // pairs with dbNow: the first transaction's own duration must not be counted twice (TCV05)
		return nil
	})
	if err != nil {
		return ShipmentView{}, mapCVSError(err)
	}
	var queryStatus, foundLogistics, foundPayment, foundValidation, foundShipment any
	var queryAt any
	if creds.HashKey != "" {
		// ecpay.Client.Query (QueryLogisticsTradeInfo/V5 by MerchantTradeNo): UNKNOWN/error leaves the values NULL, which the
		// definer treats as "no evidence" (409 ecpay_shows_movement for CREATED, abandon for an acknowledged UNKNOWN).
		queryCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		res := c.client.Query(queryCtx, creds, src.MerchantTradeNo)
		cancel()
		// The DB clock at the start plus the monotonic elapsed time keeps query_at inside the definer's 10-minute window
		// without trusting the API host's wall clock.
		queryAt = dbNow.Add(time.Since(started))
		if res.Outcome == "SUCCEEDED" {
			queryStatus = nilIfEmpty(res.StatusCode)
			foundLogistics, foundPayment = nilIfEmpty(res.LogisticsID), nilIfEmpty(res.PaymentNo)
			foundValidation, foundShipment = nilIfEmpty(res.ValidationNo), nilIfEmpty(res.ShipmentNo)
		}
	}
	var found bool
	err = c.scoped(ctx, token, storeID, "fulfillment:write", func(tx pgx.Tx, s platform.Scope) error {
		hash := tokenHash(token)
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT fulfillment.abandon_cvs_shipment($1,$2::uuid,$3::uuid,$4,$5,$6,$7,$8::timestamptz,$9,$10,$11,$12,$13)`,
			hash[:], storeID, orderID, key, digest, in.ExpectedVersion, queryStatus, queryAt, in.IChecked,
			foundLogistics, foundPayment, foundValidation, foundShipment).Scan(&raw); err != nil {
			return mapCVSError(err)
		}
		var result struct {
			Outcome string `json:"outcome"`
			Attempt int    `json:"attempt"`
			State   string `json:"state"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return ErrCVSProjection
		}
		// Returned, not raised: the UNKNOWN -> CREATED write must COMMIT, so the closure returns nil and the 409 is
		// produced only after platform.WithScope acknowledged the commit.
		found = result.Outcome == "trade_found"
		return nil
	})
	if err != nil {
		return ShipmentView{}, mapCVSError(err)
	}
	if found {
		return ShipmentView{}, &CVSError{Status: http.StatusConflict, Code: "ecpay_trade_found"}
	}
	return c.Shipment(ctx, token, storeID, orderID)
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---- collection and pay-at-pickup release (§16.4, §16.8) ------------------------------------------

// CollectionInput is the exact POST body of .../orders/{id}/collection.
type CollectionInput struct {
	ExpectedState string `json:"expected_state"`
	State         string `json:"state"`
}

// CollectionResult is the 200 body.
type CollectionResult struct {
	OrderID         string `json:"order_id"`
	CollectionState string `json:"collection_state"`
}

var collectionStates = map[string]bool{"PENDING": true, "COLLECTED": true, "RETURNED": true, "REFUNDED_OFFLINE": true, "CANCELLED": true, "RESTOCKED": true}

// RecordCollection is the merchant's audited manual collected / returned / refunded_offline mark (fulfillment:write). No money
// moves: the platform never holds pay-at-pickup money.
func (c *CVS) RecordCollection(ctx context.Context, token, storeID, key, orderID string, in CollectionInput) (CollectionResult, error) {
	if !cvsKey.MatchString(key) || !command.ValidID(orderID) || !collectionStates[in.ExpectedState] ||
		(in.State != "collected" && in.State != "returned" && in.State != "refunded_offline") {
		return CollectionResult{}, command.ErrInvalid
	}
	digest, err := digestOf(struct {
		Op      string `json:"op"`
		OrderID string `json:"order_id"`
		CollectionInput
	}{"cvs.collection", orderID, in})
	if err != nil {
		return CollectionResult{}, err
	}
	var out CollectionResult
	err = c.scoped(ctx, token, storeID, "fulfillment:write", func(tx pgx.Tx, s platform.Scope) error {
		hash := tokenHash(token)
		var raw []byte
		// fulfillment.record_collection: the only merchant writer of collection_state (no ledger, payment or refund row).
		if err := tx.QueryRow(ctx, `SELECT fulfillment.record_collection($1,$2::uuid,$3::uuid,$4,$5,$6,$7)`,
			hash[:], storeID, orderID, key, digest, in.ExpectedState, in.State).Scan(&raw); err != nil {
			return mapCVSError(err)
		}
		if err := decodeCVS(raw, &out); err != nil {
			return err
		}
		if out.OrderID != orderID || !collectionStates[out.CollectionState] {
			return ErrCVSProjection
		}
		return nil
	})
	return out, mapCVSError(err)
}

// ReleaseInput is the exact POST body of .../orders/{id}/pay-at-pickup-release.
type ReleaseInput struct {
	Action        string `json:"action"`
	ExpectedState string `json:"expected_state"`
}

// ReleaseResult is the 200 body.
type ReleaseResult struct {
	OrderID         string `json:"order_id"`
	CollectionState string `json:"collection_state"`
	CommercialState string `json:"commercial_state"`
	ReleasedLines   int    `json:"released_lines"`
}

// Release cancels an unshipped PENDING pay-at-pickup order or restocks a RETURNED one through the single audited stock writer
// (X8, §16.8). §11.5: the ledger rows carry order id + collection_state + actor.
func (c *CVS) Release(ctx context.Context, token, storeID, key, orderID string, in ReleaseInput) (ReleaseResult, error) {
	if !cvsKey.MatchString(key) || !command.ValidID(orderID) || (in.Action != "cancel" && in.Action != "restock") ||
		(in.Action == "cancel" && in.ExpectedState != "PENDING") || (in.Action == "restock" && in.ExpectedState != "RETURNED") {
		return ReleaseResult{}, command.ErrInvalid
	}
	digest, err := digestOf(struct {
		Op      string `json:"op"`
		OrderID string `json:"order_id"`
		ReleaseInput
	}{"cvs.release", orderID, in})
	if err != nil {
		return ReleaseResult{}, err
	}
	var out ReleaseResult
	err = c.scoped(ctx, token, storeID, "fulfillment:write", func(tx pgx.Tx, s platform.Scope) error {
		hash := tokenHash(token)
		var raw []byte
		// inventory.release_pay_at_pickup: the ONLY DEALLOCATE writer; no payments, refund, operation or river row.
		if err := tx.QueryRow(ctx, `SELECT inventory.release_pay_at_pickup($1,$2::uuid,$3::uuid,$4,$5,$6,$7)`,
			hash[:], storeID, orderID, key, digest, in.Action, in.ExpectedState).Scan(&raw); err != nil {
			return mapCVSError(err)
		}
		if err := decodeCVS(raw, &out); err != nil {
			return err
		}
		if out.OrderID != orderID || !collectionStates[out.CollectionState] || out.ReleasedLines < 0 ||
			(out.CommercialState != "CONFIRMED" && out.CommercialState != "CANCELLED") {
			return ErrCVSProjection
		}
		return nil
	})
	return out, mapCVSError(err)
}
