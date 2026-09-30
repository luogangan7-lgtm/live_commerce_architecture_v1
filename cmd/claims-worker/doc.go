// Command claims-worker owns the T10c claims host (meta-claims-intake-v1 §5.3, IR-13): it runs the
// claims intake poller (internal/claimsintake) and the main-schema external_operation_v1 River
// worker whose only routes are the Meta private replies (internal/integrations/metareply).
//
// It never serves HTTP, never reads the Meta payload keyring, the claims actor key K_actor (owned by
// cmd/meta-worker) or any Stripe variable, never sends anything but the first private reply of a new claim bundle, and never
// prints a key, token, DSN or driver error (one fixed error string per failure class).
//
// External: graph.facebook.com only (private replies via internal/integrations/metareply; never in tests).
//
// Environment: COMMERCE_CLAIMS_WORKER_ENABLED (""/0 off, 1 on), COMMERCE_CLAIMS_INTAKE_DATABASE_URL,
// COMMERCE_WORKER_DATABASE_URL (same database), COMMERCE_RETENTION_JOB_DATABASE_URL (lc_retention_job, same
// database: the hourly claims_retention_v1 purge, internal/retention), COMMERCE_CLAIMS_REPLY_LINK_KEY (std base64, 32 bytes),
// COMMERCE_META_PAGE_TOKEN_ACTIVE_KEY_ID + COMMERCE_META_PAGE_TOKEN_KEYS_JSON, COMMERCE_META_GRAPH_VERSION
// (required, no default: probe U5), optional COMMERCE_META_GRAPH_BASE_URL (default https://graph.facebook.com;
// loopback http://127.0.0.1:<port> for MOCK) and COMMERCE_META_GRAPH_AUTH_HEADER (""/0 token in JSON body, 1 Bearer).
// ECPay CVS route (taiwan-cvs-logistics-v1 §12, registered only when CVS_ECPAY_ENABLED=1): CVS_ECPAY_ENABLED,
// CVS_ECPAY_LIVE_CREATE, COMMERCE_CVS_HOOKS_ORIGIN, and, only when enabled, COMMERCE_PAYMENT_PROFILE (one ECPay
// environment per deployment) and ECPAY_LOGISTICS_KEYRING. The worker calls logistics(-stage).ecpay.com.tw through
// internal/integrations/shipping/ecpay.
package main
