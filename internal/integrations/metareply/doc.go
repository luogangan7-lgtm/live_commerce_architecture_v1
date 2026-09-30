// Package metareply owns the first Meta private reply of a keyword-claim bundle
// (meta-claims-intake-v1 §6.3, §7): the dispatcher routes (facebook|instagram, meta.private_reply,
// service), the per-store Page-token custody (AES-256-GCM seal/open and the registrar call), the
// operator route registration (RegisterRoute/DisableRoute: store binding + webhook route, R1 ruling F2)
// and the fixed reply text.
//
// It never plans operations or issues links (internal/claimsintake and the SQL definers do), never
// sends more than one POST per operation (every non-2xx or doubt is UNKNOWN; Reconcile is
// query-only and returns UNKNOWN until probe U4), never falls back to another channel, never logs
// or returns a token, comment id or response body, and never refreshes or issues Page tokens (T07).
//
// External: graph.facebook.com POST /{version}/{asset_id}/messages — the one private-reply send per
// operation (loopback httptest only in MOCK); docs
// https://developers.facebook.com/docs/messenger-platform/discovery/private-replies/ and
// https://developers.facebook.com/docs/instagram-platform/private-replies/ (retrieved 2026-09-28).
package metareply
