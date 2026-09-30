# Stripe B1 runtime UNIT evidence (MODEL_ONLY)

Branch `codex/stripe-runtime`, base `67cebe1ffb0c5b78557521cfe535e8ce32faa489`.
Role `commerce_worker`, model `gpt-6-sol`, reasoning `high`.

Changed only `internal/payments/stripe_*.go`, `internal/payments/query_worker.go`,
`internal/payments/runtime.go`, and this evidence file. No SQL, checkout, HTTP,
accounts crypto, credentials, or production provider calls were changed or read.

| Check | Result |
| --- | --- |
| `GOTOOLCHAIN=go1.27.1 go test -race -count=1 ./internal/payments ./internal/integrations/psp/stripe ./internal/integrations/accounts` | exit 0; all three packages pass |
| `GOTOOLCHAIN=go1.27.1 go vet ./internal/payments ./internal/integrations/psp/stripe ./internal/integrations/accounts` | exit 0 |
| `GOTOOLCHAIN=go1.27.1 go test -count=1 ./internal/payments` | exit 0 |

The signal implementation uses the frozen six-argument `integration.load_stripe_signal`
capability in root SQL candidate `5d35ce0`. That SQL is not in this isolated
worktree. The direct unit tests validate config rejection, scope/body drift,
identity checks, bounded rate delay, and malformed signal args. They do not prove
database/River/provider behavior.

NOT_RUN: SP08–SP12 REAL_PG + fake HTTP gate, mixed-account queue, replay/staleness,
atomic rollback and stock/fact outcomes. Requires root to integrate its privileged
SQL candidate with this Go commit and run the exclusive PG lane. This UNIT must
not be reported as B1 or SANDBOX completion until that gate and independent review pass.
