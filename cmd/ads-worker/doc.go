// Command ads-worker owns the Meta ads River host (meta-ads-v1 §6, ads-graph): the main-schema
// external_operation_v1 dispatcher whose only routes are the eight meta_ads actions
// (internal/integrations/meta_ads), the ads domain workers and periodic jobs (internal/ads:
// publish-advance sweeper, insights planner, OAuth-state purge), all on the River queue "ads" only.
// It is the ONLY process that holds the HPKE private key file and can therefore open a sealed BISU
// token (A-4, MA11); the token reaches code only through the lease-fenced loader
// integration.load_meta_ads_token inside the dispatcher's transaction.
//
// It never serves HTTP, never works the "default" queue (cmd/claims-worker owns it), never reads the
// Meta app secret, the Stripe or page-token variables, never sends a message to a buyer, and never
// prints a key, token, DSN or driver error (one fixed error string per failure class).
//
// External: graph.facebook.com only, via internal/integrations/meta_ads (never in tests; loopback
// httptest there). Ad-account writes are refused by ads.Checker unless the store's environment allows
// them (AD9) and an approval + allowance hold (AD5/AD6).
//
// Environment (names only; every one is required, any missing or invalid value refuses to start):
// COMMERCE_ADS_WORKER_DATABASE_URL (commerce_worker login), COMMERCE_META_ADS_GRAPH_VERSION (v26.0, A-9),
// COMMERCE_META_ADS_TOKEN_HPKE_PRIVATE_KEYS_FILE (path of the owner-supplied key file, O-D),
// COMMERCE_META_ADS_PARTNER_AGENT (CAPI partner_agent, F7), COMMERCE_CAPI_EXTERNAL_ID_KEY_FILE (C3 HMAC key of the
// CAPI external_id, >= 32 bytes, worker only, O-D). In a container deploy/tools/lcentry expands
// each NAME_FILE into NAME=<contents>; the key file is accepted in either form (metaads.SecretFromEnv).
package main
