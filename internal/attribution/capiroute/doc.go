// Package capiroute owns the dispatcher route (provider meta_dataset, action meta.capi.purchase) that sends one Meta
// Conversions API Purchase event per planned operation: the PG-only Check (consent, binding, environment, age), the
// lease-fenced LoadSecret that reads the buyer's user data and the dataset token in one transaction and hashes in
// memory, and the DispatchWithSecret that posts exactly one event through metaads.Client.PostEvent.
//
// It never resends an event after an UNKNOWN result (Reconcile returns UNKNOWN unchanged and needs no secret: Meta
// documents no server-to-server dedup, F15), never persists or logs a hash, phone, user agent or token, never calls
// Meta from Check, and never reads user data outside ads.capi_user_data. Only cmd/ads-worker imports it, because it
// links the HPKE private-key loader (tokenopen) that cmd/api must never contain (gate MA11).
//
// External service: graph.facebook.com/{version}/{pixel_id}/events (via internal/integrations/meta_ads only).
package capiroute
