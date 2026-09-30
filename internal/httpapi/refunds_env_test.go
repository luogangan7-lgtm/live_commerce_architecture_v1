// refunds_env_test.go: unit tests of registerRefundRoutesIn (contracts/stripe-live-enable-v1.md §5.2, S6): the
// routes mount for SANDBOX and LIVE only, an unknown environment or a nil client mounts nothing, and
// registerRefundRoutes keeps its SANDBOX behavior. With a nil pool an admitted request is answered 401, the
// "reached the transaction" marker used by refunds_test.go. Non-goals: the not_refundable answer itself (SL04 and
// merchantorders/refunds_env_test.go) and any database. Callers: go test ./internal/httpapi.

package httpapi

import (
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

func TestRegisterRefundRoutesInMountsOnlyKnownEnvironments(t *testing.T) {
	jobs, err := river.NewClient(riverpgxv5.New(nil), &river.Config{Schema: "river_payment"})
	if err != nil {
		t.Fatal(err)
	}
	mount := func(environment string, client *river.Client[pgx.Tx]) http.Handler {
		mux := http.NewServeMux()
		registerRefundRoutesIn(mux, nil, client, environment)
		return mux
	}
	for _, environment := range []string{"SANDBOX", "LIVE"} {
		if w := refundCall(mount(environment, jobs), http.MethodPost, refundBase, refundBody, withKey); w.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status=%d, want the 401 transaction marker", environment, w.Code)
		}
	}
	for _, environment := range []string{"", "PROVIDER_MOCK", "sandbox", "live", "PRODUCTION"} {
		if w := refundCall(mount(environment, jobs), http.MethodPost, refundBase, refundBody, withKey); w.Code != http.StatusNotFound {
			t.Fatalf("environment %q mounted the routes: status=%d", environment, w.Code)
		}
	}
	if w := refundCall(mount("LIVE", nil), http.MethodPost, refundBase, refundBody, withKey); w.Code != http.StatusNotFound {
		t.Fatalf("nil client mounted the routes: status=%d", w.Code)
	}
	// The pre-LIVE signature is SANDBOX.
	mux := http.NewServeMux()
	registerRefundRoutes(mux, nil, jobs)
	if w := refundCall(mux, http.MethodPost, refundBase, refundBody, withKey); w.Code != http.StatusUnauthorized {
		t.Fatalf("registerRefundRoutes: status=%d", w.Code)
	}
}
