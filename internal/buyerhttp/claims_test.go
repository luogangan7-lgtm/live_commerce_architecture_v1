// claims_test.go covers the B1–B2 transport rules that hold before the buyer transaction
// (contract live-keyword-claims-v1 §7.2, §5.6) and the frozen projections. Real-PG
// preview/redeem behaviour is KC14 (test_worker) and the KC16 browser chain.

package buyerhttp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"livecommerce/internal/claims"
	"livecommerce/internal/httperror"
	"livecommerce/internal/storefront"
)

const testClaimToken = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBA"

func TestClaimRoutesAndMethods(t *testing.T) {
	for _, tc := range []struct {
		path, method string
		kind         routeKind
		ok           bool
	}{
		{"/v1/buyer/claim-link", http.MethodGet, claimLinkRoute, true},
		{"/v1/buyer/claim-link", http.MethodPost, claimLinkRoute, false},
		{"/v1/buyer/claim-link", http.MethodHead, claimLinkRoute, false},
		{"/v1/buyer/claim-link/redeem", http.MethodPost, claimRedeemRoute, true},
		{"/v1/buyer/claim-link/redeem", http.MethodGet, claimRedeemRoute, false},
		{"/v1/buyer/claim-link/", http.MethodGet, unknownRoute, false},
		{"/v1/buyer/claim-link/" + testClaimToken, http.MethodGet, unknownRoute, false},
	} {
		found := matchRoute(tc.path)
		if found.kind != tc.kind || allowed(found.kind, tc.method) != tc.ok {
			t.Fatalf("%s %s: kind=%d allowed=%v", tc.method, tc.path, found.kind, allowed(found.kind, tc.method))
		}
	}
}

func TestClaimTokenOnlyInItsHeaderOnItsRoutes(t *testing.T) {
	for _, path := range []string{"/v1/buyer/cart", "/v1/buyer/catalog", "/v1/buyer/session", "/v1/buyer/claim-link/x"} {
		r := httptest.NewRequest(http.MethodGet, "http://internal"+path, nil)
		r.Header[claimTokenHeader] = []string{""}
		if !forbiddenInput(r) {
			t.Fatalf("claim token header accepted on %s", path)
		}
	}
	for _, path := range []string{claimLinkPath, claimRedeemPath} {
		r := httptest.NewRequest(http.MethodGet, "http://internal"+path, nil)
		r.Header.Set(claimTokenHeader, testClaimToken)
		if forbiddenInput(r) {
			t.Fatalf("claim token header rejected on %s", path)
		}
		r.URL.RawQuery = "t=" + testClaimToken
		if !forbiddenInput(r) {
			t.Fatalf("token query accepted on %s", path)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "http://internal/v1/buyer/claim%2Dlink", nil)
	r.Header.Set(claimTokenHeader, testClaimToken)
	if !forbiddenInput(r) {
		t.Fatal("escaped alias of the claim route admitted the token header")
	}
}

func TestClaimTokenHeaderStrictness(t *testing.T) {
	for _, values := range [][]string{nil, {""}, {testClaimToken, testClaimToken}, {testClaimToken + "="},
		{strings.Repeat("B", 42)}, {"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"}, {"Bearer " + testClaimToken}} {
		r := httptest.NewRequest(http.MethodGet, "http://internal"+claimLinkPath, nil)
		if values != nil {
			r.Header[claimTokenHeader] = values
		}
		if _, err := claimToken(r); err == nil {
			t.Fatalf("malformed claim token accepted: %d values", len(values))
		} else if status, code := classify(err); status != 422 || code != "invalid_request" {
			t.Fatalf("malformed claim token status %d/%s", status, code)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "http://internal"+claimLinkPath, nil)
	r.Header.Set(claimTokenHeader, testClaimToken)
	if token, err := claimToken(r); err != nil || string(token) != testClaimToken {
		t.Fatalf("canonical claim token rejected: %v", err)
	}
}

func TestClaimEarlyTransportFailures(t *testing.T) {
	h := httperror.Middleware(&handler{bffKey: testBuyerKey})
	for _, tc := range []struct {
		name, method, path, key string
		status                  int
		cache                   string
	}{
		{"direct cart write with derived key", http.MethodPut, "/v1/buyer/cart", "clm:0123456789abcdef", 422, "no-store"},
		{"preview replay key", http.MethodGet, claimLinkPath, "validkey1", 422, "private, no-store"},
		{"redeem without key", http.MethodPost, claimRedeemPath, "", 422, "private, no-store"},
		{"preview wrong method", http.MethodPost, claimLinkPath, "validkey1", 405, "private, no-store"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "http://internal"+tc.path, strings.NewReader(`{}`))
			r.Header.Set("X-Commerce-Buyer-BFF-Key", testBuyerKey)
			r.Header.Set("X-Commerce-Storefront-Origin", "https://shop.example")
			r.Header.Set("Authorization", "Bearer "+testBuyerKey)
			r.Header.Set(claimTokenHeader, testClaimToken)
			if tc.key != "" {
				r.Header.Set("Idempotency-Key", tc.key)
			}
			if tc.path == "/v1/buyer/cart" {
				r.Header.Del(claimTokenHeader)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			var got httperror.Envelope
			if w.Code != tc.status || json.Unmarshal(w.Body.Bytes(), &got) != nil || w.Header().Get("Cache-Control") != tc.cache {
				t.Fatalf("status=%d cache=%q body=%s", w.Code, w.Header().Get("Cache-Control"), w.Body.String())
			}
			if strings.Contains(w.Body.String(), testClaimToken) {
				t.Fatal("claim token echoed in an error body")
			}
		})
	}
}

func TestClaimProjectionsAreExact(t *testing.T) {
	expires := time.Date(2026, 9, 28, 12, 0, 0, 123000, time.FixedZone("x", 8*3600))
	preview, err := json.Marshal(projectClaimPreview(claims.Preview{BundleVersion: 2, Bound: true, ExpiresAt: expires,
		Lines: []claims.PreviewLine{{Keyword: "A1", SKUID: "s1", SKUCode: "CODE", ProductName: "Name", Currency: "TWD",
			UnitPriceMinor: 1200, Quantity: 3, Pending: true, Available: false}}}))
	want := `{"bundle_version":2,"bound":true,"expires_at":"2026-09-28T04:00:00.000123Z","lines":[{"keyword":"A1","sku_id":"s1","sku_code":"CODE","product_name":"Name","currency":"TWD","unit_price_minor":1200,"quantity":3,"pending":true,"available":false}]}`
	if err != nil || string(preview) != want {
		t.Fatalf("B1 projection %s", preview)
	}
	redeem, err := json.Marshal(projectClaimRedeem(claims.Redeemed{BundleVersion: 2,
		Cart:    storefront.Cart{ID: "c1", Currency: "TWD", Version: 4, Items: []storefront.Item{{SKUID: "s1", Quantity: 3}}},
		Applied: []storefront.Item{{SKUID: "s1", Quantity: 3}}, Skipped: []claims.Skipped{{SKUID: "s2", Reason: "unavailable"}}}))
	want = `{"bundle_version":2,"cart":{"id":"c1","currency":"TWD","version":4,"items":[{"sku_id":"s1","quantity":3}]},"applied":[{"sku_id":"s1","quantity":3}],"skipped":[{"sku_id":"s2","reason":"unavailable"}]}`
	if err != nil || string(redeem) != want {
		t.Fatalf("B2 projection %s", redeem)
	}
	empty, _ := json.Marshal(projectClaimRedeem(claims.Redeemed{}))
	if !strings.Contains(string(empty), `"applied":[],"skipped":[]`) {
		t.Fatalf("empty lists must stay arrays: %s", empty)
	}
}
