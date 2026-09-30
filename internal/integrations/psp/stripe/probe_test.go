// probe_test.go: golden-body and call-sequence unit tests for ProbeCheckout (ruling 7).
// Non-goal: no network and no SANDBOX run; the real probe is NOT_RUN without STRIPE_SANDBOX=1.
// Callers: go test ./internal/integrations/psp/stripe/...

package stripe

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

const probeQualification = "5c1e4b2a-9d3f-4a7e-8b60-1f2e3d4c5b6a"

// goldenProbeBody is written by hand (sorted keys, QueryEscape) for now=1789995000
// (expires_at = now + 31 min = 1789996860).
const goldenProbeBody = "adaptive_pricing%5Benabled%5D=false" +
	"&automatic_tax%5Benabled%5D=false" +
	"&cancel_url=https%3A%2F%2Fshop.example.test%2Fpayment%2Freturn" +
	"&client_reference_id=" + probeQualification +
	"&expires_at=1789996860" +
	"&line_items%5B0%5D%5Bprice_data%5D%5Bcurrency%5D=hkd" +
	"&line_items%5B0%5D%5Bprice_data%5D%5Bproduct_data%5D%5Bname%5D=Stripe+probe" +
	"&line_items%5B0%5D%5Bprice_data%5D%5Bunit_amount%5D=400" +
	"&line_items%5B0%5D%5Bquantity%5D=1" +
	"&managed_payments%5Benabled%5D=false" +
	"&metadata%5Blc_probe%5D=1" +
	"&mode=payment" +
	"&payment_method_types%5B0%5D=card" +
	"&submit_type=pay" +
	"&success_url=https%3A%2F%2Fshop.example.test%2Fpayment%2Freturn" +
	"&ui_mode=hosted_page"

func probeSession(status string) string {
	return `{"id":"cs_test_probe1","object":"checkout.session","livemode":false,"status":"` + status +
		`","payment_status":"unpaid","currency":"hkd","amount_total":400,"amount_subtotal":400,` +
		`"total_details":null,"presentment_details":null,"currency_conversion":null,` +
		`"client_reference_id":"` + probeQualification + `","metadata":{"lc_probe":"1"},` +
		`"expires_at":1789996860,"created":1789995000,"mode":"payment","payment_method_types":["card"],` +
		`"payment_intent":null,"url":null}`
}

type probeCall struct{ method, path, key, body string }

func probeClient(t *testing.T, finalStatus string, calls *[]probeCall) *Client {
	t.Helper()
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b := ""
		if r.Body != nil {
			raw, _ := io.ReadAll(r.Body)
			b = string(raw)
		}
		*calls = append(*calls, probeCall{r.Method, r.URL.Path, r.Header.Get("Idempotency-Key"), b})
		reply := probeSession("open")
		if r.Method == http.MethodGet || strings.HasSuffix(r.URL.Path, "/expire") {
			reply = probeSession(finalStatus)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(reply)),
			Header: http.Header{"Content-Type": {"application/json"}}}, nil
	})
	c, err := NewWithMockTransport(sandboxConfig(), rt)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestProbeCheckoutSequenceAndGoldenBody(t *testing.T) {
	var calls []probeCall
	c := probeClient(t, "expired", &calls)
	id, _, err := c.probeCheckout(context.Background(), time.Unix(1789995000, 0), probeQualification,
		"HKD", 400, fxReturnURL)
	if err != nil || id != "cs_test_probe1" {
		t.Fatalf("probe failed: %q %v", id, err)
	}
	if len(calls) != 3 {
		t.Fatalf("want create, expire, retrieve; got %d calls", len(calls))
	}
	if calls[0].method != "POST" || calls[0].path != "/v1/checkout/sessions" ||
		calls[0].key != "lc:stripe:probe:v1:"+probeQualification || calls[0].body != goldenProbeBody {
		t.Fatalf("create call mismatch:\n got %+v\nwant body %s", calls[0], goldenProbeBody)
	}
	if calls[1].path != "/v1/checkout/sessions/cs_test_probe1/expire" || calls[1].body != "" ||
		calls[1].key != "lc:stripe:probe-expire:v1:"+probeQualification {
		t.Fatalf("expire call mismatch: %+v", calls[1])
	}
	if calls[2].method != "GET" || !strings.HasPrefix(calls[2].path, "/v1/checkout/sessions/cs_test_probe1") {
		t.Fatalf("retrieve call mismatch: %+v", calls[2])
	}
}

func TestProbeCheckoutRefusals(t *testing.T) {
	var calls []probeCall
	// A session that is still open after the expire call is not qualification evidence.
	c := probeClient(t, "open", &calls)
	if _, _, err := c.probeCheckout(context.Background(), time.Unix(1789995000, 0), probeQualification,
		"HKD", 400, fxReturnURL); !errors.Is(err, ErrRejected) {
		t.Fatalf("unexpired probe accepted: %v", err)
	}
	calls = nil
	for _, bad := range []struct {
		q, cur, url string
		amt         int64
	}{
		{"not-a-uuid", "HKD", fxReturnURL, 400},
		{probeQualification, "JPY", fxReturnURL, 400},
		{probeQualification, "HKD", fxReturnURL, 399},
		{probeQualification, "HKD", "http://shop.example.test/x", 400},
	} {
		if _, _, err := c.probeCheckout(context.Background(), time.Unix(1789995000, 0), bad.q, bad.cur,
			bad.amt, bad.url); !errors.Is(err, ErrInvalid) {
			t.Fatalf("bad input %+v accepted: %v", bad, err)
		}
	}
	if len(calls) != 0 {
		t.Fatal("invalid probe input reached the network")
	}
	// stripe-live-core: a LIVE client is no longer refused by ProbeCheckout itself (it only
	// exists after admit() accepted the pair); the LIVE probe rules are asserted in
	// live_test.go (TestStripeSL01LiveConfig).
}
