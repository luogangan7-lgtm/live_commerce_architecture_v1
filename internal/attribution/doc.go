// Package attribution owns the storefront-side half of Meta conversion attribution (contracts/meta-ads-v1.md,
// T16): the capi_purchase_sweep_v1 sweeper that plans one CAPI Purchase operation per consented CAPTURED payment
// attempt, the public product feed of a verified storefront host, the consent hook that records a buyer's browser
// user agent, and the pure hashing helpers (F16 phone hash, C3 external id, AD8 event id).
//
// It never sends anything to Meta (internal/attribution/capiroute and internal/integrations/meta_ads own
// graph.facebook.com), never resends an event after an UNKNOWN result (F15: server-to-server dedup is
// undocumented), never stores a hash, a phone number or any other buyer identifier, and never reads user data
// outside the lease-fenced definer ads.capi_user_data. It holds no consent or billing state: consent is
// customers.consent_allows (CD5) and is asked in the same transaction that plans the job.
//
// External services: none called from this package. Every write goes through an ads.* SECURITY DEFINER function of
// migrations/0080_meta_capi.sql (owner commerce_ads_writer); the package is importable by cmd/api (feed, consent
// hook) and cmd/ads-worker (sweeper) and never links the HPKE private-key loader.
package attribution
