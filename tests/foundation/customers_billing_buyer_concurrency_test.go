// customers_billing_buyer_concurrency_test.go: R1 review finding P1-1 (lock upgrade), HTTP tier.
// Concurrent consent and export requests of ONE buyer must serialize behind the owner lock (CD4), not deadlock:
// buyer.WithScope takes FOR SHARE on buyer.owners first, and a definer that then wants FOR UPDATE makes two such
// requests wait on each other (55P03 -> HTTP 503). The routes must therefore reach the definers without WithScope.
// Tier: REAL_PG through the real buyerhttp handler. Run: bash scripts/dev/test-focused.sh '^TestCustomersBillingCB04BuyerHTTPConcurrency$'

package foundation_test

import (
	"sync"
	"testing"
)

func TestCustomersBillingCB04BuyerHTTPConcurrency(t *testing.T) {
	bh := bhSetup(t)
	issued := bhRead[struct {
		Token string `json:"token"`
	}](t, bh.request(t, "POST", "/v1/buyer/session", "", "", struct{}{}, nil), 200)
	const n = 12
	statuses := make([]int, 2*n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			body := map[string]any{"purpose": "marketing_messages", "channel": "meta_dm", "granted": i%2 == 0, "context": "settings"}
			statuses[i] = bh.request(t, "PUT", "/v1/buyer/consents", issued.Token, t04Key("cbc-http-conc"), body, nil).status
		}()
		go func() {
			defer wg.Done()
			statuses[n+i] = bh.request(t, "POST", "/v1/buyer/privacy/export", issued.Token, t04Key("cbc-http-conc-x"), nil, nil).status
		}()
	}
	wg.Wait()
	for i, s := range statuses {
		if s != 200 {
			kind := "consent PUT"
			if i >= n {
				kind = "export POST"
			}
			t.Errorf("concurrent %s %d of one buyer: HTTP %d, want 200 (a lock-upgrade deadlock surfaces as 503)", kind, i%n, s)
		}
	}
}
