// merchant_ads.go builds the merchant Meta-ads service for the API process (meta-ads-v1 §2 steps 1-3,
// ads-graph G3/G6): the OAuth connect exchange (Meta app secret from a file), the HPKE PUBLIC keys
// used to seal the returned BISU token, and an insert-only River client for the ads operation jobs.
//
// Non-goals: no route (internal/httpapi/ads.go), no rule (internal/ads), no Graph write (that is
// cmd/ads-worker), and NO private key: this file must never import meta_ads/tokenopen, which is
// how cmd/api is kept unable to read a token back (gate MA11; merchant_ads_test.go checks
// `go list -deps`). External: graph.facebook.com during the connect callback only, through
// internal/integrations/meta_ads (the only OIDC-issuer exception in doc.go gains this one).
// Wiring (Options.Ads in main.go) is the integrator's hook.

package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"livecommerce/internal/ads"
	metaads "livecommerce/internal/integrations/meta_ads"
)

var (
	errMerchantAdsConfig = errors.New("merchant_ads_invalid_config")

	adsConfigIDPattern = regexp.MustCompile(`^[0-9]{1,40}$`)
)

// maxAppSecretFile bounds the app-secret file (a Meta app secret is 32 hex characters).
const maxAppSecretFile = 512

// newMerchantAds returns nil, nil when the surface is off (COMMERCE_META_ADS_APP_ID unset), so a
// deployment without ads reads no other variable. When the app id is set, every other value is
// required and any missing or invalid one is the one fixed errMerchantAdsConfig (never the value,
// the secret file's contents or a path).
//
// Variables (names only): COMMERCE_META_ADS_APP_ID, COMMERCE_META_ADS_APP_SECRET_FILE,
// COMMERCE_META_ADS_CONFIG_ID, COMMERCE_META_ADS_REDIRECT_URI, COMMERCE_META_ADS_GRAPH_VERSION,
// COMMERCE_META_ADS_TOKEN_HPKE_PUBLIC_KEYS_JSON, COMMERCE_META_ADS_TOKEN_HPKE_ACTIVE_KEY_ID.
func newMerchantAds(pool *pgxpool.Pool, getenv func(string) string) (*ads.Service, error) {
	if getenv == nil {
		return nil, errMerchantAdsConfig
	}
	appID := getenv("COMMERCE_META_ADS_APP_ID")
	if appID == "" {
		return nil, nil
	}
	configID := getenv("COMMERCE_META_ADS_CONFIG_ID")
	redirect := getenv("COMMERCE_META_ADS_REDIRECT_URI")
	version := getenv("COMMERCE_META_ADS_GRAPH_VERSION")
	if pool == nil || !adsConfigIDPattern.MatchString(configID) {
		return nil, errMerchantAdsConfig
	}
	// COMMERCE_META_ADS_APP_SECRET_FILE (path) or, after lcentry expansion in a container,
	// COMMERCE_META_ADS_APP_SECRET (contents): see metaads.SecretFromEnv.
	secret, err := metaads.SecretFromEnv(getenv, "COMMERCE_META_ADS_APP_SECRET", maxAppSecretFile)
	if err != nil {
		return nil, errMerchantAdsConfig
	}
	defer clear(secret)
	keys, err := metaads.LoadSealKeys(getenv)
	if err != nil {
		return nil, errMerchantAdsConfig
	}
	// Graph base URL stays the fixed production host (empty = GraphHost): no env can redirect the
	// code exchange, which carries the app secret, to another host.
	cfg := metaads.Config{GraphVersion: version}
	oauth, err := metaads.NewOAuth(cfg, metaads.AppConfig{AppID: appID, RedirectURI: redirect, AppSecret: secret}, keys)
	if err != nil {
		return nil, errMerchantAdsConfig
	}
	// The runtime pool is used solely by River's transactional insert path (planners enqueue ops in
	// their own transaction). No queue or worker starts in the API process.
	jobs, err := river.NewClient[pgx.Tx](riverpgxv5.New(pool), &river.Config{Schema: "river"})
	if err != nil {
		return nil, errMerchantAdsConfig
	}
	// OAuth state key: HMAC of a fixed label under the app secret (domain-separated, never the secret itself); the api
	// already holds the secret for the code exchange, so no new deploy secret is needed.
	km := hmac.New(sha256.New, secret)
	km.Write([]byte("livecommerce/ads-oauth-state-key/v1"))
	svc, err := ads.NewService(jobs, oauth.Connect, ads.DialogConfig{AppID: appID, ConfigID: configID, RedirectURI: redirect, GraphVersion: version, StateKey: km.Sum(nil)})
	if err != nil {
		return nil, errMerchantAdsConfig
	}
	return svc, nil
}
