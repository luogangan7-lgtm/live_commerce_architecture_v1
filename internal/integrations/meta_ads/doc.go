// Package metaads owns every Meta Graph wire call of the ads product (contracts/meta-ads-v1.md §3):
// the eight meta_ads dispatcher routes (create_campaign, create_adset, create_creative, create_ad,
// preflight_account, activate, pause, read_insights), the CAPI event POST (Client.PostEvent, called
// by the ads-capi route), the merchant OAuth connect (OAuth.Connect: code exchange, client business,
// granted permissions, ad-account/dataset pick list, HPKE seal), the Meta money conversions (I05),
// the read-result grammar carried in integration.operations.provider_reference, and the seal side of
// token custody (SealKeys: public keys only).
//
// It never plans operations, never decides policy (ads.Checker does, PG only), never reads PG
// outside the lease-fenced loader integration.load_meta_ads_token that the dispatcher hands it,
// never stores or logs a token, never opens a sealed token (only cmd/ads-worker may, through the
// subpackage tokenopen), never re-POSTs a create/activate/pause after an unconfirmed result (Graph
// has no idempotency key, F9) and never follows a redirect.
//
// External: graph.facebook.com only (Config rejects any other host; loopback httptest in MOCK):
// campaigns/adsets/adcreatives/ads/insights/adspixels, /me, /me/permissions, /me/adaccounts,
// /oauth/access_token and POST /{pixel_id}/events. Docs: https://developers.facebook.com/docs/marketing-api/
// and https://developers.facebook.com/docs/facebook-login/facebook-login-for-business (retrieved 2026-09-29/30).
package metaads
