// refund_test.go: unit tests of the refund wire calls and projections (stripe-refund-v1 §3, §3.1) against a
// local TLS server. Non-goals: the RF01/RF02 gates (independent test unit) and the stripetest fake.
// Callers: go test ./internal/integrations/psp/stripe/...

package stripe

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

const (
	rfRefund  = "3a1f6d2e-0b7c-4c55-8d0a-6e2f9b1c4a77"
	rfPI      = "pi_test_RefundFixture01"
	rfRefundE = "re_test_RefundFixture01"
)

func refundJSON(id, status, extra string) string {
	return `{"id":"` + id + `","object":"refund","amount":5000,"currency":"hkd","status":"` + status + `",` +
		`"payment_intent":"` + rfPI + `","failure_reason":null,"pending_reason":null,` +
		`"destination_details":{"card":{"reference":"ARN_SENTINEL_0001","reference_type":"acquirer_reference_number"}},` +
		`"receipt_number":"RECEIPT_SENTINEL","metadata":{"lc_refund":"` + rfRefund + `","lc_attempt":"` + fxAttempt + `"}` + extra + `}`
}

func refundParamsFixture() RefundParams {
	return RefundParams{PaymentIntentID: rfPI, Currency: "HKD", Reason: "requested_by_customer",
		RefundRef: rfRefund, AttemptRef: fxAttempt, AmountMinor: 5000}
}

func TestRefundAmountOKMatchesTheSQLTable(t *testing.T) {
	for _, c := range []struct {
		currency string
		amount   int64
		ok       bool
	}{
		{"HKD", 1, true}, {"USD", 99_999_999, true}, {"SGD", 100_000_000, false}, {"MYR", 0, false},
		{"TWD", 100, true}, {"TWD", 150, false}, {"TWD", 99_999_900, true}, {"TWD", 100_000_000, false},
		{"TWD", 99, false}, {"JPY", 100, false}, {"hkd", 100, false}, {"", 100, false}, {"HKD", -1, false},
	} {
		if got := RefundAmountOK(c.currency, c.amount); got != c.ok {
			t.Errorf("RefundAmountOK(%q,%d)=%v want %v", c.currency, c.amount, got, c.ok)
		}
	}
}

func TestRefundBodyIsCanonicalAndClosed(t *testing.T) {
	body, err := EncodeRefundBody(refundParamsFixture())
	if err != nil {
		t.Fatal(err)
	}
	want := "amount=5000&metadata%5Blc_attempt%5D=" + fxAttempt + "&metadata%5Blc_refund%5D=" + rfRefund +
		"&payment_intent=" + rfPI + "&reason=requested_by_customer"
	if string(body) != want {
		t.Fatalf("body=%s", body)
	}
	if again, _ := EncodeRefundBody(refundParamsFixture()); string(again) != want {
		t.Fatal("resend is not byte-identical")
	}
	for _, key := range []string{"charge", "fraudulent", "reverse_transfer", "refund_application_fee", "instructions_email", "origin"} {
		if strings.Contains(string(body), key) {
			t.Errorf("forbidden key %q in body", key)
		}
	}
	for name, mutate := range map[string]func(*RefundParams){
		"fraudulent":     func(p *RefundParams) { p.Reason = "fraudulent" },
		"empty reason":   func(p *RefundParams) { p.Reason = "" },
		"zero amount":    func(p *RefundParams) { p.AmountMinor = 0 },
		"twd step":       func(p *RefundParams) { p.Currency, p.AmountMinor = "TWD", 150 },
		"bad currency":   func(p *RefundParams) { p.Currency = "JPY" },
		"bad pi":         func(p *RefundParams) { p.PaymentIntentID = "pi_bad&charge=ch_1" },
		"bad refund ref": func(p *RefundParams) { p.RefundRef = "not-a-uuid" },
		"bad attempt":    func(p *RefundParams) { p.AttemptRef = "" },
	} {
		p := refundParamsFixture()
		mutate(&p)
		if _, err := EncodeRefundBody(p); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err=%v", name, err)
		}
	}
	if RefundIdempotencyKey(rfRefund) != "lc:stripe:refund:v1:"+rfRefund {
		t.Fatal("key format")
	}
}

func TestCreateRefundSendsOneKeyAndValidatesIdentity(t *testing.T) {
	var keys, bodies []string
	c, _ := newFakeStripe(t, func(r *http.Request) fakeReply {
		raw, _ := io.ReadAll(r.Body)
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		bodies = append(bodies, string(raw))
		if r.Method != http.MethodPost || r.URL.Path != "/v1/refunds" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		return fakeReply{status: 200, body: refundJSON(rfRefundE, "pending", "")}
	})
	for i := 0; i < 2; i++ {
		r, meta, err := c.CreateRefund(context.Background(), refundParamsFixture())
		if err != nil || r.ID != rfRefundE || r.Status != "pending" || r.Amount != 5000 || r.Currency != "HKD" ||
			r.MetadataRefund != rfRefund || r.MetadataAttempt != fxAttempt || r.Livemode || meta.HTTPStatus != 200 {
			t.Fatalf("refund=%+v meta=%+v err=%v", r, meta, err)
		}
	}
	if keys[0] != keys[1] || keys[0] != RefundIdempotencyKey(rfRefund) || bodies[0] != bodies[1] {
		t.Fatalf("resend must reuse key and bytes: %v", keys)
	}
	// A 200 that is not our refund is never evidence.
	foreign, _ := newFakeStripe(t, func(*http.Request) fakeReply {
		return fakeReply{status: 200, body: strings.Replace(refundJSON(rfRefundE, "succeeded", ""), rfRefund, "00000000-0000-4000-8000-000000000000", 1)}
	})
	if _, _, err := foreign.CreateRefund(context.Background(), refundParamsFixture()); !errors.Is(err, ErrUncertain) {
		t.Fatalf("foreign metadata: %v", err)
	}
}

func TestRefundCallsClassifyPerSection56(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   string
		call   func(*Client) error
		want   error
	}{
		{"create 400 rejected", 400, `{"error":{"type":"invalid_request_error","code":"charge_already_refunded","message":"` + leakyMessage + `"}}`,
			func(c *Client) error {
				_, _, err := c.CreateRefund(context.Background(), refundParamsFixture())
				return err
			}, ErrRejected},
		{"create idempotency", 400, `{"error":{"type":"idempotency_error"}}`,
			func(c *Client) error {
				_, _, err := c.CreateRefund(context.Background(), refundParamsFixture())
				return err
			}, ErrIdempotency},
		{"create 401", 401, `{}`,
			func(c *Client) error {
				_, _, err := c.CreateRefund(context.Background(), refundParamsFixture())
				return err
			}, ErrAuthentication},
		{"create 409", 409, `{}`,
			func(c *Client) error {
				_, _, err := c.CreateRefund(context.Background(), refundParamsFixture())
				return err
			}, ErrConflict},
		{"create 429", 429, `{}`,
			func(c *Client) error {
				_, _, err := c.CreateRefund(context.Background(), refundParamsFixture())
				return err
			}, ErrRateLimited},
		{"create cached 500", 500, `{}`,
			func(c *Client) error {
				_, _, err := c.CreateRefund(context.Background(), refundParamsFixture())
				return err
			}, ErrUncertain},
		{"retrieve 404 is not absence", 404, `{}`,
			func(c *Client) error { _, _, err := c.RetrieveRefund(context.Background(), rfRefundE); return err }, ErrUncertain},
		{"retrieve 400 is uncertain", 400, `{"error":{"type":"invalid_request_error"}}`,
			func(c *Client) error { _, _, err := c.RetrieveRefund(context.Background(), rfRefundE); return err }, ErrUncertain},
		{"payment intent 404", 404, `{}`,
			func(c *Client) error { _, _, err := c.RetrievePaymentCharge(context.Background(), rfPI); return err }, ErrUncertain},
	} {
		t.Run(c.name, func(t *testing.T) {
			client, _ := newFakeStripe(t, func(*http.Request) fakeReply { return fakeReply{status: c.status, body: c.body} })
			err := c.call(client)
			if !errors.Is(err, c.want) {
				t.Fatalf("err=%v want %v", err, c.want)
			}
			if err != nil && containsAny(err.Error(), leakyMessage, "4242", "buyer@example.test") {
				t.Fatalf("error leaks a provider message: %q", err.Error())
			}
		})
	}
}

func TestRetrieveRefundRejectsWrongObjectsAndMode(t *testing.T) {
	for name, body := range map[string]string{
		"other id":        refundJSON("re_test_Other", "pending", ""),
		"unknown status":  refundJSON(rfRefundE, "refunding", ""),
		"live mode":       refundJSON(rfRefundE, "pending", `,"livemode":true`),
		"not a refund":    strings.Replace(refundJSON(rfRefundE, "pending", ""), `"object":"refund"`, `"object":"charge"`, 1),
		"duplicate key":   strings.Replace(refundJSON(rfRefundE, "pending", ""), `"amount":5000`, `"amount":5000,"amount":1`, 1),
		"negative amount": strings.Replace(refundJSON(rfRefundE, "pending", ""), `"amount":5000`, `"amount":-1`, 1),
		"bad reason":      strings.Replace(refundJSON(rfRefundE, "failed", ""), `"failure_reason":null`, `"failure_reason":"because"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := newFakeStripe(t, func(*http.Request) fakeReply { return fakeReply{status: 200, body: body} })
			if _, _, err := c.RetrieveRefund(context.Background(), rfRefundE); !errors.Is(err, ErrUncertain) {
				t.Fatalf("err=%v", err)
			}
		})
	}
	c, _ := newFakeStripe(t, func(*http.Request) fakeReply {
		return fakeReply{status: 200, body: strings.Replace(refundJSON(rfRefundE, "failed", ""), `"failure_reason":null`, `"failure_reason":"declined"`, 1)}
	})
	r, _, err := c.RetrieveRefund(context.Background(), rfRefundE)
	if err != nil || r.Status != "failed" || r.FailureReason != "declined" {
		t.Fatalf("failed refund: %+v %v", r, err)
	}
}

func TestListRefundsPagesAndCapsAtTenPages(t *testing.T) {
	var calls int32
	c, _ := newFakeStripe(t, func(r *http.Request) fakeReply {
		n := atomic.AddInt32(&calls, 1)
		if r.URL.Query().Get("payment_intent") != rfPI || r.URL.Query().Get("limit") != "100" {
			t.Errorf("query=%s", r.URL.RawQuery)
		}
		more := n < 3
		id := "re_test_Page" + string(rune('A'+n))
		return fakeReply{status: 200, body: `{"object":"list","has_more":` + map[bool]string{true: "true", false: "false"}[more] +
			`,"data":[` + refundJSON(id, "succeeded", "") + `]}`}
	})
	refunds, _, err := c.ListRefunds(context.Background(), rfPI, "")
	if err != nil || len(refunds) != 3 || calls != 3 {
		t.Fatalf("refunds=%d calls=%d err=%v", len(refunds), calls, err)
	}
	calls = 0
	endless, _ := newFakeStripe(t, func(*http.Request) fakeReply {
		atomic.AddInt32(&calls, 1)
		return fakeReply{status: 200, body: `{"object":"list","has_more":true,"data":[` + refundJSON(rfRefundE, "succeeded", "") + `]}`}
	})
	if _, _, err := endless.ListRefunds(context.Background(), rfPI, ""); !errors.Is(err, ErrUncertain) || calls != int32(maxListPages) {
		t.Fatalf("10-page cap must be ErrUncertain, never not-found: err=%v calls=%d", err, calls)
	}
	empty, _ := newFakeStripe(t, func(*http.Request) fakeReply {
		return fakeReply{status: 200, body: `{"object":"list","has_more":true,"data":[]}`}
	})
	if _, _, err := empty.ListRefunds(context.Background(), rfPI, ""); !errors.Is(err, ErrUncertain) {
		t.Fatalf("has_more with an empty page: %v", err)
	}
}

func TestRetrievePaymentChargeReadsOnlyTheChargeFields(t *testing.T) {
	body := func(latest string) string {
		return `{"id":"` + rfPI + `","object":"payment_intent","client_secret":"pi_secret_SENTINEL","latest_charge":` + latest + `}`
	}
	charge := `{"id":"ch_test_1","object":"charge","currency":"hkd","amount_captured":10000,"amount_refunded":2500,` +
		`"refunded":false,"disputed":false,"livemode":false,"receipt_url":"https://pay.stripe.com/receipts/SENTINEL","billing_details":{"email":"buyer@example.test"}}`
	var query string
	c, _ := newFakeStripe(t, func(r *http.Request) fakeReply {
		query = r.URL.RawQuery
		return fakeReply{status: 200, body: body(charge)}
	})
	got, _, err := c.RetrievePaymentCharge(context.Background(), rfPI)
	if err != nil || got != (PaymentCharge{PaymentIntentID: rfPI, ChargeID: "ch_test_1", Currency: "HKD",
		AmountCaptured: 10000, AmountRefunded: 2500}) || !strings.Contains(query, "expand%5B%5D=latest_charge") {
		t.Fatalf("charge=%+v query=%q err=%v", got, query, err)
	}
	for name, latest := range map[string]string{
		"unexpanded":  `"ch_test_1"`,
		"null":        `null`,
		"live":        strings.Replace(charge, `"livemode":false`, `"livemode":true`, 1),
		"no captured": strings.Replace(charge, `"amount_captured":10000,`, ``, 1),
		"not charge":  strings.Replace(charge, `"object":"charge"`, `"object":"refund"`, 1),
	} {
		bad, _ := newFakeStripe(t, func(*http.Request) fakeReply { return fakeReply{status: 200, body: body(latest)} })
		if _, _, err := bad.RetrievePaymentCharge(context.Background(), rfPI); !errors.Is(err, ErrUncertain) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestRefundObservationsHaveExactKeysAndNoSensitiveData(t *testing.T) {
	c, _ := newFakeStripe(t, func(*http.Request) fakeReply {
		return fakeReply{status: 200, body: refundJSON(rfRefundE, "pending", ""), headers: map[string]string{"Request-Id": "req_test_1"}}
	})
	refund, meta, err := c.RetrieveRefund(context.Background(), rfRefundE)
	if err != nil {
		t.Fatal(err)
	}
	obs := refund.Observation("retrieve", meta, fakeAccount, 3, 9, rfRefund, fxAttempt)
	raw, _ := json.Marshal(obs)
	var keys map[string]json.RawMessage
	if json.Unmarshal(raw, &keys) != nil || len(keys) != 25 || len(raw) > 2048 {
		t.Fatalf("refund report keys=%d bytes=%d: %s", len(keys), len(raw), raw)
	}
	for _, k := range []string{"Provider", "Version", "Object", "Via", "AccountID", "KeyVersion", "RequestID", "SendCount",
		"RefundRef", "AttemptRef", "RefundID", "Status", "FailureReason", "PendingReason", "Amount", "Currency",
		"PaymentIntentID", "Livemode", "MetadataRefund", "MetadataAttempt", "ErrorClass", "ErrorCode", "HTTPStatus",
		"ListMatchCount", "LocalReason"} {
		if _, ok := keys[k]; !ok {
			t.Errorf("missing key %s", k)
		}
	}
	if obs.SendCount != 0 || obs.RequestID != "req_test_1" || obs.Object != "refund" || obs.Amount == nil || *obs.Amount != 5000 ||
		containsAny(string(raw), "ARN_SENTINEL", "RECEIPT_SENTINEL", "acquirer_reference_number") {
		t.Fatalf("unexpected report: %s", raw)
	}
	if o := refund.Observation("create", meta, fakeAccount, 3, 2, rfRefund, fxAttempt); o.SendCount != 2 {
		t.Fatal("create keeps SendCount")
	}
	if o := refund.Observation("bogus", meta, fakeAccount, 3, 0, rfRefund, fxAttempt); o.Via != "" {
		t.Fatal("unknown via must be empty so SQL rejects it")
	}
	// An identity-less report: amount is null and a list without a match says zero.
	none := (Refund{}).Observation("list", CallMeta{HTTPStatus: 200}, fakeAccount, 3, 0, rfRefund, fxAttempt)
	if none.Amount != nil || none.ListMatchCount == nil || *none.ListMatchCount != 0 || none.RefundID != "" {
		t.Fatalf("none=%+v", none)
	}
	rejected := (Refund{}).Observation("create", CallMeta{HTTPStatus: 400, ErrorType: "invalid_request_error", ErrorCode: "charge_already_refunded"}, fakeAccount, 3, 1, rfRefund, fxAttempt)
	if rejected.ErrorClass != "rejected" || rejected.ErrorCode != "charge_already_refunded" {
		t.Fatalf("rejected=%+v", rejected)
	}
	cobs := PaymentCharge{PaymentIntentID: rfPI, ChargeID: "ch_test_1", Currency: "HKD", AmountCaptured: 10000, AmountRefunded: 100}.
		Observation("presend", meta, fakeAccount, 3, rfRefund)
	craw, _ := json.Marshal(cobs)
	var ckeys map[string]json.RawMessage
	if json.Unmarshal(craw, &ckeys) != nil || len(ckeys) != 17 || cobs.RefundRef != rfRefund {
		t.Fatalf("charge report keys=%d: %s", len(ckeys), craw)
	}
	if o := (PaymentCharge{}).Observation("retrieve", meta, fakeAccount, 3, rfRefund); o.RefundRef != "" {
		t.Fatal("retrieve carries no refund ref")
	}
	if o := (PaymentCharge{}).Observation("nope", meta, fakeAccount, 3, ""); o.Via != "" {
		t.Fatal("unknown charge via must be empty")
	}
}

func TestRefundValuesAreRedactedInEveryFormat(t *testing.T) {
	r := Refund{ID: rfRefundE, Amount: 5000}
	for _, s := range []string{r.String(), r.GoString(), (PaymentCharge{ChargeID: "ch_test_1"}).String()} {
		if strings.Contains(s, "re_test") || strings.Contains(s, "5000") || strings.Contains(s, "ch_test") {
			t.Fatalf("value leaked: %s", s)
		}
	}
	if raw, _ := json.Marshal(r); strings.Contains(string(raw), "re_test") {
		t.Fatalf("json leaked: %s", raw)
	}
}
