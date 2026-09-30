package metaads

// oauth.go: the merchant connect exchange (meta-ads-v1 §2 steps 1-2, G6). cmd/api holds the Meta app
// secret and only HPKE public keys: it exchanges the code, reads what the pick list needs, seals the
// BISU token and returns; the plaintext exists only in this call's memory and is zeroed after sealing.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"livecommerce/internal/ads"
	"livecommerce/internal/command"
)

const (
	// connectTimeout bounds one whole Connect (up to ~45 Graph calls): the merchant is waiting on a
	// browser redirect, so a stuck Meta must fail fast as ErrConnectFailed.
	connectTimeout = 25 * time.Second
	maxAccounts    = 20 // G6: <= 20 accounts get a pixel query and a pick
	accountPages   = 5  // G6: /me/adaccounts <= 5 pages
	pixelsPerAcct  = 5  // keeps the pick list under ads.oauth_states.pick_list's 16 KiB CHECK
	pickListBudget = 15000
	pickNameRunes  = 40
	maxScopes      = 64
)

var (
	scopePattern = regexp.MustCompile(`^[a-z_]{1,64}$`)
)

// AppConfig is the Meta app used for the code exchange. RedirectURI must equal the dialog value and
// the dashboard setting (U10). Every formatter is redacted; AppSecret comes from a secret file (O-D).
type AppConfig struct {
	AppID, RedirectURI string
	AppSecret          []byte
}

func (AppConfig) String() string               { return "[redacted]" }
func (AppConfig) GoString() string             { return "[redacted]" }
func (AppConfig) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte("[redacted]")) }
func (AppConfig) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }
func (AppConfig) MarshalText() ([]byte, error) { return []byte("[redacted]"), nil }

// OAuth performs Connect. It holds the app secret, so it is redacted in every formatter.
type OAuth struct {
	g    *graph
	app  AppConfig
	keys *SealKeys
}

func (OAuth) String() string               { return "[redacted]" }
func (OAuth) GoString() string             { return "[redacted]" }
func (OAuth) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte("[redacted]")) }
func (OAuth) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

// NewOAuth validates cfg and app and copies the secret.
func NewOAuth(cfg Config, app AppConfig, keys *SealKeys) (*OAuth, error) {
	g, err := newGraph(cfg)
	if err != nil {
		return nil, err
	}
	if keys == nil || !numericIDPattern.MatchString(app.AppID) || !ValidToken(app.AppSecret) || len(app.AppSecret) > 128 {
		return nil, ErrConfig
	}
	u, err := url.Parse(app.RedirectURI)
	if err != nil || len(app.RedirectURI) > 512 || u.Host == "" || u.User != nil || u.Fragment != "" ||
		!(u.Scheme == "https" || (u.Scheme == "http" && loopbackPattern.MatchString(u.Scheme+"://"+u.Host))) {
		return nil, ErrConfig
	}
	app.AppSecret = append([]byte(nil), app.AppSecret...)
	return &OAuth{g: g, app: app, keys: keys}, nil
}

// Connect is ads.ConnectFunc. In order (G6): code exchange (with redirect_uri, U10),
// /me?fields=client_business_id, /me/permissions (granted only), /me/adaccounts (<= 5 pages),
// /act_{id}/adspixels per account (<= 20 accounts), then seal and zero. ANY failure is
// ads.ErrConnectFailed with no partial result and no token; the cause is deliberately not returned
// (the exchange URL carries client_secret, Graph error bodies can echo identifiers).
func (o *OAuth) Connect(ctx context.Context, code string, seal ads.SealInfo) (ads.ConnectResult, error) {
	if o == nil || ctx == nil || !validCode(code) || !command.ValidID(seal.TenantID) || !command.ValidID(seal.StoreID) {
		return ads.ConnectResult{}, ads.ErrConnectFailed
	}
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	// 1. Code exchange, server to server. Contract F3 documents GET with client_secret in the query;
	// this is the only call whose URL holds a secret, over TLS to graph.facebook.com, never logged, and
	// its errors are flattened (errTransport). // UNKNOWN until MA-S3 (U10): redirect_uri may be unneeded.
	rep, err := o.g.do(ctx, "GET", "oauth/access_token", url.Values{
		"client_id": {o.app.AppID}, "client_secret": {string(o.app.AppSecret)},
		"redirect_uri": {o.app.RedirectURI}, "code": {code}}, nil, nil)
	if err != nil || !rep.ok() {
		return ads.ConnectResult{}, ads.ErrConnectFailed
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	err = json.Unmarshal(rep.body, &tok)
	clear(rep.body)
	token := []byte(tok.AccessToken)
	tok.AccessToken = ""
	defer clear(token) // ponytail: the string copy inside json/net/http cannot be zeroed; process-memory only
	if err != nil || !ValidToken(token) {
		return ads.ConnectResult{}, ads.ErrConnectFailed
	}

	// 2. Which client business granted the token (U7 guard at bind time).
	rep, err = o.g.do(ctx, "GET", "me", url.Values{"fields": {"client_business_id"}}, token, nil)
	if err != nil || !rep.ok() {
		return ads.ConnectResult{}, ads.ErrConnectFailed
	}
	var me struct {
		ClientBusinessID string `json:"client_business_id"`
	}
	if json.Unmarshal(rep.body, &me) != nil || !numericIDPattern.MatchString(me.ClientBusinessID) {
		return ads.ConnectResult{}, ads.ErrConnectFailed
	}

	// 3. Granted permissions only (scopes_attested).
	scopes, err := o.scopes(ctx, token)
	if err != nil {
		return ads.ConnectResult{}, ads.ErrConnectFailed
	}

	// 4. Ad accounts, then each account's datasets.
	picks, err := o.picks(ctx, token)
	if err != nil {
		return ads.ConnectResult{}, ads.ErrConnectFailed
	}

	// 5. Seal (HPKE public key) and return; the deferred clear zeroes the plaintext.
	sealed, err := o.keys.Seal(seal, token)
	if err != nil {
		return ads.ConnectResult{}, ads.ErrConnectFailed
	}
	return ads.ConnectResult{ClientBusinessID: me.ClientBusinessID, Scopes: scopes, Picks: picks, Token: sealed}, nil
}

func validCode(code string) bool { return ValidToken([]byte(code)) && len(code) <= 2048 }

type pageDoc struct {
	Data   []json.RawMessage `json:"data"`
	Paging struct {
		Cursors struct {
			After string `json:"after"`
		} `json:"cursors"`
		Next string `json:"next"`
	} `json:"paging"`
}

// pages walks a Graph edge with cursors, at most limit pages, calling each on every item.
func (o *OAuth) pages(ctx context.Context, path string, query url.Values, token []byte, limit int, each func(json.RawMessage) bool) error {
	after := ""
	for i := 0; i < limit; i++ {
		q := url.Values{}
		for k, v := range query {
			q[k] = v
		}
		if after != "" {
			q.Set("after", after)
		}
		rep, err := o.g.do(ctx, "GET", path, q, token, nil)
		if err != nil || !rep.ok() {
			return errTransport
		}
		var doc pageDoc
		if json.Unmarshal(rep.body, &doc) != nil {
			return errTransport
		}
		for _, item := range doc.Data {
			if !each(item) {
				return nil
			}
		}
		if doc.Paging.Next == "" || !cursorPattern.MatchString(doc.Paging.Cursors.After) {
			return nil
		}
		after = doc.Paging.Cursors.After
	}
	return nil
}

func (o *OAuth) scopes(ctx context.Context, token []byte) ([]string, error) {
	seen := map[string]bool{}
	err := o.pages(ctx, "me/permissions", nil, token, 2, func(raw json.RawMessage) bool {
		var p struct {
			Permission string `json:"permission"`
			Status     string `json:"status"`
		}
		if json.Unmarshal(raw, &p) == nil && p.Status == "granted" && scopePattern.MatchString(p.Permission) && len(seen) < maxScopes {
			seen[p.Permission] = true
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}

// picks lists ad accounts (account_id, name, currency, timezone_name, account_status) then each
// account's pixels; malformed single items are skipped, a failed call fails the whole connect (G6).
func (o *OAuth) picks(ctx context.Context, token []byte) ([]ads.Pick, error) {
	var accounts []ads.Pick
	err := o.pages(ctx, "me/adaccounts", url.Values{
		"fields": {"account_id,name,currency,timezone_name,account_status"}, "limit": {"25"}}, token, accountPages,
		func(raw json.RawMessage) bool {
			var a struct {
				AccountID string `json:"account_id"`
				Name      string `json:"name"`
				Currency  string `json:"currency"`
				Timezone  string `json:"timezone_name"`
				Status    int    `json:"account_status"`
			}
			if json.Unmarshal(raw, &a) != nil {
				return true
			}
			id := strings.TrimPrefix(a.AccountID, "act_")
			if !numericIDPattern.MatchString(id) {
				return true
			}
			p := ads.Pick{Kind: "ad_account", ID: id, Name: shortName(a.Name), AccountStatus: a.Status}
			if currencyPattern.MatchString(a.Currency) {
				p.Currency = a.Currency
			}
			if tzPattern.MatchString(a.Timezone) {
				p.Timezone = a.Timezone
			}
			accounts = append(accounts, p)
			return len(accounts) < maxAccounts
		})
	if err != nil {
		return nil, err
	}
	picks := append([]ads.Pick(nil), accounts...)
	seen := map[string]bool{}
	for _, acct := range accounts {
		got := 0
		err := o.pages(ctx, "act_"+acct.ID+"/adspixels", url.Values{"fields": {"id,name"}, "limit": {"5"}}, token, 1,
			func(raw json.RawMessage) bool {
				var d struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				}
				if json.Unmarshal(raw, &d) == nil && numericIDPattern.MatchString(d.ID) && !seen[d.ID] && got < pixelsPerAcct {
					seen[d.ID] = true
					got++
					picks = append(picks, ads.Pick{Kind: "dataset", ID: d.ID, Name: shortName(d.Name)})
				}
				return got < pixelsPerAcct
			})
		if err != nil {
			return nil, err
		}
	}
	return fitPicks(picks), nil
}

// shortName keeps a display name printable and short (pick_list is size-bounded, §4.1).
func shortName(s string) string {
	if !utf8.ValidString(s) {
		return ""
	}
	out := make([]rune, 0, pickNameRunes)
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		if len(out) == pickNameRunes {
			break
		}
		out = append(out, r)
	}
	return string(out)
}

// fitPicks drops trailing picks (datasets come last) until the JSON fits ads.oauth_states.pick_list's
// 16 KiB CHECK with margin; the accounts are never dropped before datasets.
func fitPicks(p []ads.Pick) []ads.Pick {
	for len(p) > 0 {
		raw, err := json.Marshal(p)
		if err == nil && len(raw) <= pickListBudget {
			return p
		}
		p = p[:len(p)-1]
	}
	return p
}
