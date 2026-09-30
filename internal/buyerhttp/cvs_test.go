package buyerhttp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"livecommerce/internal/checkout"
	"livecommerce/internal/fulfillment"
)

const cvsID = "33333333-3333-4333-8333-333333333333"

func TestMatchCVSRoute(t *testing.T) {
	for path, want := range map[string]route{
		"/v1/buyer/cvs-selections":                      {kind: routeCVSSelectionOpen},
		"/v1/buyer/cvs-stores":                          {kind: routeCVSStoreEnter},
		"/v1/buyer/cvs-selections/" + cvsID:             {kind: routeCVSSelectionGet, id: cvsID},
		"/v1/buyer/cvs-selections/" + cvsID + "/verify": {kind: routeCVSSelectionVerify, id: cvsID},
	} {
		if got := matchCVSRoute(path); got != want {
			t.Errorf("%s -> %+v want %+v", path, got, want)
		}
	}
	for _, path := range []string{"/v1/buyer/cvs-selections/", "/v1/buyer/cvs-selections//verify", "/v1/buyer/cvs-selections/" + cvsID + "/verify/x",
		"/v1/buyer/cvs-selections/" + cvsID + "/other", "/v1/buyer/cvs-stores/x", "/v1/buyer/cvs", "/v1/buyer/cvs-selections/a/b"} {
		if got := matchCVSRoute(path); got.kind != unknownRoute {
			t.Errorf("%s matched %+v", path, got)
		}
	}
}

func TestCVSRouteKindsClearOfHandlerIota(t *testing.T) {
	if routeCVSSelectionOpen <= claimRedeemRoute {
		t.Fatal("CVS route kinds collide with handler.go's iota block")
	}
	for kind := routeCVSSelectionOpen; kind <= routeCVSStoreEnter; kind++ {
		if !isCVSRoute(kind) {
			t.Fatalf("kind %d not recognised", kind)
		}
	}
	if isCVSRoute(claimRedeemRoute) || isCVSRoute(unknownRoute) {
		t.Fatal("foreign kind claimed")
	}
}

func TestAllowedCVSMethods(t *testing.T) {
	for kind, methods := range map[routeKind]map[string]bool{
		routeCVSSelectionOpen:   {http.MethodPost: true},
		routeCVSStoreEnter:      {http.MethodPost: true},
		routeCVSSelectionVerify: {http.MethodPost: true},
		routeCVSSelectionGet:    {http.MethodGet: true},
	} {
		for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodHead} {
			if allowedCVS(kind, m) != methods[m] {
				t.Errorf("kind %d method %s = %v", kind, m, allowedCVS(kind, m))
			}
		}
	}
	if allowedCVS(unknownRoute, http.MethodGet) {
		t.Fatal("unknown kind allowed")
	}
}

func TestCVSHTTPError(t *testing.T) {
	err := cvsHTTPError(&fulfillment.CVSError{Status: 422, Code: "bad_return_path"})
	var re responseError
	if !errors.As(err, &re) || re.status != 422 || re.code != "bad_return_path" {
		t.Fatalf("coded refusal: %v", err)
	}
	retry := cvsHTTPError(&fulfillment.CVSError{Status: 429, Code: "pay_at_pickup_limit", RetryAfter: 60})
	var coded codedResponse
	if !errors.As(retry, &coded) || coded.RetryAfterSeconds != 60 || !errors.As(retry, &re) || re.status != 429 || re.code != "pay_at_pickup_limit" {
		t.Fatalf("429 must keep Retry-After and still classify as a responseError: %v", retry)
	}
	if status, code := classify(retry); status != 429 || code != "pay_at_pickup_limit" {
		t.Fatalf("classify: %d %s", status, code)
	}
	other := errors.New("plain")
	if cvsHTTPError(other) != other || cvsHTTPError(nil) != nil {
		t.Fatal("non-CVS errors must pass through untouched")
	}
}

func TestOriginAndMobileHints(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("X-Commerce-Storefront-Origin", "https://shop.example.test")
	if got := storefrontOrigin(r); got != "https://shop.example.test" {
		t.Fatalf("origin %q", got)
	}
	r.Header.Add("X-Commerce-Storefront-Origin", "https://evil.example.test")
	if storefrontOrigin(r) != "" {
		t.Fatal("duplicate origin header must not be trusted")
	}
	for value, want := range map[string]bool{"mobile": true, "desktop": false, "": false, "MOBILE": false} {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		if value != "" {
			req.Header.Set("X-Commerce-Device", value)
		}
		if mobileHint(req) != want {
			t.Errorf("device %q -> %v", value, mobileHint(req))
		}
	}
	dup := httptest.NewRequest(http.MethodPost, "/", nil)
	dup.Header.Add("X-Commerce-Device", "mobile")
	dup.Header.Add("X-Commerce-Device", "mobile")
	if mobileHint(dup) {
		t.Fatal("duplicate device header must not select mobile")
	}
}

func TestCVSRequestIsNotFoundWhenNotWired(t *testing.T) {
	h := &handler{checkout: &checkout.Service{}}
	for _, kind := range []routeKind{routeCVSSelectionOpen, routeCVSSelectionGet, routeCVSSelectionVerify, routeCVSStoreEnter} {
		_, err := h.cvsRequest(context.Background(), httptest.NewRequest(http.MethodPost, "/", nil), route{kind: kind, id: cvsID}, cvsID, "t", "k")
		var re responseError
		if !errors.As(err, &re) || re.status != http.StatusNotFound {
			t.Errorf("kind %d: %v", kind, err)
		}
	}
}
