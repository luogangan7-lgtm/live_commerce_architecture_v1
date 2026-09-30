package buyerhttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"livecommerce/internal/checkout"
	"livecommerce/internal/command"
)

const paymentOrderID = "00000000-0000-0000-0000-000000000001"

func TestBuyerPaymentRoutesAndOneShotInput(t *testing.T) {
	path := "/v1/buyer/orders/" + paymentOrderID + "/payment"
	for _, tc := range []struct {
		suffix, method string
		kind           routeKind
	}{
		{"", http.MethodGet, paymentRoute},
		{"/prepare", http.MethodPost, paymentPrepareRoute},
		{"/handoff", http.MethodPost, paymentHandoffRoute},
	} {
		selected := matchRoute(path + tc.suffix)
		if selected.kind != tc.kind || selected.id != paymentOrderID || !allowed(selected.kind, tc.method) {
			t.Fatalf("payment route drift: %s %s", tc.method, path+tc.suffix)
		}
		wrong := http.MethodGet
		if tc.method == wrong {
			wrong = http.MethodPost
		}
		if allowed(selected.kind, wrong) {
			t.Fatal("payment route accepted wrong method")
		}
	}
	for _, malformed := range []string{path + "/handoff/", path + "/other", path + "/prepare/extra",
		"/v1/buyer/orders/a/b/payment/handoff"} {
		if matchRoute(malformed).kind != unknownRoute {
			t.Fatalf("malformed route accepted: %s", malformed)
		}
	}
	handoff := httptest.NewRequest(http.MethodPost, path+"/handoff", nil)
	if key, ok := keyFor(handoff, true, false); !ok || key != "" {
		t.Fatal("handoff required a replay key")
	}
	handoff.Header.Set("Idempotency-Key", "valid-key-1")
	if _, ok := keyFor(handoff, true, false); ok {
		t.Fatal("handoff accepted a replay key")
	}
	if !forbiddenInput(httptest.NewRequest(http.MethodGet, path+"?x=1", nil)) {
		t.Fatal("payment view accepted a query")
	}
	if err := noBody(httptest.NewRequest(http.MethodPost, path+"/handoff", strings.NewReader("{}"))); err == nil {
		t.Fatal("handoff accepted a body")
	}
	if _, err := New(context.Background(), nil, nil, nil, testBuyerKey, time.Hour, nil, nil); !errors.Is(err, command.ErrInvalid) {
		t.Fatalf("multiple hosted services accepted: %v", err)
	}
}

func TestBuyerPaymentPrepareStrictProjection(t *testing.T) {
	for _, body := range []string{
		`{"method_code":"payuni_credit","method_version":1}`,
		`{"method_code":"payuni_credit","method_version":1,"locale":"en","locale":"zh-TW"}`,
		`{"method_code":"payuni_credit","method_version":1,"locale":"en","amount_minor":100}`,
		`{"method_code":"payuni_credit","method_version":null,"locale":"en"}`,
	} {
		var input paymentPrepareInput
		if err := json.Unmarshal([]byte(body), &input); err == nil {
			t.Fatalf("non-exact prepare body accepted: %s", body)
		}
	}
	var input paymentPrepareInput
	if err := json.Unmarshal([]byte(`{"method_code":"payuni_credit","method_version":1,"locale":"zh-CN"}`), &input); err != nil ||
		input.MethodCode != "payuni_credit" || input.MethodVersion != 1 || input.Locale != "zh-CN" {
		t.Fatalf("valid prepare body rejected: %+v, %v", input, err)
	}
	body, err := json.Marshal(paymentPrepareResponse{OrderID: paymentOrderID, State: "PAYMENT_PENDING", Currency: "TWD", AmountMinor: 100})
	if err != nil || string(body) != `{"order_id":"`+paymentOrderID+`","state":"PAYMENT_PENDING","currency":"TWD","amount_minor":100}` {
		t.Fatalf("prepare leaked private fields or changed shape: %s, %v", body, err)
	}
	h := &handler{}
	selected := matchRoute("/v1/buyer/orders/" + paymentOrderID + "/payment/handoff")
	err = h.dispatch(context.Background(), httptest.NewRecorder(), httptest.NewRequest(http.MethodPost,
		"/v1/buyer/orders/"+paymentOrderID+"/payment/handoff", nil), selected, paymentOrderID, testBuyerKey, "")
	status, code := classify(err)
	if status != http.StatusNotFound || code != "not_found" {
		t.Fatalf("disabled payment service exposed route: %d %s", status, code)
	}
	h.payment = &checkout.HostedPaymentStarter{}
	err = h.dispatch(context.Background(), httptest.NewRecorder(), httptest.NewRequest(http.MethodPost,
		"/v1/buyer/orders/"+paymentOrderID+"/payment/handoff", strings.NewReader("{}")), selected,
		paymentOrderID, testBuyerKey, "")
	status, code = classify(err)
	if status != http.StatusUnprocessableEntity || code != "invalid_request" {
		t.Fatalf("handoff body reached service: %d %s", status, code)
	}
}

func TestBuyerPaymentSignalRoutesAreKeylessPostOnlyAndFailClosed(t *testing.T) {
	path := "/v1/buyer/orders/" + paymentOrderID + "/payment"
	for suffix, kind := range map[string]routeKind{"/refresh": paymentRefreshRoute, "/cancel": paymentCancelRoute} {
		selected := matchRoute(path + suffix)
		if selected.kind != kind || selected.id != paymentOrderID || !isPaymentRoute(kind) || !isKeylessPaymentRoute(kind) {
			t.Fatalf("signal route drift: %s", path+suffix)
		}
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
			if allowed(kind, method) {
				t.Fatalf("signal route accepted %s", method)
			}
		}
		if !allowed(kind, http.MethodPost) {
			t.Fatal("signal route rejects POST")
		}
		if matchRoute(path+suffix+"/").kind != unknownRoute || matchRoute(path+suffix+"/x").kind != unknownRoute {
			t.Fatal("malformed signal route accepted")
		}
		request := httptest.NewRequest(http.MethodPost, path+suffix, nil)
		request.Header.Set("Idempotency-Key", "valid-key-1")
		if _, ok := keyFor(request, true, false); ok {
			t.Fatal("signal accepted a replay key")
		}
		h := &handler{}
		err := h.dispatch(context.Background(), httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path+suffix, nil),
			selected, paymentOrderID, testBuyerKey, "")
		if status, code := classify(err); status != http.StatusNotFound || code != "not_found" {
			t.Fatalf("disabled payment service exposed %s: %d %s", suffix, status, code)
		}
		h.payment = &checkout.HostedPaymentStarter{} // PAYUNi-only service: Stripe signals are absent
		err = h.dispatch(context.Background(), httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path+suffix, strings.NewReader("{}")),
			selected, paymentOrderID, testBuyerKey, "")
		if status, code := classify(err); status != http.StatusUnprocessableEntity || code != "invalid_request" {
			t.Fatalf("signal body reached service: %d %s", status, code)
		}
		err = h.dispatch(context.Background(), httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path+suffix, nil),
			selected, paymentOrderID, testBuyerKey, "")
		if status, code := classify(err); status != http.StatusNotFound || code != "not_found" {
			t.Fatalf("service without Stripe answered %s: %d %s", suffix, status, code)
		}
	}
}

func TestBuyerPaymentKeylessPathsAreNonRetryableAndPrepareAdmitsStripe(t *testing.T) {
	path := "/v1/buyer/orders/" + paymentOrderID + "/payment"
	for suffix, want := range map[string]bool{"/handoff": true, "/refresh": true, "/cancel": true,
		"": false, "/prepare": false} {
		if keylessPaymentPath(path+suffix) != want {
			t.Fatalf("non-retryable classification of %q drifted", suffix)
		}
	}
	if keylessPaymentPath("/v1/buyer/cart/payment/refresh") || keylessPaymentPath("/v1/buyer/session") {
		t.Fatal("non-order path classified as keyless payment")
	}
	for code, want := range map[string]bool{"payuni_credit": true, "stripe_checkout": true,
		"stripe": false, "": false, "payuni_installment": false, "Stripe_Checkout": false} {
		if admittedPaymentMethod(code) != want {
			t.Fatalf("prepare method allow-list drifted for %q", code)
		}
	}
	var in paymentPrepareInput
	body := `{"method_code":"stripe_checkout","method_version":1,"locale":"en"}`
	if err := json.Unmarshal([]byte(body), &in); err != nil || in.MethodCode != "stripe_checkout" {
		t.Fatalf("stripe prepare body rejected: %v", err)
	}
}
