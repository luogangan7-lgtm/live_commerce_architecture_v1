// cvs_hooks.go hosts the two PUBLIC provider callbacks of taiwan-cvs-logistics-v1 that ECPay (or a buyer's browser on ECPay's behalf)
// posts to, on the dedicated hooks host: POST /v1/cvs/ecpay/map-return/{selection_id} (§5.2) and POST /v1/cvs/ecpay/status/{endpoint_id}
// (§7.5). Neither carries a cookie or bearer token: authority is the selection nonce (map return) or the MAC over every field
// (status), both verified before anything is written.
//
// Non-goals: no wire parsing rules (ecpay.ParseMapReturn / ParseStatus own them), no state rule (record_cvs_map_return,
// verify_cvs_selection and ingest_ecpay_status decide), no request body, key, recipient field, store field or trade number in a log
// line or in the response (I11: the 303 Location holds only the stored origin, path and the selection id; the status reply is
// exactly "1|OK" or a fixed refusal).
// Callers: cmd/api mounts HooksHandler under /v1/cvs/ecpay/ (integrator hook). External hosts: none here (the inline directory check
// uses ecpay.Client's cache only, unit default C10).

package fulfillment

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"livecommerce/internal/command"
	"livecommerce/internal/integrations/shipping/ecpay"
)

// Limits of §5.2 / §7.5: map return <= 8 KiB, status <= 16 KiB, a 5 s body read, at most 4 concurrent status posts per endpoint.
const (
	mapReturnMaxBody = 8 << 10
	statusMaxBody    = 16 << 10
	hookBodyTimeout  = 5 * time.Second
	hookTxTimeout    = 10 * time.Second
	// mapGlobalCap bounds concurrent map-return transactions (each holds a commerce_runtime connection and a row lock in
	// record_cvs_map_return); far below the pool size, far above real buyer concurrency (each tx takes milliseconds).
	mapGlobalCap = 16
)

// endpointGates caps concurrent status ingress per endpoint id (the Stripe "ingress can be exhausted" finding applies).
type endpointGates struct {
	mu  sync.Mutex
	n   map[string]int
	max int
}

func newEndpointGates(max int) *endpointGates { return &endpointGates{n: map[string]int{}, max: max} }

func (g *endpointGates) enter(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.n[key] >= g.max {
		return false
	}
	g.n[key]++
	return true
}

func (g *endpointGates) leave(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.n[key] <= 1 {
		delete(g.n, key)
		return
	}
	g.n[key]--
}

// HooksHandler serves the two hooks; every other path under it is 404. It is a 404 handler when CVS_ECPAY_ENABLED is off.
func (c *CVS) HooksHandler() http.Handler {
	if !c.cfg.ECPay.Enabled {
		return http.NotFoundHandler()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/cvs/ecpay/map-return/{selection_id}", c.mapReturn)
	mux.HandleFunc("POST /v1/cvs/ecpay/status/{endpoint_id}", c.statusIngress)
	return mux
}

func plain(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// readHookBody reads at most limit bytes within the body timeout; it reports 413 for an oversize and 400 for a slow or broken body.
func readHookBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
	if r.URL.RawQuery != "" {
		plain(w, http.StatusBadRequest, "0|invalid")
		return nil, false
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(hookBodyTimeout))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			plain(w, http.StatusRequestEntityTooLarge, "0|too large")
		} else {
			plain(w, http.StatusBadRequest, "0|invalid")
		}
		return nil, false
	}
	return body, true
}

type mapReturnResult struct {
	Known           bool    `json:"known"`
	Applied         bool    `json:"applied"`
	State           string  `json:"state"`
	RejectCode      *string `json:"reject_code"`
	ReturnOrigin    string  `json:"return_origin"`
	ReturnPath      string  `json:"return_path"`
	Subtype         string  `json:"subtype"`
	ReturnedStoreID *string `json:"returned_store_id"`
}

// mapReturn records the browser's e-map return (unsigned: F3 has no CheckMacValue), verifies the store against the cached
// GetStoreList directory when that is possible without a network call (C10), and ALWAYS answers a known selection with 303 to the
// stored return origin + path (§5.2). An unknown selection is a bare 404 with no Location.
func (c *CVS) mapReturn(w http.ResponseWriter, r *http.Request) {
	selection := r.PathValue("selection_id")
	if !command.ValidID(selection) {
		plain(w, http.StatusNotFound, "not found")
		return
	}
	body, ok := readHookBody(w, r, mapReturnMaxBody)
	if !ok {
		return
	}
	// ecpay.ParseMapReturn: F3 subset, duplicate keys -> ErrInvalid, unknown fields ignored.
	mr, err := ecpay.ParseMapReturn(body)
	if err != nil {
		plain(w, http.StatusBadRequest, "0|invalid")
		return
	}
	// The hook is public and unsigned: bound the transactions a flood of well-formed selection ids can open (per selection and
	// globally) so the pool stays available for merchants. Taken after the body read so slow senders cannot hold a slot.
	// 503 is retryable by the browser/ECPay; nothing was recorded.
	if !c.gates.enter("map:" + selection) {
		plain(w, http.StatusServiceUnavailable, "busy")
		return
	}
	defer c.gates.leave("map:" + selection)
	if !c.mapGates.enter("*") {
		plain(w, http.StatusServiceUnavailable, "busy")
		return
	}
	defer c.mapGates.leave("*")
	// Only the sha256 of the nonce is compared (and only the digest is stored): the MerchantTradeNo itself never persists.
	digest := sha256.Sum256([]byte(mr.MerchantTradeNo))
	ctx, cancel := context.WithTimeout(r.Context(), hookTxTimeout)
	defer cancel()
	var result mapReturnResult
	err = c.hookTx(ctx, func(tx pgx.Tx) error {
		var raw []byte
		// fulfillment.record_cvs_map_return: nonce first, then expiry / merchant / subtype / store id shape; a wrong nonce writes nothing.
		if err := tx.QueryRow(ctx, `SELECT fulfillment.record_cvs_map_return($1::uuid,$2,$3,$4,$5,$6)`,
			selection, digest[:], mr.MerchantID, mr.SubType, mr.StoreID, mr.Outside).Scan(&raw); err != nil {
			return err
		}
		return decodeCVS(raw, &result)
	})
	if err != nil {
		plain(w, http.StatusServiceUnavailable, "unavailable") // the browser may retry; nothing was acknowledged
		return
	}
	if !result.Known {
		plain(w, http.StatusNotFound, "not found")
		return
	}
	if result.Applied && result.State == "RETURNED" && result.ReturnedStoreID != nil {
		c.verifyFromCache(ctx, selection, result.Subtype, *result.ReturnedStoreID)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Location", result.ReturnOrigin+result.ReturnPath+"?cvs_selection="+selection)
	w.WriteHeader(http.StatusSeeOther)
}

// verifyFromCache is the inline half of TD3: when the chain's directory is cached and fresh the answer (hit or miss) is final;
// otherwise the selection stays RETURNED and the buyer's verify retries with a real lookup. A failure here never fails the redirect.
func (c *CVS) verifyFromCache(ctx context.Context, selection, subtype, storeID string) {
	cvsType, err := ecpay.CVSType(subtype)
	if err != nil {
		return // OK mart has no directory (F10/F18): stays RETURNED, the storefront never offers it until ok_verified
	}
	store, hit, fresh := c.client.CachedStore(cvsType, storeID)
	if !fresh {
		return
	}
	_ = c.hookTx(ctx, func(tx pgx.Tx) error {
		var raw []byte
		// fulfillment.verify_cvs_selection: name/address come from the directory row, never from the browser POST.
		return tx.QueryRow(ctx, `SELECT fulfillment.verify_cvs_selection($1::uuid,$2,$3,$4,$5,$6)`,
			selection, storeID, hit, store.Name, store.Address, time.Now().UTC()).Scan(&raw)
	})
}

// hookTx runs fn in one READ COMMITTED transaction on the main pool without a merchant scope: the definers it calls pin their own.
func (c *CVS) hookTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := c.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		cleanup, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		_ = tx.Rollback(cleanup)
		return err
	}
	return tx.Commit(ctx)
}

// statusIngress verifies CheckMacValue over every field with the endpoint's connection keys, stores the report in one transaction
// and only after COMMIT answers exactly "1|OK". A report for an attempt still REQUESTED gets 503 (ECPay retries; the create's Finish
// decides first). A bad MAC, wrong MerchantID or malformed body stores nothing and is never acknowledged.
func (c *CVS) statusIngress(w http.ResponseWriter, r *http.Request) {
	endpoint := r.PathValue("endpoint_id")
	if !command.ValidID(endpoint) {
		plain(w, http.StatusNotFound, "not found")
		return
	}
	if !c.gates.enter(endpoint) {
		plain(w, http.StatusServiceUnavailable, "busy")
		return
	}
	defer c.gates.leave(endpoint)
	body, ok := readHookBody(w, r, statusMaxBody)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), hookTxTimeout)
	defer cancel()
	var k keyRow
	found := false
	err := c.hookTx(ctx, func(tx pgx.Tx) error {
		// integration.load_ecpay_key_for_status: the current credential of the ONE connection this URL names (any enabled state).
		err := tx.QueryRow(ctx, `SELECT tenant_id::text,store_id::text,connection_id::text,merchant_id,environment,credential_version,key_id,nonce,ciphertext
			FROM integration.load_ecpay_key_for_status($1::uuid)`, endpoint).Scan(&k.tenantID, &k.storeID, &k.connectionID, &k.merchantID,
			&k.environment, &k.version, &k.keyID, &k.nonce, &k.ciphertext)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	})
	if err != nil {
		plain(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if !found {
		plain(w, http.StatusNotFound, "not found")
		return
	}
	payload, err := c.keys.Open(ecpay.Scope{TenantID: k.tenantID, StoreID: k.storeID, ConnectionID: k.connectionID, MerchantID: k.merchantID,
		Environment: ecpay.Environment(k.environment), Version: k.version}, k.keyID, k.nonce, k.ciphertext)
	if err != nil {
		plain(w, http.StatusServiceUnavailable, "unavailable") // keyring misconfiguration: retryable, never a verification verdict
		return
	}
	// ecpay.ParseStatus: MAC over every received field (unknown fields are covered, then dropped), MerchantID must equal the
	// account, duplicate keys rejected. The recipient fields it saw are not part of StatusReport.
	report, err := ecpay.ParseStatus(body, ecpay.Credentials{MerchantID: k.merchantID, HashKey: payload.HashKey, HashIV: payload.HashIV})
	if err != nil {
		plain(w, http.StatusBadRequest, "0|verify")
		return
	}
	var outcome string
	err = c.hookTx(ctx, func(tx pgx.Tx) error {
		// fulfillment.ingest_ecpay_status: exact §6 code table, dedupe on (attempt, body_sha256), normalises vendor variants.
		return tx.QueryRow(ctx, `SELECT fulfillment.ingest_ecpay_status($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			endpoint, report.BodySHA256[:], report.MerchantID, report.MerchantTradeNo, report.LogisticsID, report.RtnCode,
			report.RtnMsg, report.UpdateDate, report.PaymentNo, report.ValidationNo).Scan(&outcome)
	})
	switch {
	case err != nil:
		plain(w, http.StatusServiceUnavailable, "unavailable") // not committed: no 1|OK, ECPay retries
	case outcome == "retry":
		plain(w, http.StatusServiceUnavailable, "retry") // attempt still REQUESTED
	default:
		plain(w, http.StatusOK, "1|OK") // after COMMIT only (hookTx returned nil)
	}
}
