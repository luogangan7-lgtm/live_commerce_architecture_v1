# Unit stripe-b1-runtime — scoped query/signal processing

Role commerce_worker. Reuse managed `stripe-custody` worktree only after its unit
is committed and released. New branch `codex/stripe-runtime`, base `67cebe1`
(integrator candidate: frozen contract, repaired SQL, independent fake/schema tests,
accepted custody helpers). This does not replace the continuing cloud branch.

## Frozen boundary

Read Stripe contract §0.2, §7–8, §11, §14–15 and existing runtime/query/capture
worker symbols. §0.2 supersedes every global-account/fingerprint/prebuilt-client
clause. Reuse accounts Stripe custody helpers; do not change them or SQL.

```go
func NewStripeRuntime(ctx context.Context, pool *pgxpool.Pool, keys *accounts.Keyring,
    profile string, mockTransport ...http.RoundTripper) (*StripeRuntime, error)
type WorkerConfig struct {
    Profile string
    Concurrency int
    Query QueryWorkerOptions
    Keys *accounts.Keyring
    Stripe *StripeRuntime
}
func NewPaymentWorkerClient(ctx context.Context, pool *pgxpool.Pool,
    c WorkerConfig) (*river.Client[pgx.Tx], error)
func NewSignalWorker(ctx context.Context, pool *pgxpool.Pool,
    s *StripeRuntime, profile string, o QueryWorkerOptions) (*SignalWorker, error)
```

StripeRuntime owns scope-bound credential loading, never a global account/client.
Its constructor checks worker SQL capability/role/queue readiness, not a provider
account. Profile is PROVIDER_MOCK or SANDBOX only. MOCK requires exactly one nonnil
transport; SANDBOX rejects transport injection. No env access or network calls at
construction. Bound worker runtime to the same pool/profile as its assembly.
Each claim loads its exact historical key via `integration.load_stripe_credential`,
decrypts with `OpenStripeAPI`, constructs a Stage-A client and verifies the account
before checkout I/O. All calls share the original claim deadline; no detached I/O,
current-head fallback, key cache, or new idempotency key on retry.

Keep the existing NewWorkerClient signature as a compatibility wrapper. Existing
QueryWorker's PAYUNi branch, CaptureWorker behavior and exported errors remain.
Dispatch an authenticated Stripe operation before the PAYUNi-only family rejection.
The query job still uses `payment_query_v1`; signals use `payment_signal_v1`, both
inside existing river_payment and profile queue. No second engine or scheduler.

**Disabled Stripe must not destroy pending work.** With Stripe=nil, recognized Stripe
query/signal jobs must snooze before the operation claim, not cancel/complete. Register
a bounded non-claiming signal handler even in this configuration so River's unknown-kind
handling cannot discard signals. Malformed/foreign jobs retain fail-closed validation.
An enabled runtime performs the frozen claim/fence/lifecycle; do not bypass SQL fences.

## Write paths / non-goals

- `internal/payments/stripe_*.go` (including local unit tests)
- `internal/payments/query_worker.go`, `internal/payments/runtime.go` (minimal assembly seam)
- `internal/payments/runtime_test.go` (new compatibility assertions only)
- `tests/foundation/stripe_worker_test.go` (prefix helpers `swr`; reuse existing fixtures)
- `output/stripe-b1-runtime/**` sanitized evidence

No accounts crypto, checkout, HTTP, cmd, schema/contracts, dependency or other test edits.
Use private helpers in payments for material loading; do not invent a generic credential
framework. HTTP/checkout/registrar belong to later units. Do not contact real Stripe or
load owner credentials. Read-only account/payment facts via frozen worker functions only.

## Evidence and handoff

Implement §8 lifecycle and SP08–12 runtime coverage: mixed two-Stripe-account + PAYUNi
queue, fake strict API-key isolation, repeated create with the same bytes/key, uncertain
result recovery, create cutoff, open-expire-retrieve, paid/unpaid/amount/account mismatch,
late money obligation without stock reopening, signal replay/staleness, key rotation,
shared timeout/cancellation/panic, and disabled-Stripe retention. Tests must exercise
real PG + fake HTTP where SQL/leases/atomicity are asserted, not source-text checks.

Run race/unit and vet for changed packages first; coordinate the exclusive PG slot with
root before `scripts/dev/test-focused.sh '^TestStripeSP(08|09|10|11|12)'`.
Never claim a gate whose fixture path is not yet available; report its precise dependency.
Do not relax timeouts/invariants or delete prior payment tests.

Commit exact candidate; deliver model/reasoning, base/path ownership, commands/exits,
evidence path and NOT_RUN. Index changed code, store why, update own canvas under lock.
Root independently reruns and reviews; this unit is not B1 or SANDBOX completion.
