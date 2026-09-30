// directory_test.go: MOCK tests of the GetStoreList decoder and the (CvsType) cache: coalescing,
// 20:00 Asia/Taipei expiry, failure memory, cancellation, over-cap and layout tolerance.

package ecpay

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const listJSON = `{"RtnCode":1,"RtnMsg":"成功","CvsList":[{"LogisticsSubType":"UNIMART","StoreList":[
 {"StoreId":"131386  ","StoreName":"測試店 ","StoreAddr":"臺北市中正區１２３號 ","StorePhone":"02-1 "},
 {"StoreId":"006598","StoreName":"全家","StoreAddr":"a","StorePhone":""}]}]}`

func taipeiAt(y int, m time.Month, d, h, mi int) time.Time {
	return time.Date(y, m, d, h, mi, 0, 0, time.FixedZone("x", 8*3600))
}

func TestDirectoryDecodeLayouts(t *testing.T) {
	flat := `{"StoreList":[{"StoreId":"1","StoreName":"n"}],"RtnCode":"1"}`
	rtn, stores, err := decodeStoreList(strings.NewReader(flat), false)
	if err != nil || rtn != "1" || stores["1"].Name != "n" {
		t.Fatalf("flat: %v %q %v", err, rtn, stores)
	}
	rtn, stores, err = decodeStoreList(strings.NewReader(listJSON), false)
	if err != nil || rtn != "1" || len(stores) != 2 {
		t.Fatalf("nested: %v %q %v", err, rtn, stores)
	}
	// X10: trailing spaces are trimmed everywhere, leading zeros and full-width digits are kept.
	if s := stores["131386"]; s.Name != "測試店" || s.Address != "臺北市中正區１２３號" || s.Phone != "02-1" {
		t.Errorf("trim/raw address: %+v", s)
	}
	if _, ok := stores["006598"]; !ok {
		t.Error("leading zeros must be kept")
	}
	for _, bad := range []string{``, `[]`, `{"RtnCode":1,"StoreList":[{"StoreId":"1"`, strings.Repeat("[", 50), `{"a":` + strings.Repeat("[", 50)} {
		if _, s, err := decodeStoreList(strings.NewReader(bad), false); err == nil && len(s) != 0 {
			t.Errorf("bad doc %.20q decoded", bad)
		}
	}
	if _, _, err := decodeStoreList(strings.NewReader(`{"RtnCode":1,"StoreList":[{"StoreId":"1"`), false); !errors.Is(err, ErrUnavailable) {
		t.Errorf("truncated doc err=%v", err)
	}
}

func TestDirectoryNextRefresh(t *testing.T) {
	for _, tc := range []struct{ in, want time.Time }{
		{taipeiAt(2026, 9, 30, 12, 0), taipeiAt(2026, 9, 30, 20, 0)},
		{taipeiAt(2026, 9, 30, 20, 0), taipeiAt(2026, 10, 1, 20, 0)},
		{taipeiAt(2026, 9, 30, 23, 59), taipeiAt(2026, 10, 1, 20, 0)},
		{taipeiAt(2026, 9, 30, 0, 0), taipeiAt(2026, 9, 30, 20, 0)},
	} {
		if got := nextRefresh(tc.in.UTC()); !got.Equal(tc.want) {
			t.Errorf("nextRefresh(%v)=%v want %v", tc.in, got, tc.want)
		}
	}
}

func dirClient(t *testing.T, f *fakeECPay, now time.Time) (*Client, *clock) {
	t.Helper()
	c := newTestClient(t, EnvSandbox, f)
	clk := &clock{t: now}
	c.now = clk.now
	return c, clk
}

func okList() *fakeECPay {
	return fakeWith(func(_ string, form url.Values) string {
		if !VerifyMac(form, tKey, tIV) {
			return `{"RtnCode":0}`
		}
		return listJSON
	}, 200)
}

func TestDirectoryCacheAndExpiry(t *testing.T) {
	f := okList()
	c, clk := dirClient(t, f, taipeiAt(2026, 9, 30, 12, 0))
	if _, hit, fresh := c.CachedStore("UNIMART", "131386"); hit || fresh {
		t.Fatal("cold cache must miss")
	}
	for i := 0; i < 3; i++ {
		m, err := c.StoreDirectory(context.Background(), "UNIMART", tCr)
		if err != nil || m["131386"].Name != "測試店" {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if n := f.count("/Helper/GetStoreList"); n != 1 {
		t.Fatalf("fetches=%d want 1", n)
	}
	if s, hit, fresh := c.CachedStore("UNIMART", "131386"); !hit || !fresh || s.Address == "" {
		t.Fatalf("cached store: %+v %v %v", s, hit, fresh)
	}
	if _, hit, _ := c.CachedStore("UNIMART", "999999"); hit {
		t.Error("unknown id must miss")
	}
	if _, hit, _ := c.CachedStore("FAMI", "131386"); hit {
		t.Error("cache is per CvsType")
	}
	clk.add(8*time.Hour + time.Minute) // past 20:00 Asia/Taipei
	if _, hit, fresh := c.CachedStore("UNIMART", "131386"); !hit || fresh {
		t.Errorf("stale hit must report cacheFresh=false: hit=%v fresh=%v", hit, fresh)
	}
	if _, err := c.StoreDirectory(context.Background(), "UNIMART", tCr); err != nil {
		t.Fatal(err)
	}
	if n := f.count("/Helper/GetStoreList"); n != 2 {
		t.Fatalf("refetch after 20:00: fetches=%d want 2", n)
	}
}

func TestDirectorySingleFlight(t *testing.T) {
	release := make(chan struct{})
	f := &fakeECPay{h: func(string, url.Values) (*httpResp, error) { <-release; return reply(200, listJSON), nil }}
	c, _ := dirClient(t, f, taipeiAt(2026, 9, 30, 12, 0))
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.StoreDirectory(context.Background(), "UNIMART", tCr)
			errs <- err
		}()
	}
	for f.count("/Helper/GetStoreList") == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := f.count("/Helper/GetStoreList"); n != 1 {
		t.Fatalf("concurrent misses must coalesce: fetches=%d", n)
	}
}

func TestDirectoryFailureMemory(t *testing.T) {
	f := fakeWith(func(string, url.Values) string { return "" }, 403)
	c, clk := dirClient(t, f, taipeiAt(2026, 9, 30, 12, 0))
	for i := 0; i < 3; i++ {
		if _, err := c.StoreDirectory(context.Background(), "UNIMART", tCr); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("call %d err=%v", i, err)
		}
	}
	if n := f.count("/Helper/GetStoreList"); n != 1 {
		t.Fatalf("a remembered failure must not refetch: fetches=%d", n)
	}
	clk.add(failureMemory + time.Second)
	_, _ = c.StoreDirectory(context.Background(), "UNIMART", tCr)
	if n := f.count("/Helper/GetStoreList"); n != 2 {
		t.Fatalf("after 10 minutes one more attempt: fetches=%d", n)
	}
}

func TestDirectoryCancellationIsNotRemembered(t *testing.T) {
	block := make(chan struct{})
	first := true
	var mu sync.Mutex
	f := &fakeECPay{h: func(string, url.Values) (*httpResp, error) {
		mu.Lock()
		blockThis := first
		first = false
		mu.Unlock()
		if blockThis {
			<-block
			return nil, errors.New("cancelled")
		}
		return reply(200, listJSON), nil
	}}
	c, _ := dirClient(t, f, taipeiAt(2026, 9, 30, 12, 0))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := c.StoreDirectory(ctx, "UNIMART", tCr); done <- err }()
	for f.count("/Helper/GetStoreList") == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	close(block)
	if err := <-done; !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err=%v", err)
	}
	if _, err := c.StoreDirectory(context.Background(), "UNIMART", tCr); err != nil {
		t.Fatalf("a caller-side cancellation must not poison the cache: %v", err)
	}
}

func TestDirectoryRejects(t *testing.T) {
	c, _ := dirClient(t, okList(), taipeiAt(2026, 9, 30, 12, 0))
	if _, err := c.StoreDirectory(context.Background(), "OKMART", tCr); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown CvsType err=%v", err)
	}
	if _, err := c.StoreDirectory(context.Background(), "UNIMART", Credentials{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty creds err=%v", err)
	}
	for name, body := range map[string]string{
		"rtn 0":       `{"RtnCode":0,"StoreList":[{"StoreId":"1"}]}`,
		"empty list":  `{"RtnCode":1,"StoreList":[]}`,
		"not json":    `<html>`,
		"missing rtn": `{"StoreList":[{"StoreId":"1"}]}`,
	} {
		f := fakeWith(func(string, url.Values) string { return body }, 200)
		c, _ := dirClient(t, f, taipeiAt(2026, 9, 30, 12, 0))
		if _, err := c.StoreDirectory(context.Background(), "FAMI", tCr); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: err=%v", name, err)
		}
	}
}

func TestDirectoryLargeListAndCap(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"RtnCode":1,"CvsList":[{"StoreList":[`)
	const n = 20000
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"StoreId":"%06d","StoreName":"n","StoreAddr":"a","StorePhone":"p"}`, i)
	}
	b.WriteString(`]}]}`)
	f := fakeWith(func(string, url.Values) string { return b.String() }, 200)
	c, _ := dirClient(t, f, taipeiAt(2026, 9, 30, 12, 0))
	m, err := c.StoreDirectory(context.Background(), "UNIMART", tCr)
	if err != nil || len(m) != n {
		t.Fatalf("large list: %v len=%d", err, len(m))
	}
	// Over the 32 MiB cap: a truncated document must fail, never yield a partial directory.
	big := `{"RtnCode":1,"Pad":"` + strings.Repeat("x", maxStoreListBody) + `","StoreList":[{"StoreId":"1"}]}`
	f = fakeWith(func(string, url.Values) string { return big }, 200)
	c, _ = dirClient(t, f, taipeiAt(2026, 9, 30, 12, 0))
	if _, err := c.StoreDirectory(context.Background(), "UNIMART", tCr); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("over-cap err=%v", err)
	}
}
