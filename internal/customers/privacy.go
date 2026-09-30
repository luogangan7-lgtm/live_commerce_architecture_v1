// privacy.go serves consent and owner-level privacy actions: merchant withdrawal, export and erasure
// (customers:privacy) and the buyer's own consent, privacy state, export and erasure (CD4, CD6-CD8).
//
// Every function runs inside the caller's transaction. Authority is decided by the 0078 definers (they verify
// the merchant GUCs or resolve the buyer capability from the token hash); this file never trusts a tenant,
// store or owner id from a request. Retries: every write carries a caller Idempotency-Key mapped to a uuid
// (KeyUUID); the same key repeats the stored result, a different body under the same key is
// ErrIdempotencyConflict, so retrying after an UNKNOWN commit result is always safe and never re-grants.
//
// Non-goals: no marketing send, no Meta call, no ads.put_capi_context (A-3 belongs to meta-ads; internal/buyerhttp/privacy.go
// consentPut calls it through internal/attribution), no actor-level deletion (U08), no async queue.

package customers

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"livecommerce/internal/buyer"
	"livecommerce/internal/command"
	"livecommerce/internal/merchantorders"
	"livecommerce/internal/platform"
)

// WithdrawConsent records a merchant-side withdrawal (never a grant, CD4). Idempotent per key.
func WithdrawConsent(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key, customerID string, in WithdrawInput) (ConsentResult, error) {
	keyID, err := KeyUUID(key)
	if err != nil || tx == nil || !validAuthorityInput(scope, token) || !command.ValidID(customerID) || !ValidPair(in.Purpose, in.Channel) {
		return ConsentResult{}, command.ErrInvalid
	}
	hash := sha256.Sum256([]byte(token))
	var raw []byte
	// customers.merchant_withdraw_consent: customers:privacy, owner row lock, append-only insert + audit row.
	err = tx.QueryRow(ctx, `SELECT customers.merchant_withdraw_consent($1,$2::uuid,$3::uuid,$4,$5,$6::uuid)`,
		hash[:], scope.StoreID, customerID, in.Purpose, in.Channel, keyID).Scan(&raw)
	if err != nil {
		return ConsentResult{}, mapMerchantError(err)
	}
	return decodeConsentResult(raw, in.Purpose, in.Channel)
}

// Export builds the merchant export document and records the EXPORT row in the same transaction. The document
// is built and size-checked first, so an oversized export (> 200 orders or > 1 MiB) returns ErrExportTooLarge
// and the caller's rollback leaves no row (D8). Requires customers:read and orders:read besides customers:privacy.
func Export(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key, customerID string) ([]byte, error) {
	keyID, err := KeyUUID(key)
	if err != nil {
		return nil, err
	}
	d, total, err := loadDetail(ctx, tx, scope, token, customerID)
	if err != nil {
		return nil, err
	}
	if total > MaxExportOrders {
		return nil, ErrExportTooLarge
	}
	doc := exportDoc{Format: ExportFormat, GeneratedAt: time.Now().UTC().Format(TimestampLayout), CustomerID: customerID,
		Orders: make([]merchantorders.Detail, 0, total), Consents: d.ConsentHistory, Claims: d.Claims, PrivacyActions: d.PrivacyActions}
	if err = tx.QueryRow(ctx, `SELECT name FROM control.stores WHERE tenant_id=$1::uuid AND id=$2::uuid`,
		scope.TenantID, scope.StoreID).Scan(&doc.Store.Name); err != nil {
		return nil, mapMerchantError(err)
	}
	for _, id := range d.OrderIDs {
		// merchantorders.Get: existing projection, so the export equals the order page (D8).
		order, err := merchantorders.Get(ctx, tx, scope, token, id)
		if err != nil {
			return nil, err
		}
		if order.OrderID != id {
			return nil, ErrUnavailable
		}
		doc.Orders = append(doc.Orders, order)
	}
	body, err := marshalBounded(doc, len(doc.Orders))
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256([]byte(token))
	var stored []byte
	// customers.record_export: EXPORT row + audit customers.exported in this transaction; a replayed key returns the row.
	err = tx.QueryRow(ctx, `SELECT customers.record_export($1,$2::uuid,$3::uuid,'merchant',$4::uuid,$5::jsonb)`,
		hash[:], scope.StoreID, customerID, keyID,
		exportSummary(len(doc.Orders), len(doc.Consents), len(doc.Claims), len(doc.PrivacyActions))).Scan(&stored)
	if err != nil {
		return nil, mapMerchantError(err)
	}
	return body, nil
}

// Erase applies CD7 to one owner. ErrErasureBlocked while a hold, a live Stripe session or a non-terminal refund
// exists; a repeat (same or another key) returns the stored counts. Orders, facts and shipments are retained.
func Erase(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key, customerID string) (ErasureSummary, error) {
	keyID, err := KeyUUID(key)
	if err != nil || tx == nil || !validAuthorityInput(scope, token) || !command.ValidID(customerID) {
		return ErasureSummary{}, command.ErrInvalid
	}
	hash := sha256.Sum256([]byte(token))
	var raw []byte
	// customers.erase_owner: CD7 refusal + redaction + tombstone + audit, one transaction.
	err = tx.QueryRow(ctx, `SELECT customers.erase_owner($1,$2::uuid,$3::uuid,'merchant',$4::uuid)`,
		hash[:], scope.StoreID, customerID, keyID).Scan(&raw)
	if err != nil {
		return ErasureSummary{}, mapMerchantError(err)
	}
	return decodeErasure(raw)
}

// ReadBuyerPrivacy returns the caller's current consents and whether an erasure exists.
func ReadBuyerPrivacy(ctx context.Context, tx pgx.Tx, s buyer.Scope, token string) (BuyerPrivacy, error) {
	if tx == nil || !validBuyerInput(s, token) {
		return BuyerPrivacy{}, command.ErrInvalid
	}
	hash := sha256.Sum256([]byte(token))
	var raw []byte
	// customers.buyer_read_privacy: the buyer has no table grant on customers.*; the definer resolves the capability.
	if err := tx.QueryRow(ctx, `SELECT customers.buyer_read_privacy($1,$2::uuid,false)`, hash[:], s.StoreID).Scan(&raw); err != nil {
		return BuyerPrivacy{}, mapBuyerError(err)
	}
	var wire struct {
		StoreName string   `json:"store_name"`
		Consents  Consents `json:"consents"`
		Erased    bool     `json:"erased"`
	}
	if err := exactKeys(raw, "store_name", "consents", "erased"); err != nil || strict(raw, &wire) != nil {
		return BuyerPrivacy{}, ErrUnavailable
	}
	return BuyerPrivacy{Consents: wire.Consents, Erased: wire.Erased}, nil
}

// BuyerSetConsent records the buyer's own grant or withdrawal. Source and policy version come from the server
// (context maps to a source, PrivacyPolicyVersion is the deployed notice), never from the body (CD4, D12).
//
// The caller opens a buyer-pool transaction WITHOUT buyer.WithScope and passes the store its origin resolved: the
// definer takes the owner FOR UPDATE before it resolves the capability, and a prior resolve_scope (FOR SHARE) would
// make two concurrent requests of one buyer deadlock on the upgrade.
func BuyerSetConsent(ctx context.Context, tx pgx.Tx, storeID, token, key string, in ConsentInput) (ConsentResult, error) {
	keyID, err := KeyUUID(key)
	if err != nil || tx == nil || !command.ValidID(storeID) || !validBuyerToken(token) || !ValidPair(in.Purpose, in.Channel) {
		return ConsentResult{}, command.ErrInvalid
	}
	source, err := consentSourceFor(in.Context)
	if err != nil {
		return ConsentResult{}, err
	}
	hash := sha256.Sum256([]byte(token))
	var raw []byte
	// customers.buyer_set_consent: owner lock, always-insert per key, PT409 on a different body under the same key.
	err = tx.QueryRow(ctx, `SELECT customers.buyer_set_consent($1,$2::uuid,$3,$4,$5,$6,$7,$8::uuid)`,
		hash[:], storeID, in.Purpose, in.Channel, in.Granted, source, PrivacyPolicyVersion, keyID).Scan(&raw)
	if err != nil {
		return ConsentResult{}, mapBuyerError(err)
	}
	return decodeConsentResult(raw, in.Purpose, in.Channel)
}

// BuyerExport builds the buyer's own export (same envelope as the merchant's, no customer_id or principal ids)
// and records the EXPORT row in the same transaction; > 200 orders or > 1 MiB is ErrExportTooLarge (rollback).
//
// Like BuyerSetConsent it must be the first buyer-scope work of its transaction (no buyer.WithScope): the first
// definer, buyer_read_privacy(detail), takes the owner FOR UPDATE that record_export needs later.
func BuyerExport(ctx context.Context, tx pgx.Tx, storeID, token, key string) ([]byte, error) {
	keyID, err := KeyUUID(key)
	if err != nil || tx == nil || !command.ValidID(storeID) || !validBuyerToken(token) {
		return nil, command.ErrInvalid
	}
	hash := sha256.Sum256([]byte(token))
	var rawPrivacy, rawOrders []byte
	// customers.buyer_read_privacy(detail): consent history, bound-bundle summaries, privacy actions, store name.
	if err = tx.QueryRow(ctx, `SELECT customers.buyer_read_privacy($1,$2::uuid,true)`, hash[:], storeID).Scan(&rawPrivacy); err != nil {
		return nil, mapBuyerError(err)
	}
	// customers.buyer_export_orders: the buyer pool cannot read checkout tables, so a definer returns the caller's orders.
	if err = tx.QueryRow(ctx, `SELECT customers.buyer_export_orders($1,$2::uuid)`, hash[:], storeID).Scan(&rawOrders); err != nil {
		return nil, mapBuyerError(err)
	}
	var priv struct {
		StoreName      string          `json:"store_name"`
		Consents       Consents        `json:"consents"`
		Erased         bool            `json:"erased"`
		ConsentHistory []ConsentEvent  `json:"consent_history"`
		Claims         []ClaimSummary  `json:"claims"`
		PrivacyActions []PrivacyAction `json:"privacy_actions"`
	}
	if err = exactKeys(rawPrivacy, "store_name", "consents", "erased", "consent_history", "claims", "privacy_actions"); err != nil ||
		strict(rawPrivacy, &priv) != nil || priv.ConsentHistory == nil || priv.Claims == nil || priv.PrivacyActions == nil {
		return nil, ErrUnavailable
	}
	for _, e := range priv.ConsentHistory {
		if !validEvent(e) {
			return nil, ErrUnavailable
		}
	}
	for _, c := range priv.Claims {
		if !validClaim(c) {
			return nil, ErrUnavailable
		}
	}
	for _, a := range priv.PrivacyActions {
		if !validAction(a) {
			return nil, ErrUnavailable
		}
	}
	var orders []json.RawMessage
	if json.Unmarshal(rawOrders, &orders) != nil || orders == nil || len(orders) > MaxExportOrders+1 {
		return nil, ErrUnavailable
	}
	doc := buyerExportDoc{Format: ExportFormat, GeneratedAt: time.Now().UTC().Format(TimestampLayout),
		Store: exportStore{Name: priv.StoreName}, Orders: orders, Consents: priv.ConsentHistory, Claims: priv.Claims,
		PrivacyActions: priv.PrivacyActions}
	body, err := marshalBounded(doc, len(orders))
	if err != nil {
		return nil, err
	}
	var stored []byte
	// customers.record_export: buyer via; EXPORT row in this transaction, replayed key returns the stored row.
	err = tx.QueryRow(ctx, `SELECT customers.record_export($1,$2::uuid,NULL,'buyer',$3::uuid,$4::jsonb)`,
		hash[:], storeID, keyID,
		exportSummary(len(orders), len(doc.Consents), len(doc.Claims), len(doc.PrivacyActions))).Scan(&stored)
	if err != nil {
		return nil, mapBuyerError(err)
	}
	return body, nil
}

// BuyerErase erases the caller's owner. D11: it does NOT use buyer.WithScope, because a revoked capability must
// reach erase_owner to get PT410 (a retry after success). The caller opens a buyer-pool transaction and passes
// only the token and the store the origin resolved; the definer authenticates from the token hash.
func BuyerErase(ctx context.Context, tx pgx.Tx, token, storeID, key string) (ErasureSummary, error) {
	keyID, err := KeyUUID(key)
	if err != nil || tx == nil || !validBuyerToken(token) || !command.ValidID(storeID) {
		return ErasureSummary{}, command.ErrInvalid
	}
	hash := sha256.Sum256([]byte(token))
	var raw []byte
	// customers.erase_owner (buyer via): PT410 for a retry after success, PT409 erasure_blocked while money is in flight.
	err = tx.QueryRow(ctx, `SELECT customers.erase_owner($1,$2::uuid,NULL,'buyer',$3::uuid)`, hash[:], storeID, keyID).Scan(&raw)
	if err != nil {
		return ErasureSummary{}, mapBuyerError(err)
	}
	return decodeErasure(raw)
}

func validBuyerInput(s buyer.Scope, token string) bool {
	return command.ValidID(s.TenantID) && command.ValidID(s.StoreID) && command.ValidID(s.OwnerID) && validBuyerToken(token)
}

// validBuyerToken mirrors the buyer capability token grammar (43-char base64url of 32 bytes).
func validBuyerToken(value string) bool {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return len(value) == 43 && err == nil && len(decoded) == 32
}
