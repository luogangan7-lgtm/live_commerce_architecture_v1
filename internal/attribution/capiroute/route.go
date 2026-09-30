package capiroute

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"livecommerce/internal/attribution"
	"livecommerce/internal/command"
	"livecommerce/internal/integrations/core"
	metaads "livecommerce/internal/integrations/meta_ads"
	"livecommerce/internal/integrations/meta_ads/tokenopen"
	"livecommerce/internal/platform"
)

// route.go is the one CAPI dispatcher route (contract 6.1 CAPI bullet, C2/C4/C7).
// Cross-domain calls: integration.load_meta_ads_token (dataset token, lease-fenced, commerce_worker) and
// ads.capi_user_data (buyer user data, same lease fence, same transaction) in LoadSecret; ads.check_capi in Check.
// The dispatcher zeroes the core.Secret after DispatchWithSecret returns; this file zeroes every buffer it owns.

// ErrConfig is every constructor failure; it never carries a secret.
var ErrConfig = errors.New("capiroute: invalid configuration")

const (
	actionPurchase = "meta.capi.purchase"
	purposeMarket  = "marketing"
	minKeyBytes    = 32
	maxContents    = 50
)

// requiredScopes mirrors metaads (dataset bindings need ads_management and ads_read, contract 4.1): checked again at
// use time so a token attested without them never reaches Graph.
var requiredScopes = []string{"ads_management", "ads_read"}

var (
	isoPattern    = regexp.MustCompile(`^[A-Z]{3}$`)
	sourcePattern = regexp.MustCompile(`^https://[a-z0-9.-]{1,253}/orders$`)
	testPattern   = regexp.MustCompile(`^[A-Z0-9]{4,20}$`)
	badRequest    = core.Outcome{State: "FAILED_FINAL", Code: "bad_request"}
)

type poster interface {
	PostEvent(ctx context.Context, token []byte, pixelID string, body []byte) (core.Outcome, error)
}

type tokenOpener interface {
	Open(tenantID, storeID, keyID string, enc, ciphertext []byte) ([]byte, error)
}

type route struct {
	post          poster
	keys          tokenOpener
	externalIDKey []byte
	ask           func(ctx context.Context, operationID string) (string, error) // ads.check_capi
}

// Routes returns the single CAPI route. pool must be the commerce_worker pool (platform.ValidateWorkerPool); cfg must
// carry PartnerAgent (F7) and a version; externalIDKey is the worker-only C3 key (>= 32 bytes, copied).
func Routes(pool *pgxpool.Pool, cfg metaads.Config, keys *tokenopen.Keyring, externalIDKey []byte) ([]core.DispatchRoute, error) {
	if pool == nil || keys == nil || len(externalIDKey) < minKeyBytes || cfg.PartnerAgent == "" {
		return nil, ErrConfig
	}
	if err := platform.ValidateWorkerPool(context.Background(), pool); err != nil {
		return nil, err
	}
	client, err := metaads.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	ask := func(ctx context.Context, op string) (string, error) {
		var code string
		// ads.check_capi: PG only, EXECUTE commerce_worker; returns the BLOCKED_POLICY code or ''.
		err := pool.QueryRow(ctx, `SELECT ads.check_capi($1::uuid)`, op).Scan(&code)
		return code, err
	}
	return []core.DispatchRoute{newRoute(client, keys, append([]byte(nil), externalIDKey...), ask).dispatchRoute()}, nil
}

func newRoute(post poster, keys tokenOpener, externalIDKey []byte, ask func(context.Context, string) (string, error)) *route {
	return &route{post: post, keys: keys, externalIDKey: externalIDKey, ask: ask}
}

func (r *route) dispatchRoute() core.DispatchRoute {
	return core.DispatchRoute{
		Provider: metaads.ProviderDataset, Action: actionPurchase, Purpose: purposeMarket,
		Check: r.check, LoadSecret: r.loadSecret, DispatchWithSecret: r.dispatch,
		Reconcile: r.reconcile,
	}
}

// check is PG-only (no network, no secret). Reconcile mode returns nil: a reconcile is query-only and has no effect, and a
// denial there would leave an UNKNOWN operation unreconcilable (ruling X2). A database error is returned as an error, never
// a denial: the dispatcher then records UNKNOWN policy_check_failed, because an unanswered Check must not read as permission.
func (r *route) check(ctx context.Context, req core.DispatchRequest) error {
	if req.Mode == "reconcile" {
		return nil
	}
	if r == nil || r.ask == nil || req.Provider != metaads.ProviderDataset || req.Action != actionPurchase {
		return core.DenyPolicy("unknown_action")
	}
	code, err := r.ask(ctx, req.OperationID)
	if err != nil {
		return err
	}
	if code != "" {
		return core.DenyPolicy(code)
	}
	return nil
}

// reconcile returns UNKNOWN unchanged and calls nothing: Meta has no query API for a server event and no documented
// server-to-server dedup (F15), so the only safe outcome is never to send again (A-7: a lost event is accepted, a
// double-counted sale is not). It needs no secret, so it is a plain Reconcile.
func (r *route) reconcile(_ context.Context, req core.DispatchRequest) (core.Outcome, error) {
	return core.Outcome{State: "UNKNOWN", Code: "capi_unconfirmed", ProviderReference: req.ProviderReference}, nil
}

// tokenRow is one row of integration.load_meta_ads_token.
type tokenRow struct {
	tenant, store, binding, provider, asset, keyID string
	version                                        int64
	nonce, ciphertext                              []byte
	scopes                                         []string
}

// userRow is the one row of ads.capi_user_data.
type userRow struct {
	phone                      *string
	owner                      string
	contents                   []byte
	valueMinor                 int64
	currency, sourceURL, agent string
}

// loadSecret is the dispatcher LoadSecret hook (C2). Both loaders run in the dispatcher's lease-fenced transaction with
// the same claim: the token loader first (its FOR SHARE lock on the operation row is still held for the second call),
// then the user-data definer. Zero rows from either is a policy denial (BLOCKED_POLICY before any Graph call). The packed
// secret holds the token and already-hashed user data; the raw phone and owner id are cleared before return. A loader
// error that is not "no rows" leaves the operation UNKNOWN, and UNKNOWN is never resent (lost-event risk A-7).
func (r *route) loadSecret(ctx context.Context, tx pgx.Tx, claim core.SecretClaim) (core.Secret, error) {
	var tr tokenRow
	// integration.load_meta_ads_token: commerce_worker, lease-fenced for (DISPATCHING,dispatch); the only reader of ads ciphertext.
	err := tx.QueryRow(ctx, `SELECT tenant_id::text,store_id::text,binding_id::text,provider,asset_id,version,key_id,nonce,ciphertext,scopes_attested
		FROM integration.load_meta_ads_token($1::uuid,$2::bigint,$3::bytea)`, claim.OperationID, claim.Generation, claim.LeaseToken).
		Scan(&tr.tenant, &tr.store, &tr.binding, &tr.provider, &tr.asset, &tr.version, &tr.keyID, &tr.nonce, &tr.ciphertext, &tr.scopes)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Secret{}, fmt.Errorf("no dataset token: %w", core.ErrPolicyDenied)
	}
	if err != nil {
		return core.Secret{}, errors.New("capiroute: credential load failed")
	}
	var ur userRow
	// ads.capi_user_data: 0080, same lease fence; the only place buyer user data is read for CAPI (never persisted).
	err = tx.QueryRow(ctx, `SELECT ph_e164,owner_id::text,contents,value_minor,currency,event_source_url,user_agent
		FROM ads.capi_user_data($1::uuid,$2::bigint,$3::bytea)`, claim.OperationID, claim.Generation, claim.LeaseToken).
		Scan(&ur.phone, &ur.owner, &ur.contents, &ur.valueMinor, &ur.currency, &ur.sourceURL, &ur.agent)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Secret{}, fmt.Errorf("no user data: %w", core.ErrPolicyDenied)
	}
	if err != nil {
		return core.Secret{}, errors.New("capiroute: user data load failed")
	}
	return r.assemble(tr, ur)
}

// packed is the core.Secret payload: what DispatchWithSecret needs and nothing raw.
type packed struct {
	Token      []byte    `json:"token"`
	PH         string    `json:"ph,omitempty"`
	ExternalID string    `json:"external_id"`
	Agent      string    `json:"ua"`
	Contents   []content `json:"contents"`
	Value      string    `json:"value"`
	Currency   string    `json:"currency"`
	SourceURL  string    `json:"source_url"`
}

type content struct {
	ID       string `json:"id"`
	Quantity int64  `json:"quantity"`
}

// assemble checks provider and attested scopes, opens the token, hashes ph (F16) and external_id (C3) in memory, validates
// the rest and packs the secret. Split from loadSecret so it is unit-testable without PG.
func (r *route) assemble(tr tokenRow, ur userRow) (core.Secret, error) {
	defer clear(ur.contents)
	if r.keys == nil || tr.provider != metaads.ProviderDataset || !hasScopes(tr.scopes, requiredScopes) {
		return core.Secret{}, fmt.Errorf("dataset token lacks provider or attested scope: %w", core.ErrPolicyDenied)
	}
	value, ok := decimal(ur.valueMinor)
	if !ok || !isoPattern.MatchString(ur.currency) || !sourcePattern.MatchString(ur.sourceURL) || !command.ValidID(ur.owner) ||
		ur.agent == "" || utf8.RuneCountInString(ur.agent) > 512 {
		return core.Secret{}, fmt.Errorf("invalid CAPI user data: %w", core.ErrPolicyDenied)
	}
	var contents []content
	if !decodeStrict(ur.contents, &contents) || len(contents) < 1 || len(contents) > maxContents {
		return core.Secret{}, fmt.Errorf("invalid CAPI contents: %w", core.ErrPolicyDenied)
	}
	for _, c := range contents {
		if !command.ValidID(c.ID) || c.Quantity < 1 || c.Quantity > 1_000_000_000 {
			return core.Secret{}, fmt.Errorf("invalid CAPI contents: %w", core.ErrPolicyDenied)
		}
	}
	p := packed{ExternalID: attribution.ExternalID(r.externalIDKey, tr.tenant, tr.store, ur.owner), Agent: ur.agent,
		Contents: contents, Value: value, Currency: ur.currency, SourceURL: ur.sourceURL}
	if p.ExternalID == "" {
		return core.Secret{}, ErrConfig
	}
	if ur.phone != nil {
		if h, ok := attribution.HashPhone(*ur.phone); ok { // F16: not normalizable -> omit, never guess
			p.PH = h
		}
	}
	token, err := r.keys.Open(tr.tenant, tr.store, tr.keyID, tr.nonce, tr.ciphertext)
	if err != nil || len(token) == 0 {
		return core.Secret{}, errors.New("capiroute: credential unavailable")
	}
	p.Token = token
	raw, err := json.Marshal(p)
	clear(token)
	if err != nil {
		return core.Secret{}, errors.New("capiroute: pack failed")
	}
	defer clear(raw) // core.NewSecret copies; the dispatcher zeroes that copy after the callback
	return core.NewSecret(raw), nil
}

// captureRequest is the frozen op request (C4): internal ids and the event time only.
type captureRequest struct {
	V             int    `json:"v"`
	AttemptID     string `json:"attempt_id"`
	EventID       string `json:"event_id"`
	EventTime     int64  `json:"event_time"`
	TestEventCode string `json:"test_event_code,omitempty"`
}

// dispatch is DispatchWithSecret: exactly one POST through Client.PostEvent, pixel = the operation's ExternalAssetID. Any
// invalid frozen request or packed secret ends FAILED_FINAL bad_request BEFORE any Graph call. The outcome of PostEvent is
// returned as is: 2xx with events_received==1 SUCCEEDED, 4xx Graph error FAILED_FINAL, everything else UNKNOWN, and an
// UNKNOWN is never sent again (Reconcile above) because a resend could double-count the purchase (F15, A-7).
func (r *route) dispatch(ctx context.Context, req core.DispatchRequest, secret core.Secret) (core.Outcome, error) {
	raw := secret.Reveal()
	var p packed
	ok := decodeStrict(raw, &p)
	defer clear(p.Token)
	var cr captureRequest
	if !ok || !decodeStrict(req.Request, &cr) || cr.V != 1 || !command.ValidID(cr.AttemptID) || cr.EventID != attribution.EventID(cr.AttemptID) ||
		cr.EventTime < 1 || (cr.TestEventCode != "" && !testPattern.MatchString(cr.TestEventCode)) || len(p.Token) == 0 {
		return badRequest, nil
	}
	body, err := buildBody(cr, p)
	if err != nil {
		return badRequest, nil
	}
	return r.post.PostEvent(ctx, p.Token, req.ExternalAssetID, body)
}

// Wire shapes of the Conversions API server event (F14):
// https://developers.facebook.com/docs/marketing-api/conversions-api/parameters/server-event and
// .../customer-information-parameters (F16), Purchase custom_data (currency, value, content_type, contents), all as cited by
// contract F14/F16 (retrieved 2026-09-29); not re-fetched in this unit (no network).
type serverEvent struct {
	EventName      string     `json:"event_name"`
	EventTime      int64      `json:"event_time"`
	EventID        string     `json:"event_id"`
	EventSourceURL string     `json:"event_source_url"`
	ActionSource   string     `json:"action_source"`
	UserData       userData   `json:"user_data"`
	CustomData     customData `json:"custom_data"`
}

type userData struct {
	ExternalID []string `json:"external_id"`
	PH         []string `json:"ph,omitempty"`
	UserAgent  string   `json:"client_user_agent"`
}

type customData struct {
	Currency    string      `json:"currency"`
	Value       json.Number `json:"value"`
	ContentType string      `json:"content_type"`
	Contents    []content   `json:"contents"`
}

type eventBody struct {
	Data          []serverEvent `json:"data"`
	TestEventCode string        `json:"test_event_code,omitempty"`
}

// buildBody is the exact CAPI request body without the token and partner_agent (PostEvent adds those). action_source is
// "website" (C4); value is the exact decimal of amount_minor/100 as a JSON number (I05), never a float.
func buildBody(cr captureRequest, p packed) ([]byte, error) {
	ud := userData{ExternalID: []string{p.ExternalID}, UserAgent: p.Agent}
	if p.PH != "" {
		ud.PH = []string{p.PH}
	}
	return json.Marshal(eventBody{
		Data: []serverEvent{{EventName: "Purchase", EventTime: cr.EventTime, EventID: cr.EventID, EventSourceURL: p.SourceURL,
			ActionSource: "website", UserData: ud,
			CustomData: customData{Currency: p.Currency, Value: json.Number(p.Value), ContentType: "product", Contents: p.Contents}}},
		TestEventCode: cr.TestEventCode,
	})
}

// decimal formats minor units as an exact two-fraction decimal (I05: amount_minor/100; facts are TWD/USD/HKD with a
// 100 minor unit, checked by the fact CHECKs). Negative or zero amounts are refused.
func decimal(minor int64) (string, bool) {
	if minor < 1 {
		return "", false
	}
	return fmt.Sprintf("%d.%02d", minor/100, minor%100), true
}

func hasScopes(have, need []string) bool {
	for _, n := range need {
		found := false
		for _, h := range have {
			found = found || h == n
		}
		if !found {
			return false
		}
	}
	return true
}

func decodeStrict(raw []byte, v any) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(v) != nil {
		return false
	}
	var extra json.RawMessage
	return dec.Decode(&extra) == io.EOF
}
