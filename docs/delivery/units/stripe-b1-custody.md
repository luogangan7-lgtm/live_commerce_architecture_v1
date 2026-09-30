# Unit stripe-b1-custody — purpose-separated credential encryption

Role: integration_worker. Base: `9f9d031` on the continuing cloud branch.
Worktree: `/Users/luolimo/.codex/worktrees/stripe-custody/live_commerce_architecture_v1`.
Branch: `codex/stripe-custody`. No recursive delegation or provider/network calls.

## Goal and interfaces

Implement only the ciphertext boundary required by frozen Stripe §0.2, reusing
`accounts.Keyring` and AES-256-GCM. Keep PAYUNi `Credentials`, `credentialAAD`,
its validators and its serialized bytes unchanged. Do not build a second keyring.

Public types in package accounts:

```go
type StripeAPIScope struct {
    TenantID, StoreID, ConnectionID, Environment, AccountID string
    CredentialVersion int64
}
type StripeWebhookScope struct {
    TenantID, StoreID, ConnectionID, EndpointID, Environment, AccountID, Profile string
    KeyVersion int64
}
type StripeAPICredentials struct { SecretKey string }
type StripeWebhookSecrets struct { CurrentSecret, NextSecret string }
func (k *Keyring) SealStripeAPI(StripeAPIScope, StripeAPICredentials) (string, []byte, []byte, error)
func (k *Keyring) OpenStripeAPI(StripeAPIScope, string, []byte, []byte) (StripeAPICredentials, error)
func (k *Keyring) SealStripeWebhook(StripeWebhookScope, StripeWebhookSecrets) (string, []byte, []byte, error)
func (k *Keyring) OpenStripeWebhook(StripeWebhookScope, string, []byte, []byte) (StripeWebhookSecrets, error)
```

Seal returns key ID, nonce, ciphertext. Open accepts those values in that order.
Secret-bearing types implement redacted String, GoString and MarshalJSON; the
encryption payload uses a private wire struct so redaction cannot erase secrets
before encryption. API plaintext contains only `secret_key`; webhook plaintext
contains only `current_secret` and optional `next_secret`.

AAD contains a fixed, caller-unsettable purpose discriminator `stripe-api-v1` or
`stripe-webhook-v1`, fixed provider `stripe`, and every corresponding scope field.
Use deterministic JSON structs. Scope IDs must be valid UUIDs, versions positive,
environment SANDBOX only, webhook profile PROVIDER_MOCK or SANDBOX. No LIVE in B1.
Use Stage-A constructors to validate API/signing secrets without network I/O;
do not weaken PAYUNi validation or add another ad-hoc Stripe key grammar.
Reject unknown/missing/oversized material and wrong-purpose/scope ciphertext with
fixed errors. Validate bounded ciphertext before attempting decryption. Copy
owned secret bytes as required; never log secret inputs or emit them in test errors.

An ingress process will receive a signing-only Keyring instance; a payment worker
an API-material Keyring instance. These helpers do not load env vars, SQL, or either
process's keys. Actual role separation and lease fences are later integration gates.

## Allowed write paths

- `internal/integrations/accounts/stripe_crypto.go`
- `internal/integrations/accounts/stripe_crypto_test.go`
- `output/stripe-b1-custody/run-summary.md` (only sanitized evidence)

No existing crypto file, SQL, contracts, dependency manifests or fixtures may change.
Private helper functions within the new file may share AES-GCM mechanics.

## Gates and handoff

Actual unit tests: round trips; randomized nonces; active-key rotation with historical
open; wrong key/purpose/each scope field/version/profile; PAYUNi cross-open failure;
missing and malformed material, empty/oversized fields, invalid IDs/live keys;
one/two distinct signing secrets; all formatting/JSON redaction; no plaintext in
ciphertext. Preserve existing PAYUNi tests. Run:

```sh
GOTOOLCHAIN=go1.27.1 go test -race -count=1 ./internal/integrations/accounts ./internal/integrations/psp/stripe
GOTOOLCHAIN=go1.27.1 go vet ./internal/integrations/accounts
```

This is UNIT evidence, not SQL, SANDBOX or LIVE. Return exact commit, model/reasoning,
base, changed paths, commands/exit codes, evidence path, risks and NOT_RUN. Store
the deliverable and update own canvas under the shared lock. Root independently
reruns and reviews before integration. Index changed code and link the decision.
