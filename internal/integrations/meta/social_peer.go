// social_peer.go exports the social peer key derivation for the claims retention operator
// (contracts/claims-retention-purge-v1.md D1). Non-goals: no SQL, no network, no state; it is the
// same tupleHash projection.go writes into social.conversations.peer_key, exported so
// cmd/retention-admin can find a person's conversations without duplicating the format.

package meta

// SocialPeerKey is social.conversations.peer_key for one sender: the unkeyed tupleHash of
// ("meta-social-peer/v1", app, object, asset, sender). Unkeyed because it must equal what the
// consumer projection stored; callers must therefore never log it.
func SocialPeerKey(app, object, asset, sender string) string {
	return tupleHash("meta-social-peer/v1", app, object, asset, sender)
}
