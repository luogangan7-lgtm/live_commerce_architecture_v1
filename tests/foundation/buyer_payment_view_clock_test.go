package foundation_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"livecommerce/internal/buyer"
	"livecommerce/internal/checkout"
	"livecommerce/internal/integrations/accounts"
	"livecommerce/internal/platform"
)

// A real relation lock stops the projection after its initial scope/time read.
// Assert the exact blocker while the deadline is still valid; merely delaying a
// caller cannot prove that the final DB-clock fence was exercised.
func TestBuyerPaymentViewFinalDatabaseClock(t *testing.T) {
	for _, stage := range []string{"capability", "qualification", "page"} {
		t.Run(stage, func(t *testing.T) {
			h := hpSetup(t)
			if stage == "page" {
				if _, err := h.begin(t04Key("bp-clock-page")); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			name := "payment-view-clock-" + t04Tag()
			pool, err := platform.OpenHostedPool(ctx, withApplicationName(t, hpRole(t, h.f), name))
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			keys, err := accounts.NewKeyring("hosted_fixture", map[string][]byte{"hosted_fixture": h.key}, randomBytes(32))
			if err != nil {
				t.Fatal(err)
			}
			service := hpStarter(t, pool, "PROVIDER_MOCK", keys, h.config).(*checkout.HostedPaymentStarter)
			before, beforeOther := bpCounts(t, h)
			holder, err := h.f.owner.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Rollback(context.Background())
			if _, err := holder.Exec(ctx, `LOCK TABLE checkout.orders IN ACCESS EXCLUSIVE MODE`); err != nil {
				t.Fatal(err)
			}
			var expiry time.Time
			switch stage {
			// The existing buyer transaction has a 1s lock timeout. Exercise
			// expiry inside that bound, not its unrelated unavailable fallback.
			case "capability":
				err = h.f.owner.QueryRow(ctx, `UPDATE buyer.capability_sessions SET expires_at=clock_timestamp()+interval '450 milliseconds' WHERE id=$1 RETURNING expires_at`, h.cap.Scope.SessionID).Scan(&expiry)
			case "qualification":
				err = qualUpdateScan(ctx, h.f.owner, `UPDATE payments.account_qualifications SET expires_at=clock_timestamp()+interval '450 milliseconds' WHERE id=$1 RETURNING expires_at`, []any{h.proof}, &expiry)
			case "page":
				err = h.f.owner.QueryRow(ctx, `UPDATE checkout.hosted_payment_pages SET expires_at=clock_timestamp()+interval '450 milliseconds' WHERE attempt_id=(SELECT id FROM checkout.payment_attempts WHERE order_id=$1) RETURNING expires_at`, h.hold.OrderID).Scan(&expiry)
			}
			if err != nil {
				t.Fatal(err)
			}
			type result struct {
				view checkout.OrderPayment
				err  error
			}
			done := make(chan result, 1)
			go func() {
				view, err := service.PaymentView(ctx, h.cap.Token, h.f.storeA1, h.hold.OrderID)
				done <- result{view, err}
			}()
			waitForDatabaseLock(t, h.f.owner, name)
			var causal bool
			err = h.f.owner.QueryRow(ctx, `SELECT clock_timestamp()<$1 AND EXISTS(
			 SELECT 1 FROM pg_stat_activity WHERE application_name=$2 AND state='active'
			 AND query LIKE '%checkout.hosted_payment_view%' AND wait_event_type='Lock'
			 AND $3::int=ANY(pg_blocking_pids(pid)))`, expiry, name, int32(holder.Conn().PgConn().PID())).Scan(&causal)
			if err != nil || !causal {
				t.Fatalf("view did not reach the exact relation blocker before expiry: %v", err)
			}
			mustExec(t, h.f.owner, `SELECT pg_sleep(GREATEST(0,extract(epoch FROM $1::timestamptz-clock_timestamp()))+0.02)`, expiry)
			if err := holder.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-done:
				if stage == "capability" {
					if !errors.Is(got.err, buyer.ErrUnauthorized) || got.view.OrderID != "" || got.view.Methods != nil {
						t.Fatalf("expired capability returned projection: %v", got.err)
					}
				} else if got.err != nil || len(got.view.Methods) != 0 ||
					(stage == "page" && (got.view.HandoffState != "EXPIRED" || got.view.PaymentState != "PENDING")) {
					t.Fatalf("final deadline did not prune payment eligibility: %v", got.err)
				}
			case <-ctx.Done():
				t.Fatal("view did not return after releasing its blocker")
			}
			after, afterOther := bpCounts(t, h)
			if before != after || beforeOther != afterOther {
				t.Fatal("expired projection changed financial, stock, event, receipt or job facts")
			}
		})
	}
}
