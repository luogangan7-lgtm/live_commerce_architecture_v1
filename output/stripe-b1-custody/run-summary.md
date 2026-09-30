# Stripe B1 custody UNIT evidence

- Unit: `stripe-b1-custody`
- Role: `integration_worker`; actual model: `gpt-6-sol`, reasoning: `high`
- Base: `9f9d0314623cb4072268819c9871a6291fb35c80`
- Candidate: `ea072594f332b80e99d6f95425d3d32391fdd703`
- Branch/worktree: `codex/stripe-custody`, `/Users/luolimo/.codex/worktrees/stripe-custody/live_commerce_architecture_v1`
- Changed: `internal/integrations/accounts/stripe_crypto.go`, `internal/integrations/accounts/stripe_crypto_test.go`

## UNIT gate

| Command | Exit | Observed result |
| --- | ---: | --- |
| `GOTOOLCHAIN=go1.27.1 go test -race -count=1 -v ./internal/integrations/accounts ./internal/integrations/psp/stripe` | 0 | 2 packages passed; 22 top-level PASS, 0 FAIL, 1 SKIP (existing Stripe sandbox gate) |
| `GOTOOLCHAIN=go1.27.1 go vet ./internal/integrations/accounts` | 0 | No diagnostics |
| `git diff --cached --check` | 0 | No whitespace errors before commit |

The first focused test run failed on an intentionally forged duplicate-key JSON payload. The decoder was changed to require the canonical private wire form; the final gate above passed. Existing PAYUNi tests ran in the `accounts` package. Tests cover both Stripe purposes, independent scope fields, rotation, ciphertext tampering and bounds, Stage-A validation, cross-purpose rejection, and redaction.

## Scope and remaining gates

- Evidence level: UNIT; source and tests only.
- NOT_RUN: REAL_PG, HTTP/process role separation, PROVIDER_MOCK integration, SANDBOX provider calls, BROWSER, LIVE, production deployment.
- Risk: ingress-only versus worker-only Keyring provisioning is an integration/process gate; these pure helpers do not establish it.
- Root integrator must independently review and rerun before integration.
