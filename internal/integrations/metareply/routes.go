package metareply

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"livecommerce/internal/claims"
	"livecommerce/internal/command"
	"livecommerce/internal/integrations/core"
	"livecommerce/internal/platform"
)

// Graph constants (meta-claims-intake-v1 §0 F1/F2, retrieved 2026-09-28):
//   - Facebook Page private reply: POST /{PAGE-ID}/messages, recipient.comment_id, needs
//     pages_messaging: https://developers.facebook.com/docs/messenger-platform/discovery/private-replies/
//   - Instagram private reply: POST /<IG_ID>/messages on graph.facebook.com (Facebook Login path),
//     needs instagram_manage_comments + pages_read_engagement:
//     https://developers.facebook.com/docs/instagram-platform/private-replies/
//
// The API version is required configuration (U5, no default) and the token travels in the JSON
// body unless AuthorizationHeader is set (U6 default false), never in the URL.
const (
	graphHost       = "https://graph.facebook.com"
	maxResponseBody = 64 << 10
	templateID      = "claim-link/v1"
	messageType     = "first_private_reply"

	codeSent        = "graph_sent"
	codeUnconfirmed = "graph_unconfirmed"
	codeUnproven    = "reconcile_unproven"
)

var (
	versionPattern  = regexp.MustCompile(`^v[0-9]{1,3}\.[0-9]{1,2}$`)
	loopbackPattern = regexp.MustCompile(`^http://127\.0\.0\.1:[0-9]{1,5}$`)

	// checkDenyCodes are the §6.3 fixed deny codes of claims.check_meta_reply.
	checkDenyCodes = map[string]bool{"deadline": true, "source_off": true, "principal_revoked": true, "link_invalid": true, "live_closed": true}

	// requiredScopes are the attested Page-token scopes per provider (§7; U2 open for Instagram).
	requiredScopes = map[string][]string{
		"facebook":  {"pages_messaging"},
		"instagram": {"instagram_manage_comments", "pages_read_engagement"},
	}
)

// Config is the Graph adapter configuration. GraphBaseURL is exactly https://graph.facebook.com or
// a loopback http://127.0.0.1:<port> (MOCK); GraphVersion is required (U5). HTTPClient is optional;
// redirects are never followed (a 307 would replay the POST body with the token).
type Config struct {
	GraphBaseURL        string
	GraphVersion        string
	AuthorizationHeader bool
	HTTPClient          *http.Client
}

// Validate is the configuration part of Routes' checks (no I/O), so a command can refuse a bad
// base URL or API version before opening any connection.
func (c Config) Validate() error {
	if !versionPattern.MatchString(c.GraphVersion) || !(c.GraphBaseURL == graphHost || loopbackPattern.MatchString(c.GraphBaseURL)) {
		return ErrConfig
	}
	return nil
}

// GraphHost is the only non-loopback base URL Config accepts.
const GraphHost = graphHost

// checkFunc runs claims.check_meta_reply; separated so unit tests need no database.
type checkFunc func(ctx context.Context, operationID string, linkHash []byte) (string, error)

// Routes returns the two dispatcher routes (facebook and instagram, meta.private_reply, service).
// checkPool must be the commerce_worker pool (platform.ValidateWorkerPool) and is used for one
// STABLE statement per Check; no transaction is held across I/O.
func Routes(checkPool *pgxpool.Pool, linkKey claims.ReplyLinkKey, pageKeys *PageTokenKeyring, cfg Config) ([]core.DispatchRoute, error) {
	if checkPool == nil {
		return nil, ErrConfig
	}
	if err := platform.ValidateWorkerPool(context.Background(), checkPool); err != nil {
		return nil, err
	}
	check := func(ctx context.Context, operationID string, linkHash []byte) (code string, err error) {
		// claims.check_meta_reply: definer commerce_claims_writer, lock-free read of the frozen operation.
		err = checkPool.QueryRow(ctx, `SELECT claims.check_meta_reply($1::uuid,$2::bytea)`, operationID, linkHash).Scan(&code)
		return code, err
	}
	return newRoutes(check, linkKey, pageKeys, cfg)
}

func newRoutes(check checkFunc, linkKey claims.ReplyLinkKey, pageKeys *PageTokenKeyring, cfg Config) ([]core.DispatchRoute, error) {
	if check == nil || linkKey.ID() == "" || pageKeys == nil || cfg.Validate() != nil {
		return nil, ErrConfig
	}
	client := &http.Client{}
	if cfg.HTTPClient != nil {
		copied := *cfg.HTTPClient
		client = &copied
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	a := &adapter{check: check, linkKey: linkKey, keys: pageKeys, cfg: cfg, client: client}
	routes := make([]core.DispatchRoute, 0, 2)
	for _, provider := range []string{"facebook", "instagram"} {
		routes = append(routes, core.DispatchRoute{
			Provider: provider, Action: "meta.private_reply", Purpose: "service",
			Check:              a.checkRoute,
			LoadSecret:         a.loadSecretFor(provider),
			DispatchWithSecret: a.dispatch,
			Reconcile:          a.reconcile,
		})
	}
	return routes, nil
}

type adapter struct {
	check   checkFunc
	linkKey claims.ReplyLinkKey
	keys    *PageTokenKeyring
	cfg     Config
	client  *http.Client
}

// replyRequest is the part of the frozen operation request (§6.2, no token/text/name) the adapter reads.
type replyRequest struct {
	V           int    `json:"v"`
	AssetID     string `json:"asset_id"`
	CommentRef  string `json:"comment_ref"`
	BundleID    string `json:"bundle_id"`
	LinkKeyID   string `json:"link_key_id"`
	Locale      string `json:"locale"`
	Template    string `json:"template"`
	MessageType string `json:"message_type"`
	Origin      string `json:"origin"`
}

var errBadRequest = errors.New("metareply: malformed operation request")

func parseRequest(req core.DispatchRequest) (r replyRequest, err error) {
	if err = json.Unmarshal(req.Request, &r); err != nil || r.V != 1 || r.Template != templateID || r.MessageType != messageType ||
		!command.ValidID(r.BundleID) || r.AssetID != req.ExternalAssetID {
		return replyRequest{}, errBadRequest
	}
	return r, nil
}

// linkToken re-derives the bundle's claim token; it exists only in memory for one call.
func (a *adapter) linkToken(req core.DispatchRequest, r replyRequest) (claims.LinkToken, error) {
	return claims.SystemLinkToken(a.linkKey, req.TenantID, req.StoreID, r.BundleID, req.OperationID)
}

// checkRoute is Check (every attempt): only a returned deny code is a policy denial (zero HTTP
// calls, BLOCKED_POLICY); any infrastructure error is a plain error, which the dispatcher records
// as UNKNOWN policy_check_failed (the reply is not sent; documented limit, §14).
func (a *adapter) checkRoute(ctx context.Context, req core.DispatchRequest) error {
	r, err := parseRequest(req)
	if err != nil {
		return err
	}
	if r.LinkKeyID != a.linkKey.ID() {
		return fmt.Errorf("link_key_changed: %w", core.ErrPolicyDenied)
	}
	token, err := a.linkToken(req, r)
	if err != nil {
		return err
	}
	hash := sha256.Sum256([]byte(token))
	code, err := a.check(ctx, req.OperationID, hash[:])
	if err != nil {
		return errors.New("metareply: check unavailable")
	}
	if code == "OK" {
		return nil
	}
	if checkDenyCodes[code] {
		return fmt.Errorf("%s: %w", code, core.ErrPolicyDenied)
	}
	return errors.New("metareply: unexpected check result")
}

// loadSecretFor returns the LoadSecret hook of one provider route: its only body is one call to the
// lease-fenced loader integration.load_meta_page_token (definer commerce_integration_writer),
// inside the dispatcher's transaction. Zero rows or a missing attested scope is a pre-dispatch
// denial (capability evidence, arch §10.2/I07).
func (a *adapter) loadSecretFor(provider string) func(context.Context, pgx.Tx, core.SecretClaim) (core.Secret, error) {
	return func(ctx context.Context, tx pgx.Tx, claim core.SecretClaim) (core.Secret, error) {
		var row struct {
			tenant, store, binding, provider, asset, keyID string
			version                                        int64
			nonce, ciphertext                              []byte
			scopes                                         []string
		}
		err := tx.QueryRow(ctx, `SELECT tenant_id::text,store_id::text,binding_id::text,provider,asset_id,version,key_id,nonce,ciphertext,scopes_attested
			FROM integration.load_meta_page_token($1::uuid,$2::bigint,$3::bytea)`,
			claim.OperationID, claim.Generation, claim.LeaseToken).
			Scan(&row.tenant, &row.store, &row.binding, &row.provider, &row.asset, &row.version, &row.keyID, &row.nonce, &row.ciphertext, &row.scopes)
		if errors.Is(err, pgx.ErrNoRows) {
			return core.Secret{}, fmt.Errorf("no page token: %w", core.ErrPolicyDenied)
		}
		if err != nil {
			return core.Secret{}, errors.New("metareply: credential load failed")
		}
		if row.provider != provider || !hasScopes(row.scopes, requiredScopes[provider]) {
			return core.Secret{}, fmt.Errorf("page token lacks attested scope: %w", core.ErrPolicyDenied)
		}
		return a.keys.Open(PageTokenScope{TenantID: row.tenant, StoreID: row.store, BindingID: row.binding,
			Provider: row.provider, AssetID: row.asset, Version: row.version}, row.keyID, row.nonce, row.ciphertext)
	}
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

type graphBody struct {
	Recipient struct {
		CommentID string `json:"comment_id"`
	} `json:"recipient"`
	Message struct {
		Text string `json:"text"`
	} `json:"message"`
	AccessToken string `json:"access_token,omitempty"`
}

// dispatch sends the one POST. Every result other than 2xx-with-message_id is UNKNOWN
// graph_unconfirmed: the request may have been accepted, so it is never repeated (the private reply
// budget is one per comment) and only the query-only Reconcile follows. FAILED_FINAL stays unused
// until probe U3 documents the permanent error codes. A pre-send failure returns an error, which
// the dispatcher also records as UNKNOWN with zero calls.
func (a *adapter) dispatch(ctx context.Context, req core.DispatchRequest, secret core.Secret) (core.Outcome, error) {
	r, err := parseRequest(req)
	if err != nil {
		return core.Outcome{}, err
	}
	if r.LinkKeyID != a.linkKey.ID() || !validCommentRef(r.CommentRef) || len(secret.Reveal()) == 0 {
		return core.Outcome{}, errBadRequest
	}
	token, err := a.linkToken(req, r)
	if err != nil {
		return core.Outcome{}, err
	}
	text, err := RenderClaimLink(r.Locale, r.Origin, token)
	if err != nil {
		return core.Outcome{}, err
	}
	var body graphBody
	body.Recipient.CommentID, body.Message.Text = r.CommentRef, text
	if !a.cfg.AuthorizationHeader {
		body.AccessToken = string(secret.Reveal())
	}
	// ponytail: Go strings and net/http buffers cannot be zeroed end to end; the token is
	// process-memory only and short-lived. Upgrade path: none in stdlib.
	payload, err := json.Marshal(body)
	if err != nil {
		return core.Outcome{}, errBadRequest
	}
	url := a.cfg.GraphBaseURL + "/" + a.cfg.GraphVersion + "/" + req.ExternalAssetID + "/messages"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return core.Outcome{}, errBadRequest
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if a.cfg.AuthorizationHeader {
		httpReq.Header.Set("Authorization", "Bearer "+string(secret.Reveal()))
	}
	unconfirmed := core.Outcome{State: "UNKNOWN", Code: codeUnconfirmed}
	resp, err := a.client.Do(httpReq)
	if err != nil {
		return unconfirmed, nil // transport error or timeout: the POST may have landed
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	if err != nil || len(raw) > maxResponseBody || resp.StatusCode < 200 || resp.StatusCode > 299 {
		return unconfirmed, nil
	}
	var ok struct {
		MessageID string `json:"message_id"`
	}
	if json.Unmarshal(raw, &ok) != nil || !validMessageID(ok.MessageID) {
		return unconfirmed, nil
	}
	return core.Outcome{State: "SUCCEEDED", Code: codeSent, ProviderReference: ok.MessageID}, nil
}

// reconcile is query-only and proves nothing until probe U4 shows a read field that does; it
// never dispatches again.
func (a *adapter) reconcile(context.Context, core.DispatchRequest) (core.Outcome, error) {
	return core.Outcome{State: "UNKNOWN", Code: codeUnproven}, nil
}

func printable(s string, max int) bool {
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) < 1 || utf8.RuneCountInString(s) > max {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

func validMessageID(s string) bool { return printable(s, 200) }

// validCommentRef: the plain platform comment id (IR-4), 1..200 printable characters.
func validCommentRef(s string) bool { return printable(s, 200) }
