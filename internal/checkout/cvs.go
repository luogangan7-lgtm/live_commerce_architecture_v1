// cvs.go is the buyer side of taiwan-cvs-logistics-v1: open an ECPay e-map selection (a same-tab form the storefront submits), read
// or verify it after the buyer returns (directory lookup OUTSIDE any transaction, then verify_cvs_selection), and enter a store by
// hand when the merchant has no ECPay connection (§16.1, source "buyer_entered", never "verified").
//
// Non-goals: no state rule (the SQL definers own every check, the check order of the map return and the buyer scope), no wire format
// (internal/integrations/shipping/ecpay builds the form and looks the directory up), no merchant surface (internal/fulfillment), no
// secret in a return value or log, no map-return handling (that is the public hook in internal/fulfillment).
// Pool: the checkout pool (commerce_checkout_runtime); every definer it calls is EXECUTE-granted to that role only (X9 pool note).
// External hosts: none directly; ecpay.Client owns logistics(-stage).ecpay.com.tw for the directory lookup.

package checkout

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
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

	"livecommerce/internal/buyer"
	"livecommerce/internal/command"
	"livecommerce/internal/fulfillment"
	"livecommerce/internal/integrations/shipping/ecpay"
	"livecommerce/internal/platform"
)

// directory is the one ecpay.Client method the verify path needs, so tests can inject a fake directory.
type directory interface {
	StoreDirectory(ctx context.Context, cvsType string, cr ecpay.Credentials) (map[string]ecpay.Store, error)
}

// BuyerCVS serves the four buyer CVS routes on the checkout pool.
type BuyerCVS struct {
	pool   *pgxpool.Pool
	keys   *ecpay.Keyring
	client *ecpay.Client
	dir    directory
	cfg    fulfillment.CVSConfig
}

// NewBuyerCVS wires the buyer surface. keys/client may be nil while cfg.ECPay.Enabled is false: the buyer-entered store route needs
// neither. The pool must be the checkout authority.
func NewBuyerCVS(checkoutPool *pgxpool.Pool, keys *ecpay.Keyring, client *ecpay.Client, cfg fulfillment.CVSConfig) (*BuyerCVS, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := platform.ValidateCheckoutPool(ctx, checkoutPool); err != nil {
		return nil, err
	}
	if cfg.PaymentEnvironment != "" && cfg.PaymentEnvironment != "SANDBOX" && cfg.PaymentEnvironment != "LIVE" {
		return nil, command.ErrInvalid
	}
	if cfg.ECPay.Enabled && (keys == nil || client == nil || cfg.PaymentEnvironment == "" || cfg.ECPay.HooksOrigin == "") {
		return nil, command.ErrInvalid
	}
	b := &BuyerCVS{pool: checkoutPool, keys: keys, client: client, cfg: cfg}
	if client != nil {
		b.dir = client
	}
	return b, nil
}

var (
	returnPathPattern = regexp.MustCompile(`^/(zh-TW|zh-CN|en)/products/[A-Za-z0-9_-]{1,64}$`)
	storeCodePattern  = regexp.MustCompile(`^[0-9]{3,8}$`)
	serviceCodeRx     = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,39}$`)
)

// SelectionOpenInput is the exact POST /v1/buyer/cvs-selections body. return_origin is deliberately NOT a field: it is the
// authenticated storefront origin of the request (§4.3 open_cvs_selection).
type SelectionOpenInput struct {
	CartVersion int64  `json:"cart_version"`
	MarketID    string `json:"market_id"`
	ServiceCode string `json:"service_code"`
	ReturnPath  string `json:"return_path"`
}

// SelectionForm is the top-level auto-submit form: fixed ECPay map action per environment, never from input.
type SelectionForm struct {
	Action string            `json:"action"`
	Fields map[string]string `json:"fields"`
}

// SelectionOpened is the 201 body of POST cvs-selections.
type SelectionOpened struct {
	SelectionID string        `json:"selection_id"`
	ExpiresAt   time.Time     `json:"expires_at"`
	Form        SelectionForm `json:"form"`
}

// Open creates a selection and returns the map form. The 20-char MerchantTradeNo is generated here from crypto/rand; only its sha256 is
// stored, so a replay of the same key cannot re-derive it (SQL answers PT409 selection_replay_new_key and the client opens a new one).
func (b *BuyerCVS) Open(ctx context.Context, token, storeID, key, origin string, mobile bool, in SelectionOpenInput) (SelectionOpened, error) {
	if b == nil || !checkoutKey.MatchString(key) || in.CartVersion < 1 || !command.ValidID(in.MarketID) ||
		!serviceCodeRx.MatchString(in.ServiceCode) || origin == "" || len(origin) > 261 {
		return SelectionOpened{}, command.ErrInvalid
	}
	if !returnPathPattern.MatchString(in.ReturnPath) {
		return SelectionOpened{}, &fulfillment.CVSError{Status: 422, Code: "bad_return_path"}
	}
	if !b.cfg.ECPay.Enabled || b.cfg.PaymentEnvironment == "" {
		return SelectionOpened{}, &fulfillment.CVSError{Status: 422, Code: "service_unavailable"}
	}
	nonce, err := newNonce()
	if err != nil {
		return SelectionOpened{}, errCheckoutDatabase
	}
	nonceDigest := sha256.Sum256([]byte(nonce))
	request, _ := json.Marshal(struct {
		Op string `json:"op"`
		SelectionOpenInput
	}{"cvs.open", in})
	digest := sha256.Sum256(request)
	tokenHash := sha256.Sum256([]byte(token))
	var opened struct {
		SelectionID string    `json:"selection_id"`
		Subtype     string    `json:"subtype"`
		MerchantID  string    `json:"merchant_id"`
		Environment string    `json:"environment"`
		ExpiresAt   time.Time `json:"expires_at"`
	}
	err = buyer.WithScope(ctx, b.pool, token, storeID, func(callCtx context.Context, tx pgx.Tx, scope buyer.Scope) error {
		var raw []byte
		// fulfillment.open_cvs_selection: buyer scope, ACTIVE-domain origin, single enabled qualified profile, environment pin.
		if err := tx.QueryRow(callCtx, `SELECT fulfillment.open_cvs_selection($1,$2::uuid,$3,$4,$5::bigint,$6::uuid,$7,$8,$9,$10,$11)`,
			tokenHash[:], storeID, key, digest[:], in.CartVersion, in.MarketID, in.ServiceCode, nonceDigest[:], origin,
			in.ReturnPath, b.cfg.PaymentEnvironment).Scan(&raw); err != nil {
			return err
		}
		return decodeStrict(raw, &opened)
	})
	if err != nil {
		return SelectionOpened{}, cvsError(ctx, err)
	}
	if !command.ValidID(opened.SelectionID) || opened.Environment != b.cfg.PaymentEnvironment || opened.ExpiresAt.IsZero() {
		return SelectionOpened{}, command.ErrConflict
	}
	// ServerReplyURL = the hooks origin + map-return route of this selection (never a buyer-supplied URL); the browser posts to the
	// fixed ECPay map action for the environment. mobile => Device=1 (B20); the buyer never opens a new window or an iframe.
	action, fields := ecpay.MapForm(ecpay.Environment(opened.Environment), opened.MerchantID, nonce, opened.Subtype,
		strings.TrimRight(b.cfg.ECPay.HooksOrigin, "/")+"/v1/cvs/ecpay/map-return/"+opened.SelectionID, mobile)
	command.InLocalTime(&opened)
	return SelectionOpened{SelectionID: opened.SelectionID, ExpiresAt: opened.ExpiresAt, Form: SelectionForm{Action: action, Fields: fields}}, nil
}

// newNonce returns 20 characters of [A-Za-z0-9] from crypto/rand (rejection sampling: no modulo bias).
func newNonce() (string, error) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	out := make([]byte, 0, 20)
	var buf [32]byte
	for len(out) < 20 {
		if _, err := rand.Read(buf[:]); err != nil {
			return "", err
		}
		for _, c := range buf {
			if c < 248 { // 248 = 62*4: values above would bias the alphabet
				out = append(out, alphabet[int(c)%62])
				if len(out) == 20 {
					break
				}
			}
		}
	}
	return string(out), nil
}

// SelectionPickup is the verified store the buyer may confirm.
type SelectionPickup struct {
	PickupID string `json:"pickup_id"`
	Kind     string `json:"kind"`
	Code     string `json:"code"`
	Name     string `json:"name"`
	Address  string `json:"address"`
	Outside  bool   `json:"outside"`
}

// SelectionView is the GET / verify body: state, reject code, a retry hint while the directory is unavailable, and the pickup once
// VERIFIED (null otherwise).
type SelectionView struct {
	SelectionID string           `json:"selection_id"`
	State       string           `json:"state"`
	RejectCode  *string          `json:"reject_code"`
	RetryAfterS *int             `json:"retry_after_s"`
	Pickup      *SelectionPickup `json:"pickup"`
}

// selectionRow is the SQL projection; the extra keys let Verify continue without a second read.
type selectionRow struct {
	SelectionID     string           `json:"selection_id"`
	State           string           `json:"state"`
	RejectCode      *string          `json:"reject_code"`
	Kind            string           `json:"kind"`
	Subtype         string           `json:"subtype"`
	ReturnedStoreID *string          `json:"returned_store_id"`
	Pickup          *SelectionPickup `json:"pickup"`
}

func (r selectionRow) view() (SelectionView, error) {
	switch r.State {
	case "OPEN", "RETURNED", "EXPIRED":
		if r.Pickup != nil || r.RejectCode != nil {
			return SelectionView{}, command.ErrConflict
		}
	case "VERIFIED":
		if r.Pickup == nil || !command.ValidID(r.Pickup.PickupID) || r.RejectCode != nil {
			return SelectionView{}, command.ErrConflict
		}
	case "REJECTED":
		if r.Pickup != nil || r.RejectCode == nil {
			return SelectionView{}, command.ErrConflict
		}
	default:
		return SelectionView{}, command.ErrConflict
	}
	return SelectionView{SelectionID: r.SelectionID, State: r.State, RejectCode: r.RejectCode, Pickup: r.Pickup}, nil
}

// Get reads the selection of the calling buyer only (another buyer's id is an indistinguishable 404).
func (b *BuyerCVS) Get(ctx context.Context, token, storeID, selectionID string) (SelectionView, error) {
	if b == nil || !command.ValidID(selectionID) {
		return SelectionView{}, command.ErrInvalid
	}
	tokenHash := sha256.Sum256([]byte(token))
	var out SelectionView
	err := buyer.WithScope(ctx, b.pool, token, storeID, func(callCtx context.Context, tx pgx.Tx, scope buyer.Scope) error {
		row, err := b.readSelection(callCtx, tx, tokenHash[:], storeID, selectionID)
		if err != nil {
			return err
		}
		out, err = row.view()
		return err
	})
	if err != nil {
		return SelectionView{}, cvsError(ctx, err)
	}
	return out, nil
}

func (b *BuyerCVS) readSelection(ctx context.Context, tx pgx.Tx, tokenHash []byte, storeID, selectionID string) (selectionRow, error) {
	var raw []byte
	// fulfillment.read_cvs_selection: owner + session + cart must match the buyer scope.
	if err := tx.QueryRow(ctx, `SELECT fulfillment.read_cvs_selection($1,$2::uuid,$3::uuid)`, tokenHash, storeID, selectionID).Scan(&raw); err != nil {
		return selectionRow{}, err
	}
	var row selectionRow
	if err := decodeStrict(raw, &row); err != nil || row.SelectionID != selectionID {
		return selectionRow{}, command.ErrConflict
	}
	return row, nil
}

// directoryRetryAfter is the client hint while GetStoreList is unavailable (F11: a 403 bans the caller for 30 minutes, so the
// adapter remembers a failure for its throttle window and the buyer's page polls slowly).
const directoryRetryAfter = 30

// Verify completes a RETURNED selection: it looks the returned store up in the chain directory OUTSIDE any transaction, then calls
// verify_cvs_selection, which writes the directory-verified pickup source from the directory row (never from the browser POST). A
// directory failure answers 200 {state:"RETURNED", retry_after_s} without touching the selection.
func (b *BuyerCVS) Verify(ctx context.Context, token, storeID, selectionID string) (SelectionView, error) {
	if b == nil || !command.ValidID(selectionID) {
		return SelectionView{}, command.ErrInvalid
	}
	tokenHash := sha256.Sum256([]byte(token))
	var row selectionRow
	var creds ecpay.Credentials
	err := buyer.WithScope(ctx, b.pool, token, storeID, func(callCtx context.Context, tx pgx.Tx, scope buyer.Scope) error {
		var err error
		if row, err = b.readSelection(callCtx, tx, tokenHash[:], storeID, selectionID); err != nil {
			return err
		}
		if row.State != "RETURNED" || row.ReturnedStoreID == nil || b.keys == nil {
			return nil
		}
		// integration.load_ecpay_key_for_selection: only after read_cvs_selection proved the buyer owns this selection.
		var k struct {
			tenant, store, connection, merchant, environment, keyID string
			version                                                 int64
			nonce, ciphertext                                       []byte
		}
		err = tx.QueryRow(callCtx, `SELECT tenant_id::text,store_id::text,connection_id::text,merchant_id,environment,credential_version,key_id,nonce,ciphertext
			FROM integration.load_ecpay_key_for_selection($1::uuid)`, selectionID).Scan(&k.tenant, &k.store, &k.connection, &k.merchant,
			&k.environment, &k.version, &k.keyID, &k.nonce, &k.ciphertext)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // expired or no longer OPEN/RETURNED: the projection below reports it
		}
		if err != nil {
			return err
		}
		payload, err := b.keys.Open(ecpay.Scope{TenantID: k.tenant, StoreID: k.store, ConnectionID: k.connection, MerchantID: k.merchant,
			Environment: ecpay.Environment(k.environment), Version: k.version}, k.keyID, k.nonce, k.ciphertext)
		if err != nil {
			return nil // keyring misconfiguration: treated like an unavailable directory (retryable), never a verdict
		}
		creds = ecpay.Credentials{MerchantID: k.merchant, HashKey: payload.HashKey, HashIV: payload.HashIV}
		return nil
	})
	if err != nil {
		return SelectionView{}, cvsError(ctx, err)
	}
	if row.State != "RETURNED" || row.ReturnedStoreID == nil {
		view, err := row.view()
		return view, cvsError(ctx, err)
	}
	retry := func() (SelectionView, error) {
		after := directoryRetryAfter
		view, err := row.view()
		view.RetryAfterS = &after
		return view, err
	}
	if creds.HashKey == "" || b.dir == nil {
		return retry()
	}
	cvsType, err := ecpay.CVSType(row.Subtype)
	if err != nil {
		return retry() // OK mart has no directory: the store cannot be verified (stays RETURNED, never offered)
	}
	// ecpay.Client.StoreDirectory: cached until the next 20:00 Asia/Taipei, single-flight, failure remembered (§7.2).
	directoryCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
	stores, err := b.dir.StoreDirectory(directoryCtx, cvsType, creds)
	cancel()
	if err != nil {
		return retry()
	}
	store, hit := stores[*row.ReturnedStoreID]
	var out SelectionView
	err = buyer.WithScope(ctx, b.pool, token, storeID, func(callCtx context.Context, tx pgx.Tx, scope buyer.Scope) error {
		// Re-prove ownership in THIS transaction (a directory fetch may take seconds): verify is only reachable after a fresh read.
		if _, err := b.readSelection(callCtx, tx, tokenHash[:], storeID, selectionID); err != nil {
			return err
		}
		var raw []byte
		// fulfillment.verify_cvs_selection: writes/reuses the PROVIDER_DIRECTORY_VERIFIED pickup from the directory row.
		if err := tx.QueryRow(callCtx, `SELECT fulfillment.verify_cvs_selection($1::uuid,$2,$3,$4,$5,$6)`,
			selectionID, *row.ReturnedStoreID, hit, store.Name, store.Address, time.Now().UTC()).Scan(&raw); err != nil {
			return err
		}
		var next selectionRow
		if err := decodeStrict(raw, &next); err != nil || next.SelectionID != selectionID {
			return command.ErrConflict
		}
		var err error
		out, err = next.view()
		return err
	})
	if err != nil {
		return SelectionView{}, cvsError(ctx, err)
	}
	return out, nil
}

// StoreEntryInput is the exact POST /v1/buyer/cvs-stores body (§16.1).
type StoreEntryInput struct {
	CartVersion  int64  `json:"cart_version"`
	MarketID     string `json:"market_id"`
	ServiceCode  string `json:"service_code"`
	StoreCode    string `json:"store_code"`
	StoreName    string `json:"store_name"`
	StoreAddress string `json:"store_address"`
}

// StoreEntered is the 201 body: the buyer-entered pickup source, labelled as such (never "verified").
type StoreEntered struct {
	PickupID string `json:"pickup_id"`
	Kind     string `json:"kind"`
	Code     string `json:"code"`
	Name     string `json:"name"`
	Address  string `json:"address"`
	Source   string `json:"source"`
}

func boundedText(value string, min, max int) bool {
	if !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	n := utf8.RuneCountInString(value)
	if n < min || n > max {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// EnterStore records a store the buyer typed for a merchant without an ECPay connection: chain code format, name and address bounds are
// checked here for a fast 422 and again in SQL (the per-chain regex needs the service kind, which only the definer knows).
func (b *BuyerCVS) EnterStore(ctx context.Context, token, storeID, key string, in StoreEntryInput) (StoreEntered, error) {
	if b == nil || !checkoutKey.MatchString(key) || in.CartVersion < 1 || !command.ValidID(in.MarketID) || !serviceCodeRx.MatchString(in.ServiceCode) {
		return StoreEntered{}, command.ErrInvalid
	}
	switch {
	case !storeCodePattern.MatchString(in.StoreCode):
		return StoreEntered{}, &fulfillment.CVSError{Status: 422, Code: "bad_store_code"}
	case !boundedText(in.StoreName, 1, 40):
		return StoreEntered{}, &fulfillment.CVSError{Status: 422, Code: "bad_store_name"}
	case !boundedText(in.StoreAddress, 5, 120):
		return StoreEntered{}, &fulfillment.CVSError{Status: 422, Code: "bad_store_address"}
	}
	request, _ := json.Marshal(struct {
		Op string `json:"op"`
		StoreEntryInput
	}{"cvs.store", in})
	digest := sha256.Sum256(request)
	tokenHash := sha256.Sum256([]byte(token))
	var out StoreEntered
	err := buyer.WithScope(ctx, b.pool, token, storeID, func(callCtx context.Context, tx pgx.Tx, scope buyer.Scope) error {
		var raw []byte
		// fulfillment.record_buyer_cvs_store: BUYER_ENTERED, NULL principal, namespace buyer.<owner>; refused in ecpay_map mode.
		if err := tx.QueryRow(callCtx, `SELECT fulfillment.record_buyer_cvs_store($1,$2::uuid,$3,$4,$5::bigint,$6::uuid,$7,$8,$9,$10)`,
			tokenHash[:], storeID, key, digest[:], in.CartVersion, in.MarketID, in.ServiceCode, in.StoreCode, in.StoreName,
			in.StoreAddress).Scan(&raw); err != nil {
			return err
		}
		return decodeStrict(raw, &out)
	})
	if err != nil {
		return StoreEntered{}, cvsError(ctx, err)
	}
	if !command.ValidID(out.PickupID) || out.Source != "buyer_entered" || !fulfillment.ValidBuyerStoreCode(out.Kind, out.Code) {
		return StoreEntered{}, command.ErrConflict
	}
	return out, nil
}

// decodeStrict rejects unknown keys of a SQL-built projection (drift is an error, not data).
func decodeStrict(raw []byte, into any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return command.ErrConflict
	}
	return nil
}

// cvsError classifies a buyer CVS failure: coded refusals (PT409 replay/conflict, PT422 codes, PT429) surface as *fulfillment.CVSError,
// everything else through the shared safeError table.
func cvsError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	var refusal *fulfillment.CVSError
	if errors.As(err, &refusal) {
		return err
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "PT409" && cvsCode.MatchString(pg.Message) {
		// selection_replay_new_key, idempotency_conflict, ... : the fixed message is the contract code.
		return &fulfillment.CVSError{Status: 409, Code: pg.Message}
	}
	return safeError(ctx, err)
}
