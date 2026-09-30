package ads

import (
	"context"
	"errors"
	"fmt"
)

// F0 of unit ads-core: the FROZEN types that ads-graph, ads-capi, ads-ui, ads-tests and the integrator
// compile against (docs/delivery/units/ads-core.md "FROZEN Go interface"). Nothing here touches PG or
// the network. Money, stock and retry rules live in the files that use these types.

const redactedToken = "[redacted]"

// SealedToken is a Meta access token after HPKE sealing (contract A-4): KeyID names the recipient
// public key, Enc is the encapsulated key (32 bytes, stored in the nonce column) and Ciphertext the
// AEAD output. It carries no plaintext, but every formatter still redacts it so a log line can never
// carry sealed material either.
type SealedToken struct {
	KeyID      string
	Enc        []byte
	Ciphertext []byte
}

func (SealedToken) String() string               { return redactedToken }
func (SealedToken) GoString() string             { return redactedToken }
func (SealedToken) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(redactedToken)) }
func (SealedToken) MarshalJSON() ([]byte, error) { return []byte(`"` + redactedToken + `"`), nil }
func (SealedToken) MarshalText() ([]byte, error) { return []byte(redactedToken), nil }

// Pick is one selectable asset returned by the connect callback (contract 2 step 2): an ad account or a
// dataset (pixel). Only ids and display names; never a token or a business secret.
type Pick struct {
	Kind          string `json:"kind"` // ad_account | dataset
	ID            string `json:"id"`
	Name          string `json:"name"`
	Currency      string `json:"currency,omitempty"`
	Timezone      string `json:"timezone,omitempty"`
	AccountStatus int    `json:"account_status,omitempty"`
}

// SealInfo is the HPKE info context for sealing a token (contract 4.1): info = ["livecommerce/meta-ads-token/v1",
// tenant, store, key_id]. The store scope binds the ciphertext to one store.
type SealInfo struct{ TenantID, StoreID string }

// ConnectResult is what the OAuth code exchange yields: the Meta client business, the granted scopes,
// the pick list and the sealed token. The plaintext token never leaves the ConnectFunc implementation.
type ConnectResult struct {
	ClientBusinessID string
	Scopes           []string
	Picks            []Pick
	Token            SealedToken
}

// ConnectFunc exchanges an OAuth code for a ConnectResult. It is implemented by (*metaads.OAuth).Connect
// (ads-graph); ads-core only calls it, with no transaction open.
type ConnectFunc func(ctx context.Context, code string, seal SealInfo) (ConnectResult, error)

// DialogConfig is the Facebook Login for Business dialog configuration (contract 2 step 1). The dialog
// host is www.facebook.com; AppID is app 大梦 (O-C), RedirectURI the fixed admin callback.
//
// StateKey (>= 32 bytes, server-side only, derived from the Meta app secret in cmd/api) keys the OAuth state HMAC, so a
// reader of ops.command_results (which keeps the Idempotency-Key) still cannot recompute a live state.
type DialogConfig struct {
	AppID, ConfigID, RedirectURI, GraphVersion string
	StateKey                                   []byte
}

// ErrConnectFailed is returned by a ConnectFunc for any exchange failure; the HTTP layer maps it to
// 502 meta_connect_failed without echoing the cause.
var ErrConnectFailed = errors.New("meta connect failed")
