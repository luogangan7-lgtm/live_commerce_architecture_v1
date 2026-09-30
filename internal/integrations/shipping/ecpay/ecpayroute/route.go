// Package ecpayroute owns the dispatcher route ecpay_logistics / ecpay.cvs_create / transactional of
// taiwan-cvs-logistics-v1 §7.4: the lease-fenced credential and shipment load, the ECPay Create and
// Query calls through the wire package, and the completion hook that runs inside the dispatcher's
// completion transaction. Its SQL is exactly two definers, integration.load_cvs_create and
// integration.finish_cvs_create, and it is the only SQL in the ECPay unit.
//
// It never creates a label twice for one operation: an UNKNOWN Create is only ever followed by a
// Query with the same frozen MerchantTradeNo (I06), it never plans operations or writes fulfilment
// tables (the SQL definers do), never logs or returns keys, recipient data, trade numbers or codes,
// and never returns an error for a value the provider sent (Finish is total over provider data).
//
// External: none directly; every ECPay call goes through package ecpay (logistics(-stage).ecpay.com.tw).
package ecpayroute

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"livecommerce/internal/integrations/core"
	"livecommerce/internal/integrations/shipping/ecpay"
	"livecommerce/internal/platform"
)

// Route identity (contract §7.4, TD5). One row of integration.operations per shipment attempt.
const (
	Provider = "ecpay_logistics"
	Action   = "ecpay.cvs_create"
	Purpose  = "transactional"
)

// Codes recorded by this package. They match the dispatcher's result-code pattern.
const (
	codeDisabled   = "cvs_disabled"
	codeBadRequest = "bad_request"
)

// denied is a LoadSecret refusal. The dispatcher records every LoadSecret denial as
// credential_unavailable (dispatcher.go secretLoadFailure), so the reason text only helps a reader of
// the error chain; the merchant's next request shows the precise reason from request_cvs_shipment.
func denied(reason string) error { return fmt.Errorf("%s: %w", reason, core.ErrPolicyDenied) }

var (
	errConfig     = errors.New("ecpayroute: invalid configuration")
	errLoadFailed = errors.New("ecpayroute: credential load failed")
	errFinish     = errors.New("ecpayroute: finish failed")
	errBadSecret  = errors.New("ecpayroute: malformed loaded credential")
	errWrongMode  = errors.New("ecpayroute: create refused outside dispatch mode")

	uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	// taipei is a fixed UTC+8 zone (no tzdata dependency, E8); Taiwan has no DST.
	taipei = time.FixedZone("Asia/Taipei", 8*3600)
)

// Routes returns the single ECPay CVS route. workerPool must be the commerce_worker pool
// (platform.ValidateWorkerPool): the dispatcher hands LoadSecret and Finish their own transactions, so
// the pool is only validated here, which stops the route from being wired with an owner or mixed-role
// pool. keys and client must be non-nil; the client's environment must be the deployment's.
func Routes(workerPool *pgxpool.Pool, keys *ecpay.Keyring, client *ecpay.Client, cfg ecpay.Config) ([]core.DispatchRoute, error) {
	if workerPool == nil || keys == nil || client == nil {
		return nil, errConfig
	}
	if err := platform.ValidateWorkerPool(context.Background(), workerPool); err != nil {
		return nil, err
	}
	return newRoutes(keys, client, cfg)
}

func newRoutes(keys *ecpay.Keyring, client *ecpay.Client, cfg ecpay.Config) ([]core.DispatchRoute, error) {
	if keys == nil || client == nil {
		return nil, errConfig
	}
	a := &adapter{keys: keys, client: client, cfg: cfg}
	return []core.DispatchRoute{{
		Provider: Provider, Action: Action, Purpose: Purpose,
		Check:               a.check,
		LoadSecret:          a.loadSecret,
		DispatchWithSecret:  a.dispatch,
		ReconcileWithSecret: a.reconcile,
		Finish:              a.finish,
	}}, nil
}

type adapter struct {
	keys   *ecpay.Keyring
	client *ecpay.Client
	cfg    ecpay.Config
}

// queryRower and execer are the two pgx.Tx methods the route uses, so unit tests need no database.
type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// check is Check (every attempt, no tx, no Secret): the request must be exactly {order_id, attempt}
// and the ECPay switch must be on. A denial is a coded policy denial: in dispatch mode nothing was
// sent so BLOCKED_POLICY is truthful; in reconcile mode the dispatcher records UNKNOWN instead, never
// BLOCKED, because a create may already exist (A10-D2). Everything that needs SQL is in loadSecret.
func (a *adapter) check(_ context.Context, req core.DispatchRequest) error {
	if !a.cfg.Enabled {
		return core.DenyPolicy(codeDisabled)
	}
	var r struct {
		OrderID string `json:"order_id"`
		Attempt int    `json:"attempt"`
	}
	dec := json.NewDecoder(bytes.NewReader(req.Request))
	dec.DisallowUnknownFields()
	var extra json.RawMessage
	if err := dec.Decode(&r); err != nil || dec.Decode(&extra) == nil || !uuidRE.MatchString(r.OrderID) || r.Attempt < 1 {
		return core.DenyPolicy(codeBadRequest)
	}
	return nil
}

// loadedShipment is the row integration.load_cvs_create returns (§4.3): the frozen shipment fields,
// the recipient snapshot and the frozen credential version's ciphertext.
// COLUMN CONTRACT (owned by cvs-core; the §4.3 row lists them in prose only): tenant_id, store_id,
// connection_id, environment, credential_version, merchant_id, key_id, nonce, ciphertext, endpoint_id,
// logistics_subtype, receiver_store_id, merchant_trade_no, trade_created_at (timestamptz = MerchantTradeDate),
// goods_amount, collection_amount (NULL for a card order), recipient_name, recipient_phone (NULL in reconcile
// mode, TD8: coalesced to the empty string because pgx cannot scan NULL into string). route_pg_test.go prepares loadSQL
// and finishSQL against the migrated schema so a renamed column fails a test, not the first label.
type loadedShipment struct {
	tenant, store, connection, environment string
	version                                int64
	merchantID, keyID                      string
	nonce, ciphertext                      []byte
	endpoint, subType, receiverStore       string
	tradeNo                                string
	tradeDate                              time.Time
	goods                                  int32
	collection                             *int32
	recipientName, recipientPhone          string
}

const loadSQL = `SELECT tenant_id::text, store_id::text, connection_id::text, environment, credential_version, merchant_id,
	key_id, nonce, ciphertext, endpoint_id::text, logistics_subtype, receiver_store_id, merchant_trade_no,
	trade_created_at, goods_amount, collection_amount, coalesce(recipient_name, ''), coalesce(recipient_phone, '')
	FROM integration.load_cvs_create($1::uuid,$2::bigint,$3::bytea,$4::text)`

// secretDoc is what travels from LoadSecret to the callbacks inside core.Secret: the decrypted
// credential plus every frozen field the callbacks need (the dispatcher never passes them the row).
// It holds recipient PII, so it exists only as the Secret's bytes, which the dispatcher zeroes.
type secretDoc struct {
	MerchantID, HashKey, HashIV, SenderName, SenderCellPhone string
	TradeNo, TradeDate, SubType, StoreCode                   string
	RecipientName, RecipientPhone, ServerReplyURL            string
	Goods, Collection                                        int
}

// loadSecret is LoadSecret: its only SQL is the lease-fenced loader. It runs in dispatch and (R-7b)
// reconcile mode. E4: SQLSTATE PT409 (not REQUESTED, profile disabled, credential moved, not payable)
// is a policy denial; 40001/22023/P0002 and everything else are plain errors (dispatcher: UNKNOWN
// secret_load_failed, reconcile follows). In dispatch mode a denial means zero requests were sent.
func (a *adapter) loadSecret(ctx context.Context, tx pgx.Tx, claim core.SecretClaim) (core.Secret, error) {
	return a.load(ctx, tx, claim)
}

func (a *adapter) load(ctx context.Context, q queryRower, claim core.SecretClaim) (core.Secret, error) {
	var l loadedShipment
	// integration.load_cvs_create: lease-fenced, frozen credential version (§4.3); commerce_worker EXECUTE only.
	err := q.QueryRow(ctx, loadSQL, claim.OperationID, claim.Generation, claim.LeaseToken, claim.Mode).Scan(
		&l.tenant, &l.store, &l.connection, &l.environment, &l.version, &l.merchantID, &l.keyID, &l.nonce, &l.ciphertext,
		&l.endpoint, &l.subType, &l.receiverStore, &l.tradeNo, &l.tradeDate, &l.goods, &l.collection,
		&l.recipientName, &l.recipientPhone)
	if err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "PT409" {
			return core.Secret{}, fmt.Errorf("cvs create refused: %w", core.ErrPolicyDenied)
		}
		return core.Secret{}, errLoadFailed
	}
	dispatch := claim.Mode == "dispatch"
	// Environment pin: a LIVE credential must never reach the stage host or the reverse. Refusal in
	// dispatch mode = zero requests; in reconcile mode the dispatcher keeps the operation UNKNOWN.
	if ecpay.Environment(l.environment) != a.client.Environment() {
		return core.Secret{}, denied("environment_mismatch")
	}
	if l.tradeNo != ecpay.MerchantTradeNo(claim.OperationID) {
		// The row disagrees with E2: nothing was sent under this row's number, so it is not used.
		return core.Secret{}, denied("trade_no_mismatch")
	}
	if dispatch {
		if l.environment == string(ecpay.EnvLive) && !a.cfg.LiveCreate {
			return core.Secret{}, denied("live_create_disabled") // §0.3 P4: no label purchase before owner approval
		}
		if _, ok := ecpay.RecipientOK(l.recipientName, l.recipientPhone); !ok {
			return core.Secret{}, denied("recipient_rejected") // twin of fulfillment.ecpay_recipient_ok
		}
	}
	payload, err := a.keys.Open(ecpay.Scope{
		TenantID: l.tenant, StoreID: l.store, ConnectionID: l.connection, MerchantID: l.merchantID,
		Environment: ecpay.Environment(l.environment), Version: l.version,
	}, l.keyID, l.nonce, l.ciphertext)
	if err != nil {
		// A credential that cannot be opened cannot sign anything: dispatch mode sent nothing, so it
		// is a denial (the merchant can rotate); reconcile mode keeps the operation UNKNOWN.
		return core.Secret{}, denied("credential_unavailable")
	}
	doc := secretDoc{
		MerchantID: l.merchantID, HashKey: payload.HashKey, HashIV: payload.HashIV,
		SenderName: payload.SenderName, SenderCellPhone: payload.SenderCellPhone,
		TradeNo: l.tradeNo, TradeDate: l.tradeDate.In(taipei).Format("2006/01/02 15:04:05"),
		SubType: l.subType, StoreCode: l.receiverStore, RecipientName: l.recipientName, RecipientPhone: l.recipientPhone,
		ServerReplyURL: a.cfg.HooksOrigin + "/v1/cvs/ecpay/status/" + l.endpoint, Goods: int(l.goods),
	}
	if l.collection != nil {
		doc.Collection = int(*l.collection)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return core.Secret{}, errBadSecret
	}
	// ponytail: Go strings cannot be zeroed; the decoded credential lives in process memory for one call.
	s := core.NewSecret(raw)
	clear(raw)
	return s, nil
}

func decodeSecret(s core.Secret) (secretDoc, error) {
	var d secretDoc
	if json.Unmarshal(s.Reveal(), &d) != nil || d.MerchantID == "" || d.TradeNo == "" {
		return secretDoc{}, errBadSecret
	}
	return d, nil
}

// detail is the route-private Outcome.Detail handed to finish (comparable; never marshalled, R-7a).
type detail struct{ LogisticsID, PaymentNo, ValidationNo, ShipmentNo, StatusCode string }

func outcomeOf(r ecpay.Result) core.Outcome {
	o := core.Outcome{State: r.Outcome, Code: r.Code, Detail: detail{r.LogisticsID, r.PaymentNo, r.ValidationNo, r.ShipmentNo, r.StatusCode}}
	if r.Outcome == "SUCCEEDED" {
		o.ProviderReference = r.LogisticsID // AllPayLogisticsID, checked by the adapter against the column CHECK
	}
	return o
}

// dispatch is DispatchWithSecret: exactly one Express/Create with the frozen trade number and date.
// I06: whatever Create returns, this operation never creates again. UNKNOWN (timeout, bad MAC, 5xx,
// 403) is returned as UNKNOWN and the dispatcher's next claim is reconcile mode, which only queries
// the same MerchantTradeNo; a second Create would buy a second label (§13.3).
func (a *adapter) dispatch(ctx context.Context, req core.DispatchRequest, secret core.Secret) (core.Outcome, error) {
	if req.Mode == "reconcile" {
		return core.Outcome{}, errWrongMode
	}
	d, err := decodeSecret(secret)
	if err != nil {
		return core.Outcome{}, err
	}
	res := a.client.Create(ctx,
		ecpay.Credentials{MerchantID: d.MerchantID, HashKey: d.HashKey, HashIV: d.HashIV},
		ecpay.Payload{HashKey: d.HashKey, HashIV: d.HashIV, SenderName: d.SenderName, SenderCellPhone: d.SenderCellPhone},
		ecpay.CreateRequest{
			SubType: d.SubType, MerchantTradeNo: d.TradeNo, MerchantTradeDate: d.TradeDate, ReceiverStoreID: d.StoreCode,
			ReceiverName: d.RecipientName, ReceiverPhone: d.RecipientPhone, GoodsName: "商品", ServerReplyURL: d.ServerReplyURL,
			GoodsAmount: d.Goods, CollectionAmount: d.Collection, // I05: frozen server-computed TWD integers (§16.3, F20)
		})
	return outcomeOf(res), nil
}

// reconcile is ReconcileWithSecret (R-7b): Query/V5 by the frozen trade number, read-only. A
// MAC-verified trade is SUCCEEDED with its codes; everything else stays UNKNOWN. It never creates.
func (a *adapter) reconcile(ctx context.Context, _ core.DispatchRequest, secret core.Secret) (core.Outcome, error) {
	d, err := decodeSecret(secret)
	if err != nil {
		return core.Outcome{}, err
	}
	res := a.client.Query(ctx, ecpay.Credentials{MerchantID: d.MerchantID, HashKey: d.HashKey, HashIV: d.HashIV}, d.TradeNo)
	return outcomeOf(res), nil
}

const finishSQL = `SELECT integration.finish_cvs_create($1::uuid,$2::bigint,$3::bytea,$4::text,$5::text,$6::text,$7::text,$8::text,$9::text,$10::text)`

// finish is Finish (R-7a): it runs inside the dispatcher's completion transaction, before
// Service.Complete, on every completion path including BLOCKED_POLICY and UNKNOWN. It is total over
// provider data: the codes come from ecpay.Result (bounded, UTF-8 clean, the adapter-checked
// LogisticsID) and are passed as NULL when absent, so the SQL never sees an empty string as a
// "non-conforming code". Only a database fault is returned as an error, which rolls the completion
// back (the operation stays DISPATCHING and the next claim reconciles).
func (a *adapter) finish(ctx context.Context, tx pgx.Tx, claim core.SecretClaim, out core.Outcome) error {
	return a.finishOn(ctx, tx, claim, out)
}

func (a *adapter) finishOn(ctx context.Context, tx execer, claim core.SecretClaim, out core.Outcome) error {
	d, _ := out.Detail.(detail) // absent for dispatcher-made outcomes (BLOCKED_POLICY, UNKNOWN, budget)
	// integration.finish_cvs_create: lease-fenced; writes the shipment through fulfillment.apply_cvs_create_result (§4.3).
	if _, err := tx.Exec(ctx, finishSQL, claim.OperationID, claim.Generation, claim.LeaseToken, out.State, out.Code,
		nullable(d.LogisticsID), nullable(d.PaymentNo), nullable(d.ValidationNo), nullable(d.ShipmentNo), nullable(d.StatusCode)); err != nil {
		return errFinish
	}
	return nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
