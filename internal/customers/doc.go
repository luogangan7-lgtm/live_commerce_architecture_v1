// Package customers owns consent and owner-level privacy actions (contracts/customers-billing-v1.md,
// FROZEN 2026-09-30): the merchant customer list/detail read-time projection over buyer.owners (CD1-CD3),
// append-only consent with the single gate customers.consent_allows (CD4/CD5), and the synchronous export and
// erasure of one owner with tombstone replay (CD6-CD8).
//
// It never merges identities, sends marketing, calls Meta or touches actor-level data (U08): a redeemed claim
// link is not identity proof, so actor_key, claims.meta_intake, live.claim_sources and social.* stay
// untouched by owner erasure. It never decides authority or a privacy rule itself; migrations/0078 does
// (definers verify the merchant or buyer credential), this package builds the requests, decodes the strict
// results, assembles the export document and maps database error classes to stable sentinels. Order detail is
// reused from internal/merchantorders, never re-implemented.
//
// It calls no external service. Tables and functions it reaches (all in one transaction supplied by the caller):
// identity.read_merchant_customers (customers:read), customers.merchant_withdraw_consent / record_export /
// erase_owner (customers:privacy), and for the buyer customers.buyer_set_consent / buyer_read_privacy /
// buyer_export_orders / record_export / erase_owner.
package customers
