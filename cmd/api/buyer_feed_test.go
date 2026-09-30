package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// mountFeed: only the exact feed path, only with exactly one correct BFF key; everything else reaches next.
func TestMountFeed(t *testing.T) {
	const key = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFG" // 43-char neutral fixture, not a secret
	var next, feed int
	h := mountFeed(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { next++; w.WriteHeader(http.StatusTeapot) }),
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { feed++; w.WriteHeader(http.StatusOK) }), key)
	do := func(path string, keys ...string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		for _, k := range keys {
			req.Header.Add("X-Commerce-Buyer-BFF-Key", k)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if c := do("/v1/buyer/feeds/meta.csv", key); c != 200 || feed != 1 {
		t.Fatalf("right key = %d feed=%d", c, feed)
	}
	for name, keys := range map[string][]string{"none": nil, "wrong": {key[1:] + "x"}, "two": {key, key}} {
		if c := do("/v1/buyer/feeds/meta.csv", keys...); c != 401 || feed != 1 {
			t.Errorf("%s = %d feed=%d", name, c, feed)
		}
	}
	if c := do("/v1/buyer/catalog", key); c != http.StatusTeapot || next != 1 {
		t.Fatalf("other path = %d next=%d", c, next)
	}
}
