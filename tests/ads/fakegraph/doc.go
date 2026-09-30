// Package fakegraph owns a MOCK of the parts of Meta's Graph / Marketing API that meta-ads-v1 (§1 F-table, §3) relies on,
// written by the independent ads-tests author from the contract and from Meta's public documentation only, never from the
// adapter under test. It serves, on a loopback httptest listener:
//
//   - POST /{ver}/act_{id}/campaigns|adsets|adcreatives|ads (create; required-field checks per F8/F22/F12; no idempotency
//     key, no name filter: exactly the property F9 says makes tag reconcile necessary)
//   - GET  /{ver}/{parent}/campaigns|adsets|ads and /act_{id}/adcreatives|campaigns (list, id+name, cursor paging)
//   - POST /{ver}/{campaign} status (activate / pause) and GET /{ver}/{campaign}?fields=status,effective_status; a child's
//     effective_status is CAMPAIGN_PAUSED while its campaign is PAUSED (F9, F22)
//   - GET  /{ver}/act_{id} (preflight: account_status, currency, timezone_name, min_daily_budget, funding_source only when
//     funded, F10) and GET /{ver}/{campaign}/insights (F13)
//   - POST /{ver}/{pixel}/events (CAPI: one event, > 7 days old rejects the batch, website events need event_source_url and
//     client_user_agent, F14)
//   - GET  /{ver}/oauth/access_token (FLfB code exchange, single-use code, F3), /me?fields=client_business_id,
//     /me/permissions, /me/adaccounts, /act_{id}/adspixels
//
// Programmable faults (timeout with or without the remote effect, 5xx, Graph error codes 4/17/613/80004/100, unparseable
// or shapeless 2xx), per-route call counters, captured request bytes (with where the access token travelled: bearer header,
// JSON/form body, or URL query) and a token -> ad account isolation check (a token used on an account it was not granted is
// answered 403 code 200 and recorded as a violation).
//
// What it CANNOT prove (contract §9, last paragraph): that Meta accepts the enum values or field combinations the adapter
// sends (special_ad_categories, optimization_goal/billing_event pairs, U1/U2/U8/U9), ad review or delivery, real rate-limit
// behaviour, insights for a sandbox account (F20), or third-party permissions. Wire shapes mirror, retrieved 2026-09-30:
//
//	https://developers.facebook.com/docs/marketing-api/reference/ad-account/campaigns/   (F8 campaign create)
//	https://developers.facebook.com/docs/marketing-api/reference/ad-campaign-group/      (F9 status / effective_status)
//	https://developers.facebook.com/docs/marketing-api/reference/ad-campaign/            (F22 ad set)
//	https://developers.facebook.com/docs/marketing-api/reference/ad-creative/            (F12 creative)
//	https://developers.facebook.com/docs/marketing-api/reference/ad-account/             (F10 account fields)
//	https://developers.facebook.com/docs/marketing-api/insights/best-practices/          (F13)
//	https://developers.facebook.com/docs/marketing-api/conversions-api/parameters/server-event (F14)
//	https://developers.facebook.com/documentation/facebook-login/facebook-login-for-business (F3)
//	https://developers.facebook.com/docs/graph-api/overview (error envelope, paging.cursors / paging.next)
//
// Every token, code, secret and id in this package's callers is a synthetic sentinel; nothing here reaches a network.
package fakegraph
