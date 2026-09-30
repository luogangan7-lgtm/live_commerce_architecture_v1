package ads

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"livecommerce/internal/command"
	"livecommerce/internal/integrations/core"
	"livecommerce/internal/platform"
)

// service.go is the merchant-facing ads service called by internal/httpapi/ads.go. Every method runs inside the caller's
// platform.WithScope transaction (commerce_runtime, READ COMMITTED) except Callback, which needs two transactions around
// its network step. All persistence goes through the SECURITY DEFINER functions of migrations/0074 and 0075 (ads.*,
// integration.register_meta_ads_token): the definers re-authenticate the session hash and decide every rule; this file
// only shapes input, mints ids, inserts the ads-lane River job in the same transaction, and writes the audit row.
// Route -> SQL: see each method. Non-goals: no Graph call (ConnectFunc is injected, called with no transaction open),
// no token plaintext (only the sealed copy reaches SQL), no rule the SQL owns.

// Service is the merchant ads service. jobs is used only for transactional River inserts (queue ads).
type Service struct {
	jobs    *river.Client[pgx.Tx]
	core    *core.Service
	connect ConnectFunc
	dialog  DialogConfig
}

var (
	digitsPattern  = regexp.MustCompile(`^[0-9]{1,40}$`)
	versionPattern = regexp.MustCompile(`^v[0-9]{1,3}\.[0-9]{1,2}$`)
	statePattern   = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	codePattern    = regexp.MustCompile(`^[\x21-\x7e]{1,1000}$`)
	datePattern    = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	testCodeShape  = regexp.MustCompile(`^[A-Z0-9]{4,20}$`)
)

// NewService validates the dialog configuration and the River client (insert-only, Schema "river"). connect may be nil in
// tests; a nil connect makes Callback fail with ErrConnectFailed instead of panicking.
func NewService(jobs *river.Client[pgx.Tx], connect ConnectFunc, dialog DialogConfig) (*Service, error) {
	inner, err := core.New(jobs)
	if err != nil {
		return nil, err
	}
	u, perr := url.Parse(dialog.RedirectURI)
	if perr != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || len(dialog.RedirectURI) > 512 ||
		!digitsPattern.MatchString(dialog.AppID) || !digitsPattern.MatchString(dialog.ConfigID) ||
		!versionPattern.MatchString(dialog.GraphVersion) || len(dialog.StateKey) < 32 {
		return nil, command.ErrInvalid
	}
	dialog.StateKey = append([]byte(nil), dialog.StateKey...) // the caller may clear its copy
	return &Service{jobs: jobs, core: inner, connect: connect, dialog: dialog}, nil
}

// tokenHash is the session hash the definers authenticate (the same SHA-256 platform.WithScope used).
func tokenHash(token string) ([]byte, error) {
	if len(token) < 32 || len(token) > 512 {
		return nil, platform.ErrUnauthorized
	}
	h := sha256.Sum256([]byte(token))
	return h[:], nil
}

// newID mints a UUID with the database (no new dependency, one round trip).
func newID(ctx context.Context, tx pgx.Tx) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&id)
	return id, err
}

// ---------------------------------------------------------------------------------------------------------------------
// Connect (contract 2 steps 1-3)
// ---------------------------------------------------------------------------------------------------------------------

// ConnectStart is the 201 body of POST meta/connect.
type ConnectStart struct {
	StateID   string    `json:"state_id"`
	DialogURL string    `json:"dialog_url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// connectReceipt is what command.Run saves for a replay: no state secret, so the receipt table never holds an OAuth state.
type connectReceipt struct {
	StateID   string    `json:"state_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// stateParam derives the OAuth state deterministically from the caller's scope and Idempotency-Key, so a replay rebuilds the
// same dialog URL without the receipt storing it. It is an HMAC-SHA256 under the server-side key (r3 review P2): the
// Idempotency-Key sits in clear in ops.command_results, so an unkeyed hash of it would be recomputable by a reader of that
// table. It still matters only together with the server-side binding to principal + store (a stolen state fails as
// state_mismatch in another session); only its SHA-256 is stored (ads.begin_connect).
func stateParam(key []byte, scope platform.Scope, idemKey string) (param string, hash []byte) {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("livecommerce/ads-oauth-state/v2|" + scope.TenantID + "|" + scope.StoreID + "|" + scope.PrincipalID + "|" + idemKey))
	param = base64.RawURLEncoding.EncodeToString(m.Sum(nil))
	digest := sha256.Sum256([]byte(param))
	return param, digest[:]
}

// stateHash is the digest stored for a state parameter (SHA-256 of its text).
func stateHash(param string) []byte {
	d := sha256.Sum256([]byte(param))
	return d[:]
}

// Connect starts a connect: stores the hashed state (ads.begin_connect: ads:manage + integration:manage, 10 min, single
// use) and returns the FLfB dialog URL. Idempotent per Idempotency-Key.
func (s *Service) Connect(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key string) (out ConnectStart, err error) {
	hash, err := tokenHash(token)
	if err != nil || s == nil {
		return out, platform.ErrUnauthorized
	}
	param, digest := stateParam(s.dialog.StateKey, scope, key)
	var receipt connectReceipt
	err = command.Run(ctx, tx, scope, "ads.meta.connect", key, struct {
		PrincipalID string `json:"principal_id"`
	}{scope.PrincipalID}, &receipt, func() error {
		// ads.begin_connect: stores sha256(state) for this principal and store; at most 10 live states per store.
		if e := tx.QueryRow(ctx, `SELECT out_state::text,out_expires FROM ads.begin_connect($1,$2,$3)`, hash, scope.StoreID, digest).
			Scan(&receipt.StateID, &receipt.ExpiresAt); e != nil {
			return e
		}
		return command.Audit(ctx, tx, scope, "ads.meta.connect_started")
	})
	if err != nil {
		return out, mapError(err)
	}
	return ConnectStart{StateID: receipt.StateID, DialogURL: s.dialogURL(param), ExpiresAt: receipt.ExpiresAt}, nil
}

// dialogURL builds the Facebook Login for Business dialog URL: config_id replaces scope, response_type=code with
// override_default_response_type=true yields a server-exchangeable code and a BISU token (F3).
// Source: https://developers.facebook.com/documentation/facebook-login/facebook-login-for-business (retrieved 2026-09-30).
func (s *Service) dialogURL(state string) string {
	q := url.Values{}
	q.Set("client_id", s.dialog.AppID)
	q.Set("config_id", s.dialog.ConfigID)
	q.Set("response_type", "code")
	q.Set("override_default_response_type", "true")
	q.Set("redirect_uri", s.dialog.RedirectURI)
	q.Set("state", state)
	u := url.URL{Scheme: "https", Host: "www.facebook.com", Path: "/" + s.dialog.GraphVersion + "/dialog/oauth", RawQuery: q.Encode()}
	return u.String()
}

// Callback consumes the state, exchanges the code with no transaction open, and stores the sealed result. It returns the
// state id. Retry rule: the state is single-use, so a failed exchange burns it and the merchant starts a new connect; the
// exchange is never retried here (a code is single-use at Meta too).
func (s *Service) Callback(ctx context.Context, pool *pgxpool.Pool, token, storeID, code, state string) (string, error) {
	if s == nil || pool == nil {
		return "", platform.ErrUnauthorized
	}
	hash, err := tokenHash(token)
	if err != nil {
		return "", err
	}
	if !statePattern.MatchString(state) {
		return "", refusal("state_mismatch")
	}
	if !codePattern.MatchString(code) {
		return "", refusal("invalid_request")
	}
	var stateID, tenant string
	err = platform.WithScope(ctx, pool, token, storeID, "ads:manage", func(tx pgx.Tx, scope platform.Scope) error {
		tenant = scope.TenantID
		// ads.consume_state: ads:manage + integration:manage, principal + store must match the state (state_mismatch),
		// unexpired (state_expired); marks it used.
		return tx.QueryRow(ctx, `SELECT ads.consume_state($1,$2,$3)::text`, hash, storeID, stateHash(state)).Scan(&stateID)
	})
	if err != nil {
		return "", mapError(err)
	}
	if s.connect == nil {
		return "", ErrConnectFailed
	}
	res, err := s.connect(ctx, code, SealInfo{TenantID: tenant, StoreID: storeID})
	if err != nil || !validConnectResult(res) {
		return "", ErrConnectFailed // no cause is returned or logged: it could echo the code
	}
	picks, err := json.Marshal(res.Picks)
	if err != nil {
		return "", ErrConnectFailed
	}
	err = platform.WithScope(ctx, pool, token, storeID, "ads:manage", func(tx pgx.Tx, scope platform.Scope) error {
		// ads.put_connect_result: stores client business, pick list, scopes and the sealed token on the consumed state.
		if _, e := tx.Exec(ctx, `SELECT ads.put_connect_result($1,$2,$3,$4,$5::jsonb,$6,$7,$8,$9)`, hash, storeID, stateID,
			res.ClientBusinessID, string(picks), res.Scopes, res.Token.KeyID, res.Token.Enc, res.Token.Ciphertext); e != nil {
			return e
		}
		return command.Audit(ctx, tx, scope, "ads.meta.connected")
	})
	if err != nil {
		return "", mapError(err)
	}
	return stateID, nil
}

var scopeShape = regexp.MustCompile(`^[a-z_]{1,64}$`)

// validConnectResult rejects a ConnectResult the SQL would refuse anyway, before the sealed bytes travel: shapes only.
func validConnectResult(r ConnectResult) bool {
	if !digitsPattern.MatchString(r.ClientBusinessID) || len(r.Scopes) < 1 || len(r.Scopes) > 16 || len(r.Picks) > 200 ||
		len(r.Token.Enc) != 32 || len(r.Token.Ciphertext) < 17 || len(r.Token.Ciphertext) > 8192 || r.Token.KeyID == "" {
		return false
	}
	for _, sc := range r.Scopes {
		if !scopeShape.MatchString(sc) {
			return false
		}
	}
	for _, p := range r.Picks {
		if (p.Kind != "ad_account" && p.Kind != "dataset") || !digitsPattern.MatchString(p.ID) {
			return false
		}
	}
	return true
}

// GetState returns the pick list of a state of this principal and store (D5): ads.get_state.
func (s *Service) GetState(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, stateID string) (json.RawMessage, error) {
	hash, err := tokenHash(token)
	if err != nil || !command.ValidID(stateID) {
		return nil, mapError(orInvalid(err))
	}
	return queryJSON(ctx, tx, `SELECT ads.get_state($1,$2,$3)`, hash, scope.StoreID, stateID)
}

// BindInput is the POST meta/bindings body.
type BindInput struct {
	StateID     string `json:"state_id"`
	AdAccountID string `json:"ad_account_id"`
	DatasetID   string `json:"dataset_id,omitempty"`
}

// BindResult is the 201 body.
type BindResult struct {
	AdBindingID      string `json:"ad_binding_id"`
	DatasetBindingID string `json:"dataset_binding_id,omitempty"`
}

// Bind registers (or re-uses) the meta_ads and optional meta_dataset bindings for the picked assets and copies the sealed
// token to each binding server-side (integration.register_meta_ads_token). A re-connect re-uses the store's enabled binding
// for the same asset and CASes its token version (ads.connection_version); a different client business is refused by SQL
// (client_business_changed). Idempotent per Idempotency-Key.
func (s *Service) Bind(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key string, in BindInput) (out BindResult, err error) {
	hash, err := tokenHash(token)
	if err != nil || s == nil {
		return out, platform.ErrUnauthorized
	}
	if !command.ValidID(in.StateID) || !digitsPattern.MatchString(in.AdAccountID) || (in.DatasetID != "" && !digitsPattern.MatchString(in.DatasetID)) {
		return out, refusal("invalid_request")
	}
	err = command.Run(ctx, tx, scope, "ads.meta.bind", key, struct {
		PrincipalID string    `json:"principal_id"`
		Input       BindInput `json:"input"`
	}{scope.PrincipalID, in}, &out, func() error {
		id, e := s.bindOne(ctx, tx, scope, token, hash, key, in.StateID, "meta_ads", in.AdAccountID)
		if e != nil {
			return e
		}
		out.AdBindingID = id
		if in.DatasetID != "" {
			if id, e = s.bindOne(ctx, tx, scope, token, hash, key, in.StateID, "meta_dataset", in.DatasetID); e != nil {
				return e
			}
			out.DatasetBindingID = id
		}
		// ads.finish_bind: deletes the pending sealed token now that every picked asset has its copy.
		if _, e = tx.Exec(ctx, `SELECT ads.finish_bind($1,$2,$3)`, hash, scope.StoreID, in.StateID); e != nil {
			return e
		}
		return command.Audit(ctx, tx, scope, "ads.meta.bound")
	})
	return out, mapError(err)
}

func (s *Service) bindOne(ctx context.Context, tx pgx.Tx, scope platform.Scope, token string, hash []byte, key, stateID, provider, asset string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT id::text FROM integration.bindings
		WHERE tenant_id=$1 AND store_id=$2 AND provider=$3 AND external_asset_id=$4 AND enabled ORDER BY created_at,id LIMIT 1`,
		scope.TenantID, scope.StoreID, provider, asset).Scan(&id)
	if err == pgx.ErrNoRows {
		// core.RegisterBinding: integration:manage, inserts the binding under the runtime's own policy; its command key is
		// derived so the two bindings of one connect never collide.
		sum := sha256.Sum256([]byte(key + "|" + provider))
		b, e := s.core.RegisterBinding(ctx, tx, scope, token, "adsb-"+hex.EncodeToString(sum[:16]), provider, asset)
		if e != nil {
			return "", e
		}
		id = b.ID
	} else if err != nil {
		return "", err
	}
	var version int64
	// ads.connection_version: the binding's current token version (0 = never connected) for the registrar CAS.
	if err = tx.QueryRow(ctx, `SELECT ads.connection_version($1,$2,$3)`, hash, scope.StoreID, id).Scan(&version); err != nil {
		return "", err
	}
	// integration.register_meta_ads_token: hash-authenticated; asset must be in the state's pick list; copies the sealed token.
	if _, err = tx.Exec(ctx, `SELECT integration.register_meta_ads_token($1,$2,$3,$4,$5)`, hash, scope.StoreID, stateID, id, version); err != nil {
		return "", err
	}
	if err = command.Audit(ctx, tx, scope, "ads.meta.token_registered"); err != nil {
		return "", err
	}
	return id, nil
}

// ---------------------------------------------------------------------------------------------------------------------
// Settings, drafts, report, CAPI
// ---------------------------------------------------------------------------------------------------------------------

// GetSettings returns the store's ads settings JSON (D5): ads.get_settings.
func (s *Service) GetSettings(ctx context.Context, tx pgx.Tx, scope platform.Scope, token string) (json.RawMessage, error) {
	hash, err := tokenHash(token)
	if err != nil {
		return nil, err
	}
	return queryJSON(ctx, tx, `SELECT ads.get_settings($1,$2)`, hash, scope.StoreID)
}

// wire is the DraftInput as SQL parses it: RFC 3339 UTC strings, sorted countries.
func (in DraftInput) wire() ([]byte, error) {
	return json.Marshal(map[string]any{
		"ad_binding_id": in.AdBindingID, "identity_binding_id": in.IdentityBindingID, "template": in.Template,
		"source_ref": in.SourceRef, "currency": in.Currency, "lifetime_budget_minor": in.LifetimeBudgetMinor,
		"starts_at": in.StartsAt.UTC().Format(time.RFC3339Nano), "ends_at": in.EndsAt.UTC().Format(time.RFC3339Nano),
		"countries": sortedCountries(in.Countries), "age_min": in.AgeMin, "age_max": in.AgeMax,
	})
}

// CreateDraft validates locally, then ads.create_draft (ads:manage). Idempotent per Idempotency-Key.
func (s *Service) CreateDraft(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key string, in DraftInput) (json.RawMessage, error) {
	hash, err := tokenHash(token)
	if err != nil {
		return nil, err
	}
	if err = ValidateInput(in, "", time.Now()); err != nil {
		return nil, err
	}
	wire, err := in.wire()
	if err != nil {
		return nil, command.ErrInvalid
	}
	var out json.RawMessage
	err = command.Run(ctx, tx, scope, "ads.draft.create", key, struct {
		PrincipalID string          `json:"principal_id"`
		Input       json.RawMessage `json:"input"`
	}{scope.PrincipalID, wire}, &out, func() error {
		if e := tx.QueryRow(ctx, `SELECT ads.create_draft($1,$2,$3::jsonb)`, hash, scope.StoreID, string(wire)).Scan(&out); e != nil {
			return e
		}
		return command.Audit(ctx, tx, scope, "ads.draft.created")
	})
	return out, mapError(err)
}

// UpdateDraft is PUT drafts/{id} with If-Match revision: ads.update_draft (ads:manage). Refused for an approved draft.
func (s *Service) UpdateDraft(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key, draftID string, revision int, in DraftInput) (json.RawMessage, error) {
	hash, err := tokenHash(token)
	if err != nil {
		return nil, err
	}
	if !command.ValidID(draftID) || revision < 1 || revision > 1000 {
		return nil, refusal("invalid_request")
	}
	if err = ValidateInput(in, "", time.Now()); err != nil {
		return nil, err
	}
	wire, err := in.wire()
	if err != nil {
		return nil, command.ErrInvalid
	}
	var out json.RawMessage
	err = command.Run(ctx, tx, scope, "ads.draft.update", key, struct {
		PrincipalID string          `json:"principal_id"`
		DraftID     string          `json:"draft_id"`
		Revision    int             `json:"revision"`
		Input       json.RawMessage `json:"input"`
	}{scope.PrincipalID, draftID, revision, wire}, &out, func() error {
		if e := tx.QueryRow(ctx, `SELECT ads.update_draft($1,$2,$3,$4,$5::jsonb)`, hash, scope.StoreID, draftID, revision, string(wire)).Scan(&out); e != nil {
			return e
		}
		return command.Audit(ctx, tx, scope, "ads.draft.updated")
	})
	return out, mapError(err)
}

// ListDrafts and GetDraft are the ads:read projections (ads.list_drafts / ads.get_draft).
func (s *Service) ListDrafts(ctx context.Context, tx pgx.Tx, scope platform.Scope, token string) (json.RawMessage, error) {
	hash, err := tokenHash(token)
	if err != nil {
		return nil, err
	}
	return queryJSON(ctx, tx, `SELECT ads.list_drafts($1,$2)`, hash, scope.StoreID)
}

func (s *Service) GetDraft(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, draftID string) (json.RawMessage, error) {
	hash, err := tokenHash(token)
	if err != nil || !command.ValidID(draftID) {
		return nil, mapError(orInvalid(err))
	}
	return queryJSON(ctx, tx, `SELECT ads.get_draft($1,$2,$3)`, hash, scope.StoreID, draftID)
}

// Approve is ads.approve_draft (ads:approve): revision CAS, billing, allowance, then the approval hash. Idempotent per revision.
func (s *Service) Approve(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, draftID string, revision int) (json.RawMessage, error) {
	hash, err := tokenHash(token)
	if err != nil {
		return nil, err
	}
	if !command.ValidID(draftID) || revision < 1 || revision > 1000 {
		return nil, refusal("invalid_request")
	}
	out, err := queryJSON(ctx, tx, `SELECT ads.approve_draft($1,$2,$3,$4)`, hash, scope.StoreID, draftID, revision)
	if err != nil {
		return nil, err
	}
	if err = command.Audit(ctx, tx, scope, "ads.draft.approved"); err != nil {
		return nil, err
	}
	return out, nil
}

// adsLane inserts the ads-lane River job for a freshly minted operation id in the caller's transaction and returns both.
// A rollback of the caller's transaction removes the job, so no retry decision exists here. Pause uses priority 1.
func (s *Service) adsLane(ctx context.Context, tx pgx.Tx, priority int) (opID string, jobID int64, err error) {
	if opID, err = newID(ctx, tx); err != nil {
		return "", 0, err
	}
	// core.InsertOperationJobOn: queue ads (cmd/claims-worker never claims it); post_river/0015 guards the lane.
	jobID, err = core.InsertOperationJobOn(ctx, s.jobs, tx, opID, "ads", priority)
	return opID, jobID, err
}

// Publish is POST drafts/{id}/publish: inserts the create_campaign job, then ads.publish_draft (ads:approve) CASes the body's
// publish_attempt (attempt_changed), checks billing and earlier attempts, and plans create_campaign.
func (s *Service) Publish(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key, draftID string, attempt int) (json.RawMessage, error) {
	hash, err := tokenHash(token)
	if err != nil || s == nil {
		return nil, platform.ErrUnauthorized
	}
	if !command.ValidID(draftID) || attempt < 0 || attempt > 5 {
		return nil, refusal("invalid_request")
	}
	var out json.RawMessage
	err = command.Run(ctx, tx, scope, "ads.draft.publish", key, struct {
		PrincipalID string `json:"principal_id"`
		DraftID     string `json:"draft_id"`
		Attempt     int    `json:"attempt"`
	}{scope.PrincipalID, draftID, attempt}, &out, func() error {
		op, job, e := s.adsLane(ctx, tx, 3)
		if e != nil {
			return e
		}
		if e = tx.QueryRow(ctx, `SELECT ads.publish_draft($1,$2,$3,$4,$5,$6)`, hash, scope.StoreID, draftID, attempt, op, job).Scan(&out); e != nil {
			return e
		}
		return command.Audit(ctx, tx, scope, "ads.draft.published")
	})
	return out, mapError(err)
}

// Pause (ads:manage) and End (ads:approve) plan a priority-1 pause op when one is needed (ads.pause_prepare decides under the
// draft lock); pause is never blocked by billing, allowance or approval. End also marks the draft ended (nothing more is planned).
func (s *Service) Pause(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key, draftID string) (json.RawMessage, error) {
	return s.pause(ctx, tx, scope, token, key, draftID, false)
}

func (s *Service) End(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key, draftID string) (json.RawMessage, error) {
	return s.pause(ctx, tx, scope, token, key, draftID, true)
}

func (s *Service) pause(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key, draftID string, end bool) (json.RawMessage, error) {
	hash, err := tokenHash(token)
	if err != nil || s == nil {
		return nil, platform.ErrUnauthorized
	}
	if !command.ValidID(draftID) {
		return nil, refusal("invalid_request")
	}
	operation, audit := "ads.draft.pause", "ads.draft.paused"
	if end {
		operation, audit = "ads.draft.end", "ads.draft.ended"
	}
	var out json.RawMessage
	err = command.Run(ctx, tx, scope, operation, key, struct {
		PrincipalID string `json:"principal_id"`
		DraftID     string `json:"draft_id"`
	}{scope.PrincipalID, draftID}, &out, func() error {
		var plan bool
		if e := tx.QueryRow(ctx, `SELECT ads.pause_prepare($1,$2,$3,$4)`, hash, scope.StoreID, draftID, end).Scan(&plan); e != nil {
			return e
		}
		if !plan {
			// Nothing to plan (never published, pause already in flight, or the campaign is not pinned yet): return the draft.
			if e := tx.QueryRow(ctx, `SELECT ads.pause_view($1,$2,$3,$4)`, hash, scope.StoreID, draftID, end).Scan(&out); e != nil {
				return e
			}
			return command.Audit(ctx, tx, scope, audit)
		}
		op, job, e := s.adsLane(ctx, tx, 1)
		if e != nil {
			return e
		}
		if e = tx.QueryRow(ctx, `SELECT ads.pause_plan($1,$2,$3,$4,$5,$6)`, hash, scope.StoreID, draftID, end, op, job).Scan(&out); e != nil {
			return e
		}
		return command.Audit(ctx, tx, scope, audit)
	})
	return out, mapError(err)
}

// Report is GET report (ads:read): ads.report, three separate blocks.
func (s *Service) Report(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, from, to string) (json.RawMessage, error) {
	hash, err := tokenHash(token)
	if err != nil {
		return nil, err
	}
	if !datePattern.MatchString(from) || !datePattern.MatchString(to) {
		return nil, refusal("invalid_request")
	}
	f, e1 := time.Parse("2006-01-02", from)
	t, e2 := time.Parse("2006-01-02", to)
	if e1 != nil || e2 != nil || t.Before(f) || t.Sub(f) > 91*24*time.Hour {
		return nil, refusal("invalid_request")
	}
	return queryJSON(ctx, tx, `SELECT ads.report($1,$2,$3::date,$4::date)`, hash, scope.StoreID, from, to)
}

// CapiInput is the PUT capi body; dataset_binding_id and test_event_code are optional (absent = none).
type CapiInput struct {
	Enabled          bool   `json:"enabled"`
	DatasetBindingID string `json:"dataset_binding_id,omitempty"`
	TestEventCode    string `json:"test_event_code,omitempty"`
}

// SetCapi is ads.set_capi (ads:manage): the merchant's only write to ads.store_settings. Idempotent per Idempotency-Key.
func (s *Service) SetCapi(ctx context.Context, tx pgx.Tx, scope platform.Scope, token, key string, in CapiInput) (json.RawMessage, error) {
	hash, err := tokenHash(token)
	if err != nil {
		return nil, err
	}
	if (in.DatasetBindingID != "" && !command.ValidID(in.DatasetBindingID)) || (in.TestEventCode != "" && !testCodeShape.MatchString(in.TestEventCode)) {
		return nil, refusal("invalid_request")
	}
	var dataset, code *string
	if in.DatasetBindingID != "" {
		dataset = &in.DatasetBindingID
	}
	if in.TestEventCode != "" {
		code = &in.TestEventCode
	}
	var out json.RawMessage
	err = command.Run(ctx, tx, scope, "ads.capi.set", key, struct {
		PrincipalID string    `json:"principal_id"`
		Input       CapiInput `json:"input"`
	}{scope.PrincipalID, in}, &out, func() error {
		if e := tx.QueryRow(ctx, `SELECT ads.set_capi($1,$2,$3,$4::uuid,$5)`, hash, scope.StoreID, in.Enabled, dataset, code).Scan(&out); e != nil {
			return e
		}
		return command.Audit(ctx, tx, scope, "ads.capi.changed")
	})
	return out, mapError(err)
}

// queryJSON runs a definer returning jsonb and maps its refusals.
func queryJSON(ctx context.Context, tx pgx.Tx, sql string, args ...any) (json.RawMessage, error) {
	var out json.RawMessage
	if err := tx.QueryRow(ctx, sql, args...).Scan(&out); err != nil {
		return nil, mapError(err)
	}
	return out, nil
}

func orInvalid(err error) error {
	if err != nil {
		return err
	}
	return refusal("invalid_request")
}
