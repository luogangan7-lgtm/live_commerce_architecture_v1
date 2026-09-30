// Package retention owns the claims retention job and the actor-erasure calls of
// contracts/claims-retention-purge-v1.md (U08): the River job claims_retention_v1 that runs
// claims.run_retention in batches on the retention-job login, and the operator calls (Erase,
// SetPolicy, GetStatus, Replay, RunOnce) that cmd/retention-admin makes on the operator login.
// The SQL definers of migrations/0071_claims_retention.sql are the only code that touches
// claims, social or integration rows.
//
// It never chooses rows (the 0071 definers do), never calls the network, never logs an id,
// key or comment ref, and never holds a secret: Selector and Counts print no identifier, and
// the callers derive keys (meta.ClaimActorKey, meta.SocialPeerKey) before they get here. It
// also never starts a service of its own: cmd/claims-worker hosts the job, and
// cmd/retention-admin (operator only) hosts the calls.
package retention
