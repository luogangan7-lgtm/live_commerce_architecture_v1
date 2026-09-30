// Package ads owns the merchant-side lifecycle of a Meta ad campaign (contracts/meta-ads-v1.md, T15):
// connecting an ad account (OAuth state, pick list, sealed token copy), drafting, approving and
// publishing a campaign whose only spend switch is a Check-gated `activate` operation, pausing it,
// the per-store allowance, and the periodic sweepers that plan the next remote step or ingest
// insights. Every write goes through a SECURITY DEFINER function of the `ads` schema (owner
// commerce_ads_writer); this package holds no SQL authority of its own beyond calling them.
//
// It never dials Meta (internal/integrations/meta_ads owns graph.facebook.com), never holds or opens a
// Meta access token (the token is sealed by the connect callback and opened only by cmd/ads-worker
// through the dispatcher's lease-fenced LoadSecret), never pays for ads (the platform is not the
// payer, arch 15.3), and never changes the budget of an existing remote object or re-activates a paused
// campaign (ruling X7).
//
// External services: none called from here. The callback exchange is injected as ConnectFunc
// (implemented by metaads.OAuth against graph.facebook.com); the dialog host www.facebook.com only
// appears in the URL this package builds for the merchant's browser.
package ads
