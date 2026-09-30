package attribution

import (
	"bytes"
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// feed_test.go: CSV escaping, price format and the request guards that run before any SQL (nil pool is never touched).

func TestFeedPrice(t *testing.T) {
	for _, tc := range []struct {
		minor int64
		cur   string
		want  string
		ok    bool
	}{
		{123400, "TWD", "1234.00 TWD", true}, {5, "USD", "0.05 USD", true}, {0, "TWD", "0.00 TWD", true},
		{100000000000, "HKD", "1000000000.00 HKD", true}, {-1, "TWD", "", false}, {100, "twd", "", false}, {100, "", "", false},
	} {
		if got, ok := feedPrice(tc.minor, tc.cur); got != tc.want || ok != tc.ok {
			t.Errorf("feedPrice(%d,%q) = %q,%v want %q,%v", tc.minor, tc.cur, got, ok, tc.want, tc.ok)
		}
	}
}

func TestWriteFeedEscaping(t *testing.T) {
	var buf bytes.Buffer
	rows := []feedRow{
		{ID: "a", Title: `He said "hi", ok`, Description: "line1\nline2, more", Availability: "in stock", PriceMinor: 123400, Currency: "TWD", Link: "https://s.example.test/products/p", Brand: "Shop, Inc"},
		{ID: "skipped", Title: "bad price", Availability: "in stock", PriceMinor: -5, Currency: "TWD", Link: "l", Brand: "b"},
		{ID: "c", Title: "=1+1", Description: "d", Availability: "out of stock", PriceMinor: 100, Currency: "USD", Link: "l2", Brand: "b"},
	}
	if err := writeFeed(csv.NewWriter(&buf), rows); err != nil {
		t.Fatal(err)
	}
	got, err := csv.NewReader(&buf).ReadAll() // round trip: what a consumer parses is exactly what we meant
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || strings.Join(got[0], ",") != strings.Join(feedColumns, ",") {
		t.Fatalf("shape = %v", got)
	}
	want := []string{"a", `He said "hi", ok`, "line1\nline2, more", "in stock", "new", "1234.00 TWD", "https://s.example.test/products/p", "", "Shop, Inc"}
	for i := range want {
		if got[1][i] != want[i] {
			t.Errorf("row 1 col %d = %q want %q", i, got[1][i], want[i])
		}
	}
	if got[2][0] != "c" || got[2][4] != "new" || got[2][5] != "1.00 USD" {
		t.Errorf("row 2 = %v", got[2])
	}
}

func TestFeedHandlerRejectsBeforeSQL(t *testing.T) {
	h := FeedHandler(nil) // a nil pool would fail any request that got as far as SQL with 503, not the codes below
	for name, tc := range map[string]struct {
		method, target string
		origins        []string
		want           int
	}{
		"post":         {http.MethodPost, "/v1/buyer/feeds/meta.csv", []string{"https://s.example.test"}, 405},
		"query":        {http.MethodGet, "/v1/buyer/feeds/meta.csv?store=1", []string{"https://s.example.test"}, 422},
		"no origin":    {http.MethodGet, "/v1/buyer/feeds/meta.csv", nil, 422},
		"two origins":  {http.MethodGet, "/v1/buyer/feeds/meta.csv", []string{"https://a.example.test", "https://b.example.test"}, 422},
		"comma origin": {http.MethodGet, "/v1/buyer/feeds/meta.csv", []string{"https://a.example.test,https://b.example.test"}, 422},
		"empty origin": {http.MethodGet, "/v1/buyer/feeds/meta.csv", []string{""}, 422},
		"nil pool":     {http.MethodGet, "/v1/buyer/feeds/meta.csv", []string{"https://s.example.test"}, 503},
	} {
		req := httptest.NewRequest(tc.method, tc.target, nil)
		for _, o := range tc.origins {
			req.Header.Add(originHeader, o)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s: status %d want %d", name, rec.Code, tc.want)
		}
	}
}
