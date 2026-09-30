// classify_test.go: SP04 — every §5.6 row against a local httptest TLS server, plus the
// transport policy (TLS, redirects, cookies, cancel), list pagination and the §5.7
// projection bounds. Non-goal: this is not the independent stripetest fake (test_worker).
// Callers: go test ./internal/integrations/psp/stripe/...

package stripe

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

type fakeReply struct {
	status  int
	body    string
	headers map[string]string
}

// newFakeStripe starts a TLS server and a mock-transport client whose requests to
// https://api.stripe.com are rewritten to it. handler picks the reply per request.
func newFakeStripe(t *testing.T, handler func(*http.Request) fakeReply) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rep := handler(r)
		for k, v := range rep.headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(rep.status)
		_, _ = w.Write([]byte(rep.body))
	}))
	t.Cleanup(srv.Close)
	target, _ := url.Parse(srv.URL)
	inner := srv.Client().Transport
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.stripe.com" || r.URL.Scheme != "https" {
			t.Errorf("client dialled %s", r.URL.Host)
		}
		r2 := r.Clone(r.Context())
		r2.URL.Host = target.Host
		r2.Host = target.Host
		return inner.RoundTrip(r2)
	})
	c, err := NewWithMockTransport(sandboxConfig(), rt)
	if err != nil {
		t.Fatal(err)
	}
	return c, srv
}

const leakyMessage = "SENTINEL_STRIPE_MESSAGE card 4242 buyer@example.test"

func stripeError(typ, code string) string {
	return `{"error":{"type":"` + typ + `","code":"` + code + `","message":"` + leakyMessage + `"}}`
}

func TestStripeSP04Classify(t *testing.T) {
	ctx := t.Context()
	f := false
	rows := []struct {
		name   string
		op     string // create|retrieve|expire
		reply  fakeReply
		want   error
		checks func(*testing.T, CallMeta)
	}{
		{"200 create", "create", fakeReply{200, sessionJSON("cs_test_1", "open", "unpaid", false), map[string]string{"Request-Id": "req_abc123"}}, nil,
			func(t *testing.T, m CallMeta) {
				if m.RequestID != "req_abc123" || m.HTTPStatus != 200 {
					t.Fatalf("meta %d %s", m.HTTPStatus, m.RequestID)
				}
			}},
		{"200 retrieve", "retrieve", fakeReply{200, sessionJSON("cs_test_1", "complete", "paid", false), nil}, nil, nil},
		{"200 expire", "expire", fakeReply{200, sessionJSON("cs_test_1", "expired", "unpaid", false), nil}, nil, nil},
		{"400 invalid create", "create", fakeReply{400, stripeError("invalid_request_error", "parameter_invalid_integer"), nil}, ErrRejected,
			func(t *testing.T, m CallMeta) {
				if m.ErrorType != "invalid_request_error" || m.ErrorCode != "parameter_invalid_integer" {
					t.Fatalf("error words %q %q", m.ErrorType, m.ErrorCode)
				}
			}},
		{"404 create", "create", fakeReply{404, stripeError("invalid_request_error", "resource_missing"), nil}, ErrRejected, nil},
		{"404 retrieve", "retrieve", fakeReply{404, stripeError("invalid_request_error", "resource_missing"), nil}, ErrUncertain, nil},
		{"400 retrieve", "retrieve", fakeReply{400, stripeError("invalid_request_error", ""), nil}, ErrUncertain, nil},
		{"400 expire not open", "expire", fakeReply{400, stripeError("invalid_request_error", ""), nil}, ErrNotOpen, nil},
		{"400 idempotency create", "create", fakeReply{400, stripeError("idempotency_error", ""), nil}, ErrIdempotency, nil},
		{"400 unknown type create", "create", fakeReply{400, stripeError("api_error", ""), nil}, ErrUncertain, nil},
		{"400 unparseable create", "create", fakeReply{400, "not json", nil}, ErrUncertain, nil},
		{"401 create", "create", fakeReply{401, stripeError("invalid_request_error", ""), nil}, ErrAuthentication, nil},
		{"403 retrieve", "retrieve", fakeReply{403, stripeError("invalid_request_error", ""), nil}, ErrAuthentication, nil},
		{"409 create", "create", fakeReply{409, stripeError("idempotency_error", ""), nil}, ErrConflict, nil},
		{"429 create", "create", fakeReply{429, stripeError("invalid_request_error", "rate_limit"), nil}, ErrRateLimited, nil},
		{"500 replayed create", "create", fakeReply{500, stripeError("api_error", ""), map[string]string{"Idempotent-Replayed": "true", "Stripe-Should-Retry": "false"}}, ErrUncertain,
			func(t *testing.T, m CallMeta) {
				if !m.IdempotentReplayed || m.ShouldRetry == nil || *m.ShouldRetry != f {
					t.Fatal("replay / should-retry evidence lost")
				}
			}},
		{"503 expire", "expire", fakeReply{503, "", map[string]string{"Stripe-Should-Retry": "true"}}, ErrUncertain,
			func(t *testing.T, m CallMeta) {
				if m.ShouldRetry == nil || !*m.ShouldRetry {
					t.Fatal("should-retry true lost")
				}
			}},
		{"302 redirect", "create", fakeReply{302, "", map[string]string{"Location": "https://api.stripe.com/v1/redirected"}}, ErrUncertain, nil},
		{"418 unknown", "retrieve", fakeReply{418, "", nil}, ErrUncertain, nil},
		{"oversize", "retrieve", fakeReply{200, `{"pad":"` + strings.Repeat("a", maxResponseBody) + `"}`, nil}, ErrUncertain, nil},
		{"duplicate key", "retrieve", fakeReply{200, strings.Replace(sessionJSON("cs_test_1", "open", "unpaid", false), `"status":"open"`, `"status":"open","status":"complete"`, 1), nil}, ErrUncertain, nil},
		{"malformed json", "retrieve", fakeReply{200, `{"id":"cs_test_1"`, nil}, ErrUncertain, nil},
		{"trailing data", "retrieve", fakeReply{200, sessionJSON("cs_test_1", "open", "unpaid", false) + "{}", nil}, ErrUncertain, nil},
		{"wrong object", "retrieve", fakeReply{200, strings.Replace(sessionJSON("cs_test_1", "open", "unpaid", false), `"object":"checkout.session"`, `"object":"payment_intent"`, 1), nil}, ErrUncertain, nil},
		{"livemode mismatch", "retrieve", fakeReply{200, sessionJSON("cs_test_1", "open", "unpaid", true), nil}, ErrUncertain, nil},
		{"missing status", "retrieve", fakeReply{200, strings.Replace(sessionJSON("cs_test_1", "open", "unpaid", false), `"status":"open",`, "", 1), nil}, ErrUncertain, nil},
		{"unknown status", "retrieve", fakeReply{200, sessionJSON("cs_test_1", "processing", "unpaid", false), nil}, ErrUncertain, nil},
		{"negative amount", "retrieve", fakeReply{200, strings.Replace(sessionJSON("cs_test_1", "open", "unpaid", false), `"amount_total":12345`, `"amount_total":-1`, 1), nil}, ErrUncertain, nil},
		{"fractional amount", "retrieve", fakeReply{200, strings.Replace(sessionJSON("cs_test_1", "open", "unpaid", false), `"amount_total":12345`, `"amount_total":123.45`, 1), nil}, ErrUncertain, nil},
		{"id mismatch retrieve", "retrieve", fakeReply{200, sessionJSON("cs_test_other", "open", "unpaid", false), nil}, ErrUncertain, nil},
		{"create for other attempt", "create", fakeReply{200, strings.ReplaceAll(sessionJSON("cs_test_1", "open", "unpaid", false), fxAttempt, fxOrder), nil}, ErrUncertain, nil},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			var hits atomic.Int32
			c, _ := newFakeStripe(t, func(r *http.Request) fakeReply {
				hits.Add(1)
				if r.URL.Path == "/v1/redirected" {
					t.Error("redirect was followed")
				}
				return row.reply
			})
			var (
				s   Session
				m   CallMeta
				err error
			)
			switch row.op {
			case "create":
				s, m, err = c.CreateCheckoutSession(ctx, fixtureParams())
			case "retrieve":
				s, m, err = c.RetrieveCheckoutSession(ctx, "cs_test_1")
			case "expire":
				s, m, err = c.ExpireCheckoutSession(ctx, "cs_test_1", ExpireIdempotencyKey(fxAttempt, 1))
			}
			if row.want == nil {
				if err != nil || s.ID != "cs_test_1" {
					t.Fatalf("want ok, got %v", err)
				}
			} else if !errors.Is(err, row.want) {
				t.Fatalf("want %v, got %v", row.want, err)
			}
			if err != nil && s.ID != "" {
				t.Fatal("session returned with an error")
			}
			if err != nil && strings.Contains(err.Error(), "SENTINEL") {
				t.Fatal("error leaks Stripe message")
			}
			if b, _ := json.Marshal(s.Observation(row.op, m, fakeAccount, 1, 1)); strings.Contains(string(b), "SENTINEL") {
				t.Fatal("observation leaks Stripe message or URL")
			}
			if hits.Load() != 1 {
				t.Fatalf("expected exactly one request (no retries), got %d", hits.Load())
			}
			if row.checks != nil {
				row.checks(t, m)
			}
		})
	}

	t.Run("expire request shape", func(t *testing.T) {
		c, _ := newFakeStripe(t, func(r *http.Request) fakeReply {
			if r.Method != http.MethodPost || r.URL.Path != "/v1/checkout/sessions/cs_test_1/expire" ||
				r.ContentLength != 0 || r.Header.Get("Idempotency-Key") != "lc:stripe:cs-expire:v1:"+fxAttempt+":3" {
				t.Errorf("bad expire request %s %s", r.Method, r.URL.Path)
			}
			return fakeReply{200, sessionJSON("cs_test_1", "expired", "unpaid", false), nil}
		})
		if _, _, err := c.ExpireCheckoutSession(ctx, "cs_test_1", ExpireIdempotencyKey(fxAttempt, 3)); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"", "lc:stripe:cs-create:v1:" + fxAttempt, "other"} {
			if _, _, err := c.ExpireCheckoutSession(ctx, "cs_test_1", k); !errors.Is(err, ErrInvalid) {
				t.Fatalf("expire key %q admitted", k)
			}
		}
	})

	t.Run("retrieve request shape and PI projection", func(t *testing.T) {
		body := strings.Replace(sessionJSON("cs_test_1", "complete", "paid", false), `"payment_intent":null`,
			`"payment_intent":{"id":"pi_1","object":"payment_intent","status":"succeeded","amount_received":12345,"currency":"hkd","client_secret":"pi_1_secret_SENTINEL"}`, 1)
		c, _ := newFakeStripe(t, func(r *http.Request) fakeReply {
			if r.Method != http.MethodGet || r.URL.Path != "/v1/checkout/sessions/cs_test_1" ||
				r.URL.RawQuery != "expand%5B%5D=payment_intent" || r.Header.Get("Idempotency-Key") != "" ||
				r.Header.Get("Content-Type") != "" {
				t.Errorf("bad retrieve request %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			}
			return fakeReply{200, body, nil}
		})
		s, m, err := c.RetrieveCheckoutSession(ctx, "cs_test_1")
		if err != nil || s.PaymentIntentID != "pi_1" || s.PaymentIntentStatus != "succeeded" ||
			s.PaymentIntentAmountReceived == nil || *s.PaymentIntentAmountReceived != 12345 || s.PaymentIntentCurrency != "HKD" {
			t.Fatalf("pi projection: %v", err)
		}
		b, _ := json.Marshal(s.Observation("retrieve", m, fakeAccount, 1, 5))
		if containsAny(string(b), "SENTINEL", "buyer@example.test", "checkout.stripe.com") {
			t.Fatalf("observation leaks: %s", b)
		}
		for _, bad := range []string{"", "cs/../x", "cs_test_1?x=1", strings.Repeat("a", 256)} {
			if _, _, err := c.RetrieveCheckoutSession(ctx, bad); !errors.Is(err, ErrInvalid) {
				t.Fatalf("session id %q admitted", bad)
			}
		}
	})

	t.Run("cookies are never stored or sent", func(t *testing.T) {
		c, _ := newFakeStripe(t, func(r *http.Request) fakeReply {
			if r.Header.Get("Cookie") != "" {
				t.Error("cookie sent")
			}
			return fakeReply{200, sessionJSON("cs_test_1", "open", "unpaid", false), map[string]string{"Set-Cookie": "sid=SENTINEL; Path=/"}}
		})
		for i := 0; i < 2; i++ {
			if _, _, err := c.RetrieveCheckoutSession(ctx, "cs_test_1"); err != nil {
				t.Fatal(err)
			}
		}
		if c.httpClient.Jar != nil {
			t.Fatal("cookie jar configured")
		}
	})

	t.Run("context cancel", func(t *testing.T) {
		c, _ := newFakeStripe(t, func(r *http.Request) fakeReply {
			return fakeReply{200, sessionJSON("cs_test_1", "open", "unpaid", false), nil}
		})
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if _, _, err := c.RetrieveCheckoutSession(cctx, "cs_test_1"); !errors.Is(err, ErrUncertain) {
			t.Fatalf("cancelled: %v", err)
		}
		//nolint:staticcheck // a nil context is a caller bug and must not panic.
		if _, _, err := c.RetrieveCheckoutSession(nil, "cs_test_1"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("nil ctx: %v", err)
		}
	})

	t.Run("TLS policy", func(t *testing.T) {
		real, err := New(sandboxConfig())
		if err != nil {
			t.Fatal(err)
		}
		tr, ok := real.httpClient.Transport.(*http.Transport)
		if !ok || tr.TLSClientConfig == nil || tr.TLSClientConfig.MinVersion != tls.VersionTLS12 ||
			tr.TLSClientConfig.InsecureSkipVerify || real.httpClient.Jar != nil || real.httpClient.CheckRedirect == nil ||
			real.httpClient.Timeout != callTimeout {
			t.Fatal("production transport policy violated")
		}
		// An untrusted certificate is a transport failure: ErrUncertain, never success.
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("request reached an untrusted server")
		}))
		defer srv.Close()
		target, _ := url.Parse(srv.URL)
		verifying := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
		rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			r2 := r.Clone(r.Context())
			r2.URL.Host = target.Host
			return verifying.RoundTrip(r2)
		})
		c, _ := NewWithMockTransport(sandboxConfig(), rt)
		if _, _, err := c.RetrieveCheckoutSession(ctx, "cs_test_1"); !errors.Is(err, ErrUncertain) {
			t.Fatalf("bad TLS: %v", err)
		}
	})

	t.Run("VerifyAccount", func(t *testing.T) {
		for _, tc := range []struct {
			body string
			want error
		}{
			{`{"id":"` + fakeAccount + `","object":"account"}`, nil},
			{`{"id":"acct_1Other","object":"account"}`, ErrAuthentication},
			{`{"id":"` + fakeAccount + `","object":"customer"}`, ErrUncertain},
			{`{"id":"` + fakeAccount + `","id":"acct_1Other","object":"account"}`, ErrUncertain},
		} {
			c, _ := newFakeStripe(t, func(r *http.Request) fakeReply {
				if r.Method != http.MethodGet || r.URL.Path != "/v1/account" {
					t.Errorf("bad account request %s", r.URL.Path)
				}
				return fakeReply{200, tc.body, nil}
			})
			_, err := c.VerifyAccount(ctx)
			if (tc.want == nil && err != nil) || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("VerifyAccount %s: %v", tc.body, err)
			}
		}
	})

	t.Run("FindCheckoutSessions", func(t *testing.T) {
		// 250 sessions over 3 pages; two match the attempt, one only by client_reference_id.
		all := make([]string, 0, 250)
		for i := 0; i < 250; i++ {
			s := strings.ReplaceAll(sessionJSON("cs_test_"+strconv.Itoa(i), "expired", "unpaid", false), fxAttempt, "11111111-1111-4111-8111-111111111111")
			all = append(all, s)
		}
		all[5] = sessionJSON("cs_test_5", "expired", "unpaid", false)
		all[180] = sessionJSON("cs_test_180", "open", "unpaid", false)
		all[200] = strings.Replace(all[200], `"client_reference_id":"11111111-1111-4111-8111-111111111111"`, `"client_reference_id":"`+fxAttempt+`"`, 1)
		var pages atomic.Int32
		c, _ := newFakeStripe(t, func(r *http.Request) fakeReply {
			pages.Add(1)
			q := r.URL.Query()
			if q.Get("created[gte]") != "1000" || q.Get("created[lte]") != "2000" || q.Get("limit") != "100" {
				t.Errorf("bad list query %s", r.URL.RawQuery)
			}
			start := 0
			if after := q.Get("starting_after"); after != "" {
				n, _ := strconv.Atoi(strings.TrimPrefix(after, "cs_test_"))
				start = n + 1
			}
			end := min(start+100, len(all))
			return fakeReply{200, `{"object":"list","url":"/v1/checkout/sessions","has_more":` +
				strconv.FormatBool(end < len(all)) + `,"data":[` + strings.Join(all[start:end], ",") + `]}`, nil}
		})
		got, _, err := c.FindCheckoutSessions(ctx, fxAttempt, 1000, 2000)
		if err != nil || pages.Load() != 3 {
			t.Fatalf("find: %v pages=%d", err, pages.Load())
		}
		ids := []string{}
		for _, s := range got {
			ids = append(ids, s.ID)
		}
		sort.Strings(ids)
		if strings.Join(ids, ",") != "cs_test_180,cs_test_5" {
			t.Fatalf("matches %v", ids)
		}
		for _, bad := range [][2]int64{{0, 10}, {10, 5}, {1, maxUnixSeconds + 1}} {
			if _, _, err := c.FindCheckoutSessions(ctx, fxAttempt, bad[0], bad[1]); !errors.Is(err, ErrInvalid) {
				t.Fatalf("window %v admitted", bad)
			}
		}
		if _, _, err := c.FindCheckoutSessions(ctx, "not-a-uuid", 1, 2); !errors.Is(err, ErrInvalid) {
			t.Fatal("bad attempt admitted")
		}
	})

	t.Run("FindCheckoutSessions page cap is uncertain", func(t *testing.T) {
		var n atomic.Int32
		c, _ := newFakeStripe(t, func(r *http.Request) fakeReply {
			i := n.Add(1)
			return fakeReply{200, `{"object":"list","has_more":true,"data":[` +
				sessionJSON("cs_test_p"+strconv.Itoa(int(i)), "expired", "unpaid", false) + `]}`, nil}
		})
		if _, _, err := c.FindCheckoutSessions(ctx, fxAttempt, 1, 2); !errors.Is(err, ErrUncertain) || n.Load() != 10 {
			t.Fatalf("page cap: %v pages=%d", err, n.Load())
		}
	})

	t.Run("observation projection", func(t *testing.T) {
		big := int64(99_999_999_999)
		long := strings.Repeat("A", 255)
		s := Session{ID: long, Status: "complete", PaymentStatus: "no_payment_required", Currency: "HKD",
			AmountTotal: &big, AmountSubtotal: &big, AmountDiscount: &big, AmountTax: &big, AmountShipping: &big,
			PresentmentCurrency: "USD", PresentmentAmount: &big, CurrencyConversion: true,
			ClientReferenceID: strings.Repeat("r", 64), MetadataAttempt: strings.Repeat("a", 64), MetadataProfile: strings.Repeat("p", 64),
			ExpiresAt: &big, Created: &big, Mode: strings.Repeat("m", 32),
			PaymentMethodTypes: []string{strings.Repeat("x", 32), strings.Repeat("y", 32), strings.Repeat("z", 32), strings.Repeat("w", 32),
				strings.Repeat("v", 32), strings.Repeat("u", 32), strings.Repeat("t", 32), strings.Repeat("s", 32)},
			PaymentIntentID: long, PaymentIntentStatus: strings.Repeat("q", 32), PaymentIntentAmountReceived: &big, PaymentIntentCurrency: "HKD",
			url: fxSessionURL}
		meta := CallMeta{HTTPStatus: 404, RequestID: strings.Repeat("R", 64), ErrorType: "invalid_request_error", ErrorCode: strings.Repeat("e", 64)}
		o := s.Observation("list", meta, "acct_"+strings.Repeat("9", 59), 1<<62, 99)
		b, _ := json.Marshal(o)
		if len(b) > 2048 {
			t.Fatalf("worst-case projection is %d bytes > 2048", len(b))
		}
		if strings.Contains(string(b), "SENTINELURL") {
			t.Fatal("URL in projection")
		}
		var keys map[string]json.RawMessage
		_ = json.Unmarshal(b, &keys)
		want := "AccountID AmountDiscount AmountShipping AmountSubtotal AmountTax AmountTotal ClientReferenceID Created Currency CurrencyConversion ErrorClass ErrorCode ExpiresAt HTTPStatus KeyVersion ListMatchCount Livemode LocalReason MetadataAttempt MetadataProfile Mode PaymentIntentAmountReceived PaymentIntentCurrency PaymentIntentID PaymentIntentStatus PaymentMethodTypes PaymentStatus PresentmentAmount PresentmentCurrency Provider RequestID SendCount SessionID Status Version Via"
		got := make([]string, 0, len(keys))
		for k := range keys {
			got = append(got, k)
		}
		sort.Strings(got)
		if strings.Join(got, " ") != want {
			t.Fatalf("keys %v", got)
		}
		if o.SendCount != 0 || o.ErrorClass != "rejected" || o.ListMatchCount == nil || *o.ListMatchCount != 1 || o.Provider != "stripe" || o.Version != 1 {
			t.Fatalf("projection fields %+v", o.ErrorClass)
		}
		empty, _ := json.Marshal(Session{}.Observation("bogus", CallMeta{}, fakeAccount, 0, 3))
		if !strings.Contains(string(empty), `"Via":""`) || !strings.Contains(string(empty), `"PaymentMethodTypes":[]`) ||
			!strings.Contains(string(empty), `"AmountTotal":null`) || !strings.Contains(string(empty), `"ListMatchCount":null`) {
			t.Fatalf("empty projection %s", empty)
		}
		created := Session{}.Observation("create", CallMeta{HTTPStatus: 500}, fakeAccount, 2, 3)
		if created.SendCount != 3 || created.ErrorClass != "" {
			t.Fatal("create send count / class")
		}
	})
}
