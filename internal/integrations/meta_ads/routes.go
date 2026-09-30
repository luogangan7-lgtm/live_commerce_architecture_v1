package metaads

// routes.go: the eight meta_ads dispatcher routes and the token loader hook (contract §6.1, AD11).
// Check is supplied by the caller (ads.Checker.Check: PG only, no network, no secret); the token
// reaches this package only through LoadSecret, which runs inside the dispatcher's lease-fenced
// transaction in dispatch AND reconcile mode (A-10).

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"livecommerce/internal/integrations/core"
	"livecommerce/internal/platform"
)

// TokenOpener opens one sealed token (contract §4.1). *tokenopen.Keyring is the only production
// implementation; the parameter is an interface, not the concrete type, because importing tokenopen
// here would put the HPKE private-key loader into cmd/api's dependency graph (metaads is imported
// by cmd/api for OAuth) and break gate MA11. Callers still pass a *tokenopen.Keyring unchanged.
type TokenOpener interface {
	Open(tenantID, storeID, keyID string, enc, ciphertext []byte) ([]byte, error)
}

// Providers of the two ads binding kinds (contract AD2).
const (
	ProviderAds      = "meta_ads"
	ProviderDataset  = "meta_dataset"
	purposeMarketing = "marketing"
)

// requiredScopes are the attested scopes per binding provider (§4.1: registration already requires
// them; this is the defence in depth at use time, a denial before any Graph call).
var requiredScopes = map[string][]string{
	ProviderAds:     {"ads_management"},
	ProviderDataset: {"ads_management", "ads_read"},
}

// tokenRow is one row of integration.load_meta_ads_token.
type tokenRow struct {
	tenant, store, binding, provider, asset, keyID string
	version                                        int64
	nonce, ciphertext                              []byte
	scopes                                         []string
}

// LoadSecret returns the dispatcher LoadSecret hook for one binding provider. Its only body is one
// call to the lease-fenced loader integration.load_meta_ads_token (definer commerce_integration_writer,
// EXECUTE commerce_worker; the only reader of ads ciphertext), inside the dispatcher's transaction.
// Zero rows or a missing attested scope is a policy denial (dispatch: BLOCKED_POLICY before any Graph
// call; reconcile: stays UNKNOWN credential_unavailable, dispatcher A10-D4). ads-capi reuses it for the
// meta_dataset route.
func LoadSecret(provider string, keys TokenOpener) func(context.Context, pgx.Tx, core.SecretClaim) (core.Secret, error) {
	return func(ctx context.Context, tx pgx.Tx, claim core.SecretClaim) (core.Secret, error) {
		var row tokenRow
		err := tx.QueryRow(ctx, `SELECT tenant_id::text,store_id::text,binding_id::text,provider,asset_id,version,key_id,nonce,ciphertext,scopes_attested
			FROM integration.load_meta_ads_token($1::uuid,$2::bigint,$3::bytea)`,
			claim.OperationID, claim.Generation, claim.LeaseToken).
			Scan(&row.tenant, &row.store, &row.binding, &row.provider, &row.asset, &row.version, &row.keyID, &row.nonce, &row.ciphertext, &row.scopes)
		if errors.Is(err, pgx.ErrNoRows) {
			return core.Secret{}, fmt.Errorf("no ads token: %w", core.ErrPolicyDenied)
		}
		if err != nil {
			return core.Secret{}, errors.New("metaads: credential load failed")
		}
		return openRow(provider, keys, row)
	}
}

// openRow checks provider + attested scopes and opens the token. Split from LoadSecret so it is unit
// testable without PG.
func openRow(provider string, keys TokenOpener, row tokenRow) (core.Secret, error) {
	if keys == nil || row.provider != provider || !hasScopes(row.scopes, requiredScopes[provider]) {
		return core.Secret{}, fmt.Errorf("ads token lacks provider or attested scope: %w", core.ErrPolicyDenied)
	}
	plain, err := keys.Open(row.tenant, row.store, row.keyID, row.nonce, row.ciphertext)
	if err != nil {
		return core.Secret{}, errors.New("metaads: credential unavailable")
	}
	defer clear(plain) // NewSecret copies; the dispatcher zeroes the copy after the callback
	return core.NewSecret(plain), nil
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

// adsActions are the eight routes, in the contract's order.
var adsActions = []string{ActionCreateCampaign, ActionCreateAdset, ActionCreateCreative, ActionCreateAd,
	ActionPreflight, ActionActivate, ActionPause, ActionReadInsights}

// Routes returns the eight meta_ads routes (purpose "marketing"). pool must be the commerce_worker
// pool (platform.ValidateWorkerPool) and is used for that check only; no transaction is held across
// I/O. check is ads.Checker.Check. Every route: LoadSecret = load_meta_ads_token + keys,
// DispatchWithSecret, ReconcileWithSecret (A-10).
func Routes(pool *pgxpool.Pool, cfg Config, keys TokenOpener, check func(context.Context, core.DispatchRequest) error) ([]core.DispatchRoute, error) {
	if pool == nil {
		return nil, ErrConfig
	}
	if err := platform.ValidateWorkerPool(context.Background(), pool); err != nil {
		return nil, err
	}
	return newRoutes(cfg, keys, check)
}

func newRoutes(cfg Config, keys TokenOpener, check func(context.Context, core.DispatchRequest) error) ([]core.DispatchRoute, error) {
	if keys == nil || check == nil {
		return nil, ErrConfig
	}
	client, err := NewClient(cfg)
	if err != nil {
		return nil, err
	}
	routes := make([]core.DispatchRoute, 0, len(adsActions))
	for _, action := range adsActions {
		routes = append(routes, core.DispatchRoute{
			Provider: ProviderAds, Action: action, Purpose: purposeMarketing,
			Check:               check,
			LoadSecret:          LoadSecret(ProviderAds, keys),
			DispatchWithSecret:  client.dispatch,
			ReconcileWithSecret: client.reconcile,
		})
	}
	return routes, nil
}
