# stripe-b1-start-http run summary

Base ba08af8, branch unit/stripe-b1-start-http. Model: Claude Sonnet 5.5 (commerce_worker).
Evidence labels per PROCESS.md section 4. SP07/SP14 are stripe-b1-tests-b gates and are NOT claimed here.

| Gate | Command | Exit | Counts | Label |
| --- | --- | --- | --- | --- |
| Unit + race | `go test -race -count=1 -v ./internal/checkout ./internal/buyerhttp ./cmd/api` | 0 | PASS 140 (incl. subtests), FAIL 0, SKIP 0 | MOCK (unit; no PG, no provider) |
| vet / gofmt / build | `go vet ./internal/checkout ./internal/buyerhttp ./cmd/api`; `gofmt -l internal cmd`; `go build ./...` | 0 / empty / 0 | - | MOCK |
| Red run | digest constant mutated 2400->2401 | 1 | TestStripeHostedConfigDigestIsFrozenShape FAIL (red-mutation-digest.log) | MOCK |
| PG regression | `bash scripts/dev/test-focused.sh '^Test(BuyerPayment\|BuyerHTTP\|StripeSP21Pinned)'` | 0 | PASS 95 (incl. subtests), FAIL 0, SKIP 0 | REAL_PG, existing tests only |

The PG regression applies the amended 0061 (new `checkout.hosted_payment_provider`) to a disposable
DB and proves PAYUNi buyer payment + SP21 still pass. It does NOT execute the new Go Stripe paths.

NOT_RUN: REAL_PG execution of BeginHosted/TakeHosted/PaymentView v2/Refresh/Cancel Stripe branches
and of `hosted_payment_provider` (stripe-b1-tests-b, after pool-fix merges); SP07, SP14; browser/BFF;
SANDBOX/LIVE.
