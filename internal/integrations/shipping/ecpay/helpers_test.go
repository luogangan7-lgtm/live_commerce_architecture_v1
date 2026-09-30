// helpers_test.go: shared fakes for the ecpay unit tests. Nothing here touches the network: the
// client is given a RoundTripper that answers for the ECPay hostnames in-process (MOCK evidence).

package ecpay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// Synthetic credentials: obviously fake, assembled from parts so no key-shaped literal exists
// (PROCESS §6). They are not any ECPay account's keys.
var (
	tKey = "fake" + "key0123456ab"
	tIV  = "fake" + "iv0123456789"
	tCr  = Credentials{MerchantID: "2000933", HashKey: tKey, HashIV: tIV}
)

const (
	tTradeNo = "LCABCDEFGHIJKLMNOPQR"
	tHooks   = "https://hooks.example.test/v1/cvs/ecpay/status/00000000-0000-4000-8000-000000000001"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func reply(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}

// fakeECPay records every request form and answers with handler.
type fakeECPay struct {
	mu    sync.Mutex
	calls []fakeCall
	h     func(path string, form url.Values) (*http.Response, error)
}

type fakeCall struct {
	Host, Path string
	Form       url.Values
}

func (f *fakeECPay) RoundTrip(r *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(b))
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{Host: r.URL.Host, Path: r.URL.Path, Form: form})
	f.mu.Unlock()
	return f.h(r.URL.Path, form)
}

func (f *fakeECPay) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.Path == path {
			n++
		}
	}
	return n
}

func newTestClient(t *testing.T, env Environment, f *fakeECPay) *Client {
	t.Helper()
	c, err := NewClient(env, f)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// signedBody renders k=v pairs with a valid CheckMacValue in ECPay's response layout.
func signedBody(v url.Values, key, iv string) string {
	v.Set(macField, CheckMac(v, key, iv))
	var parts []string
	for k := range v {
		parts = append(parts, k+"="+v.Get(k))
	}
	return strings.Join(parts, "&")
}

func createOK() url.Values {
	return url.Values{
		"MerchantID": {tCr.MerchantID}, "MerchantTradeNo": {tTradeNo}, "RtnCode": {"300"},
		"RtnMsg": {"訂單處理中(已收到訂單資料)"}, "AllPayLogisticsID": {"1234567"}, "LogisticsType": {"CVS"},
		"LogisticsSubType": {"UNIMARTC2C"}, "GoodsAmount": {"350"}, "ReceiverName": {"王小明"},
		"CVSPaymentNo": {"C1234567"}, "CVSValidationNo": {"9876"},
	}
}

func goodPayload() Payload {
	return Payload{HashKey: tKey, HashIV: tIV, SenderName: "陳大文", SenderCellPhone: "0987654321"}
}

func goodRequest() CreateRequest {
	return CreateRequest{
		SubType: "UNIMARTC2C", MerchantTradeNo: tTradeNo, MerchantTradeDate: "2026/09/30 12:00:00",
		ReceiverStoreID: "131386", ReceiverName: "王小明", ReceiverPhone: "+886 912-345-678", GoodsName: "ignored",
		ServerReplyURL: tHooks, GoodsAmount: 350,
	}
}

// clock is a settable time source for cache tests.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func bodyContains(b []byte, s string) bool { return bytes.Contains(b, []byte(s)) }

type httpResp = http.Response

func fmtS(v any) string { return fmt.Sprintf("%s", v) }
func fmtV(v any) string { return fmt.Sprintf("%+v %#v", v, v) }
func fmtJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
