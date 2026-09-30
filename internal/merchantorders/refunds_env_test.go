// refunds_env_test.go: unit tests of the deployment-environment guard of RequestRefundIn
// (contracts/stripe-live-enable-v1.md §5.2, brief ruling S5): an order whose attempt is in another environment, or
// has none, is ErrNotRefundable BEFORE the River insert and before payments.request_stripe_refund; a matching
// environment proceeds past the guard. Non-goals: the SQL definers (SL04, independent unit) and River.
// Callers: go test ./internal/merchantorders.

package merchantorders

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"livecommerce/internal/command"
)

// guardTx answers the three statements RequestRefundIn issues before its River insert, in order:
// the replay pre-check (no rows), the environment guard, then gen_random_uuid (which ends the test).
type guardTx struct {
	pgx.Tx
	queries []string
	env     *string
	envErr  error
}

type guardRow struct{ scan func(dest ...any) error }

func (r guardRow) Scan(dest ...any) error { return r.scan(dest...) }

func (g *guardTx) QueryRow(_ context.Context, query string, _ ...any) pgx.Row {
	g.queries = append(g.queries, query)
	switch {
	case strings.Contains(query, "ops.command_results"):
		return guardRow{func(...any) error { return pgx.ErrNoRows }}
	case strings.Contains(query, "merchant_refund_environment"):
		return guardRow{func(dest ...any) error {
			if g.envErr != nil {
				return g.envErr
			}
			*dest[0].(**string) = g.env
			return nil
		}}
	case strings.Contains(query, "gen_random_uuid"):
		return guardRow{func(...any) error { return errStopAfterGuard }}
	}
	panic("unexpected statement: " + query)
}

var errStopAfterGuard = errors.New("stop after guard")

func envPtr(s string) *string { return &s }

func guardJobs(t *testing.T) *river.Client[pgx.Tx] {
	t.Helper()
	jobs, err := river.NewClient(riverpgxv5.New(nil), &river.Config{Schema: "river_payment"})
	if err != nil {
		t.Fatal(err)
	}
	return jobs
}

func TestRequestRefundInEnvironmentGuard(t *testing.T) {
	req := RefundRequest{AmountMinor: 100, Reason: "duplicate", ExpectedRefundableMinor: 100}
	jobs := guardJobs(t)
	for _, tc := range []struct {
		name        string
		deployment  string
		attemptEnv  *string
		wantRefused bool
	}{
		{"SANDBOX attempt in a LIVE deployment", "LIVE", envPtr("SANDBOX"), true},
		{"LIVE attempt in a SANDBOX deployment", "SANDBOX", envPtr("LIVE"), true},
		{"order without an attempt (NULL)", "LIVE", nil, true},
		{"NULL in a SANDBOX deployment", "SANDBOX", nil, true},
		{"LIVE attempt in a LIVE deployment", "LIVE", envPtr("LIVE"), false},
		{"SANDBOX attempt in a SANDBOX deployment", "SANDBOX", envPtr("SANDBOX"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := &guardTx{env: tc.attemptEnv}
			_, err := RequestRefundIn(context.Background(), tx, jobs, scope, tc.deployment, token, "refund-key-0001", rfOrder, req)
			last := tx.queries[len(tx.queries)-1]
			if tc.wantRefused {
				if !errors.Is(err, ErrNotRefundable) {
					t.Fatalf("want ErrNotRefundable, got %v", err)
				}
				if strings.Contains(last, "gen_random_uuid") {
					t.Fatal("a refused refund reached the River insert path")
				}
				return
			}
			// Past the guard the next statement is the refund id draw; the stub stops there, before any River insert.
			if !strings.Contains(last, "gen_random_uuid") {
				t.Fatalf("matching environment stopped early: %v", last)
			}
		})
	}
}

func TestRequestRefundInMapsGuardAuthorityErrorsAndValidatesEnvironment(t *testing.T) {
	req := RefundRequest{AmountMinor: 100, Reason: "duplicate", ExpectedRefundableMinor: 100}
	jobs := guardJobs(t)
	// A missing order is the definer's PT404 (same answer the request definer gives): not a not_refundable 422.
	tx := &guardTx{envErr: &pgconn.PgError{Code: "PT404", Message: "order not found"}}
	if _, err := RequestRefundIn(context.Background(), tx, jobs, scope, "LIVE", token, "refund-key-0001", rfOrder, req); errors.Is(err, ErrNotRefundable) || err == nil {
		t.Fatalf("PT404 mapped to %v", err)
	}
	for _, code := range []string{"PT401", "PT403"} {
		tx := &guardTx{envErr: &pgconn.PgError{Code: code, Message: "denied"}}
		if _, err := RequestRefundIn(context.Background(), tx, jobs, scope, "LIVE", token, "refund-key-0001", rfOrder, req); errors.Is(err, ErrNotRefundable) || err == nil {
			t.Fatalf("%s mapped to %v", code, err)
		}
	}
	for _, env := range []string{"", "PROVIDER_MOCK", "live", "PRODUCTION"} {
		tx := &guardTx{env: envPtr("LIVE")}
		if _, err := RequestRefundIn(context.Background(), tx, jobs, scope, env, token, "refund-key-0001", rfOrder, req); !errors.Is(err, command.ErrInvalid) || len(tx.queries) != 0 {
			t.Fatalf("environment %q: %v after %d statements", env, err, len(tx.queries))
		}
	}
}

// r2 close (review P2): the refresh route of the same §7.1 family takes the same guard as the POST, before its
// River insert, so a LIVE API never enqueues a wake-up for a pre-cutover SANDBOX refund (the LIVE worker would
// refuse it and W11 would never see it).
func TestRefreshRefundInEnvironmentGuard(t *testing.T) {
	jobs := guardJobs(t)
	for _, tc := range []struct {
		name        string
		deployment  string
		attemptEnv  *string
		wantRefused bool
	}{
		{"SANDBOX attempt in a LIVE deployment", "LIVE", envPtr("SANDBOX"), true},
		{"LIVE attempt in a SANDBOX deployment", "SANDBOX", envPtr("LIVE"), true},
		{"order without an attempt (NULL)", "LIVE", nil, true},
		{"LIVE attempt in a LIVE deployment", "LIVE", envPtr("LIVE"), false},
		{"SANDBOX attempt in a SANDBOX deployment", "SANDBOX", envPtr("SANDBOX"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := &guardTx{env: tc.attemptEnv}
			_, err := RefreshRefundIn(context.Background(), tx, jobs, scope, tc.deployment, token, rfOrder, rfRefund)
			last := tx.queries[len(tx.queries)-1]
			if tc.wantRefused {
				if !errors.Is(err, ErrNotRefundable) {
					t.Fatalf("want ErrNotRefundable, got %v", err)
				}
				if strings.Contains(last, "gen_random_uuid") {
					t.Fatal("a refused refresh reached the River insert path")
				}
				return
			}
			if !strings.Contains(last, "gen_random_uuid") { // the signal id draw precedes the River insert
				t.Fatalf("matching environment stopped early: %v", last)
			}
		})
	}
	for _, env := range []string{"", "live", "PRODUCTION"} {
		tx := &guardTx{env: envPtr("LIVE")}
		if _, err := RefreshRefundIn(context.Background(), tx, jobs, scope, env, token, rfOrder, rfRefund); !errors.Is(err, command.ErrInvalid) || len(tx.queries) != 0 {
			t.Fatalf("environment %q: %v after %d statements", env, err, len(tx.queries))
		}
	}
}
