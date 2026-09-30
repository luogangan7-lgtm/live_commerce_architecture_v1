// client_test.go: MOCK tests of Create, Query and Probe classification against an in-process fake
// transport (no network, no ECPay key). Each UNKNOWN branch asserts that exactly one request was sent.

package ecpay

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func fakeWith(h func(path string, form url.Values) string, status int) *fakeECPay {
	return &fakeECPay{h: func(p string, f url.Values) (*httpResp, error) { return reply(status, h(p, f)), nil }}
}

func TestClientCreateSucceeded(t *testing.T) {
	f := fakeWith(func(_ string, form url.Values) string {
		if !VerifyMac(form, tKey, tIV) {
			t.Error("request MAC invalid")
		}
		return "1|" + signedBody(createOK(), tKey, tIV)
	}, 200)
	c := newTestClient(t, EnvSandbox, f)
	got := c.Create(context.Background(), tCr, goodPayload(), goodRequest())
	want := Result{Outcome: "SUCCEEDED", Code: CodeCreated, LogisticsID: "1234567", PaymentNo: "C1234567", ValidationNo: "9876", StatusCode: "300"}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
	call := f.calls[0]
	if call.Host != "logistics-stage.ecpay.com.tw" || call.Path != "/Express/Create" || len(f.calls) != 1 {
		t.Fatalf("bad call %+v (%d)", call, len(f.calls))
	}
	for k, w := range map[string]string{
		"MerchantTradeNo": tTradeNo, "LogisticsType": "CVS", "LogisticsSubType": "UNIMARTC2C", "GoodsAmount": "350",
		"IsCollection": "N", "GoodsName": "商品", "ReceiverCellPhone": "0912345678", "ReceiverStoreID": "131386",
		"SenderCellPhone": "0987654321", "ServerReplyURL": tHooks, "PlatformID": "", "MerchantTradeDate": "2026/09/30 12:00:00",
	} {
		if _, present := call.Form[k]; !present || call.Form.Get(k) != w {
			t.Errorf("field %s=%q want %q", k, call.Form.Get(k), w)
		}
	}
	if _, present := call.Form["CollectionAmount"]; present {
		t.Error("card order must not send CollectionAmount")
	}
}

func TestClientCreateLiveHostAndCollection(t *testing.T) {
	f := fakeWith(func(string, url.Values) string { return "1|" + signedBody(createOK(), tKey, tIV) }, 200)
	c := newTestClient(t, EnvLive, f)
	r := goodRequest()
	r.CollectionAmount = r.GoodsAmount
	if got := c.Create(context.Background(), tCr, goodPayload(), r); got.Outcome != "SUCCEEDED" {
		t.Fatalf("got %+v", got)
	}
	call := f.calls[0]
	if call.Host != "logistics.ecpay.com.tw" || call.Form.Get("IsCollection") != "Y" || call.Form.Get("CollectionAmount") != "350" {
		t.Fatalf("live/collection form wrong: %s %v", call.Host, call.Form)
	}
}

func TestClientCreateClassification(t *testing.T) {
	badMAC := func() string { v := createOK(); return "1|" + signedBody(v, tKey, "x"+tIV) }
	wrongTrade := func() string {
		v := createOK()
		v.Set("MerchantTradeNo", "LCZZZZZZZZZZZZZZZZZZ")
		return "1|" + signedBody(v, tKey, tIV)
	}
	badID := func() string {
		v := createOK()
		v.Set("AllPayLogisticsID", "bad id!")
		return "1|" + signedBody(v, tKey, tIV)
	}
	noID := func() string { v := createOK(); v.Del("AllPayLogisticsID"); return "1|" + signedBody(v, tKey, tIV) }
	cases := []struct {
		name    string
		status  int
		body    string
		outcome string
		code    string
	}{
		{"rejected", 200, "0|餘額不足", "FAILED_FINAL", CodeRejected},
		{"bad mac", 200, badMAC(), "UNKNOWN", CodeUncertain},
		{"wrong trade", 200, wrongTrade(), "UNKNOWN", CodeUncertain},
		{"nonconforming id", 200, badID(), "UNKNOWN", CodeUncertain},
		{"missing id", 200, noID(), "UNKNOWN", CodeUncertain},
		{"malformed", 200, "<html>oops</html>", "UNKNOWN", CodeUncertain},
		{"empty", 200, "", "UNKNOWN", CodeUncertain},
		{"forbidden", 403, "", "UNKNOWN", CodeRateLimited},
		{"server error", 500, "1|" + signedBody(createOK(), tKey, tIV), "UNKNOWN", CodeUncertain},
		{"redirect", 302, "", "UNKNOWN", CodeUncertain},
		{"oversize", 200, "1|" + strings.Repeat("A", maxResponseBody), "UNKNOWN", CodeUncertain},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := fakeWith(func(string, url.Values) string { return tc.body }, tc.status)
			got := newTestClient(t, EnvSandbox, f).Create(context.Background(), tCr, goodPayload(), goodRequest())
			if got.Outcome != tc.outcome || got.Code != tc.code || got.LogisticsID != "" {
				t.Fatalf("got %+v", got)
			}
			if len(f.calls) != 1 {
				t.Fatalf("Create must send exactly one request, sent %d", len(f.calls))
			}
		})
	}
}

func TestClientCreateTransportErrorIsUnknown(t *testing.T) {
	f := &fakeECPay{h: func(string, url.Values) (*httpResp, error) {
		return nil, errors.New("connection reset https://x/secret")
	}}
	got := newTestClient(t, EnvSandbox, f).Create(context.Background(), tCr, goodPayload(), goodRequest())
	if got.Outcome != "UNKNOWN" || got.Code != CodeUncertain {
		t.Fatalf("got %+v", got)
	}
}

func TestClientCreateTimeoutIsUnknown(t *testing.T) {
	// The transport blocks until the caller's deadline, like a hung TCP connection: the request may
	// have been written, so the outcome is UNKNOWN, never FAILED.
	c, _ := NewClient(EnvSandbox, rtFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	got := c.Create(ctx, tCr, goodPayload(), goodRequest())
	if got.Outcome != "UNKNOWN" || got.Code != CodeUncertain {
		t.Fatalf("got %+v", got)
	}
}

func TestClientCreateLocalValidation(t *testing.T) {
	mut := map[string]func(*CreateRequest, *Payload, *Credentials){
		"subtype":        func(r *CreateRequest, _ *Payload, _ *Credentials) { r.SubType = "NOPE" },
		"freeze":         func(r *CreateRequest, _ *Payload, _ *Credentials) { r.SubType = "UNIMARTFREEZE" },
		"trade no":       func(r *CreateRequest, _ *Payload, _ *Credentials) { r.MerchantTradeNo = "has space" },
		"trade date":     func(r *CreateRequest, _ *Payload, _ *Credentials) { r.MerchantTradeDate = "2026-09-30" },
		"store id":       func(r *CreateRequest, _ *Payload, _ *Credentials) { r.ReceiverStoreID = "1234567" },
		"empty store":    func(r *CreateRequest, _ *Payload, _ *Credentials) { r.ReceiverStoreID = "" },
		"receiver name":  func(r *CreateRequest, _ *Payload, _ *Credentials) { r.ReceiverName = "王1" },
		"receiver phone": func(r *CreateRequest, _ *Payload, _ *Credentials) { r.ReceiverPhone = "0212345678" },
		"goods zero":     func(r *CreateRequest, _ *Payload, _ *Credentials) { r.GoodsAmount = 0 },
		"goods high":     func(r *CreateRequest, _ *Payload, _ *Credentials) { r.GoodsAmount = 20001 },
		"collect differs": func(r *CreateRequest, _ *Payload, _ *Credentials) {
			r.CollectionAmount = 351
		},
		"http url":      func(r *CreateRequest, _ *Payload, _ *Credentials) { r.ServerReplyURL = "http://h.example.test/x" },
		"non-ascii url": func(r *CreateRequest, _ *Payload, _ *Credentials) { r.ServerReplyURL = "https://例え.test/x" },
		"long url": func(r *CreateRequest, _ *Payload, _ *Credentials) {
			r.ServerReplyURL = "https://h.example.test/" + strings.Repeat("a", 200)
		},
		"sender name":     func(_ *CreateRequest, p *Payload, _ *Credentials) { p.SenderName = "陳" },
		"sender phone":    func(_ *CreateRequest, p *Payload, _ *Credentials) { p.SenderCellPhone = "123" },
		"sender required": func(_ *CreateRequest, p *Payload, _ *Credentials) { p.SenderCellPhone = "" },
		"merchant id":     func(_ *CreateRequest, _ *Payload, c *Credentials) { c.MerchantID = "abc" },
		"no key":          func(_ *CreateRequest, _ *Payload, c *Credentials) { c.HashKey = "" },
	}
	for name, m := range mut {
		t.Run(name, func(t *testing.T) {
			f := fakeWith(func(string, url.Values) string { return "1|" + signedBody(createOK(), tKey, tIV) }, 200)
			r, p, cr := goodRequest(), goodPayload(), tCr
			m(&r, &p, &cr)
			got := newTestClient(t, EnvSandbox, f).Create(context.Background(), cr, p, r)
			if got.Outcome != "FAILED_FINAL" || got.Code != CodeInvalidRequest || len(f.calls) != 0 {
				t.Fatalf("got %+v calls=%d", got, len(f.calls))
			}
		})
	}
	// FamilyMart does not require a sender phone (F5); the same request otherwise passes.
	f := fakeWith(func(string, url.Values) string {
		v := createOK()
		v.Set("LogisticsSubType", "FAMIC2C")
		return "1|" + signedBody(v, tKey, tIV)
	}, 200)
	r, p := goodRequest(), goodPayload()
	r.SubType, p.SenderCellPhone = "FAMIC2C", ""
	if got := newTestClient(t, EnvSandbox, f).Create(context.Background(), tCr, p, r); got.Outcome != "SUCCEEDED" {
		t.Fatalf("FAMIC2C without sender phone: %+v", got)
	}
}

func queryOK() url.Values {
	v := createOK()
	v.Set("LogisticsStatus", "2030")
	v.Set("ShipmentNo", "S123")
	v.Set("CollectionAmount", "350") // mixed-case key order, verified by either candidate
	return v
}

func TestClientQuery(t *testing.T) {
	f := fakeWith(func(p string, form url.Values) string {
		if p != "/Helper/QueryLogisticsTradeInfo/V5" || !VerifyMac(form, tKey, tIV) || form.Get("MerchantTradeNo") != tTradeNo || form.Get("TimeStamp") == "" {
			t.Errorf("bad query request %s %v", p, form)
		}
		return signedBody(queryOK(), tKey, tIV)
	}, 200)
	got := newTestClient(t, EnvLive, f).Query(context.Background(), tCr, tTradeNo)
	want := Result{Outcome: "SUCCEEDED", Code: CodeQueryFound, LogisticsID: "1234567", PaymentNo: "C1234567", ValidationNo: "9876", ShipmentNo: "S123", StatusCode: "2030"}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestClientQueryStaysUnknown(t *testing.T) {
	noTrade := func() string { v := queryOK(); v.Del("AllPayLogisticsID"); return signedBody(v, tKey, tIV) }
	other := func() string {
		v := queryOK()
		v.Set("MerchantTradeNo", "LCZZZZZZZZZZZZZZZZZZ")
		return signedBody(v, tKey, tIV)
	}
	for name, tc := range map[string]struct {
		status int
		body   string
		code   string
	}{
		"not found text": {200, "0|trade not found", CodeUncertain},
		"no logistics":   {200, noTrade(), CodeUncertain},
		"other trade":    {200, other(), CodeUncertain},
		"bad mac":        {200, signedBody(queryOK(), tKey, "zz"+tIV), CodeUncertain},
		"forbidden":      {403, "", CodeRateLimited},
		"5xx":            {502, "", CodeUncertain},
	} {
		t.Run(name, func(t *testing.T) {
			f := fakeWith(func(string, url.Values) string { return tc.body }, tc.status)
			got := newTestClient(t, EnvSandbox, f).Query(context.Background(), tCr, tTradeNo)
			if got.Outcome != "UNKNOWN" || got.Code != tc.code || got.LogisticsID != "" {
				t.Fatalf("got %+v", got)
			}
		})
	}
	if got := newTestClient(t, EnvSandbox, &fakeECPay{}).Query(context.Background(), tCr, "bad trade!"); got.Outcome != "UNKNOWN" {
		t.Fatalf("invalid trade no: %+v", got)
	}
}

func TestClientProbe(t *testing.T) {
	list := func(rtn string) string {
		return `{"RtnCode":` + rtn + `,"RtnMsg":"x","CvsList":[{"StoreList":[{"StoreId":"1","StoreName":"n"}]}]}`
	}
	for name, tc := range map[string]struct {
		status int
		body   string
		ok     bool
	}{
		"ok number": {200, list("1"), true},
		"ok string": {200, list(`"1"`), true},
		"rtn zero":  {200, list("0"), false},
		"http 403":  {403, list("1"), false},
		"not json":  {200, "oops", false},
		"empty":     {200, "", false},
	} {
		t.Run(name, func(t *testing.T) {
			f := fakeWith(func(p string, form url.Values) string {
				if p != "/Helper/GetStoreList" || form.Get("CvsType") != "UNIMART" || !VerifyMac(form, tKey, tIV) {
					t.Errorf("bad probe request %s %v", p, form)
				}
				return tc.body
			}, tc.status)
			err := newTestClient(t, EnvSandbox, f).Probe(context.Background(), tCr)
			if (err == nil) != tc.ok || (err != nil && !errors.Is(err, ErrUnavailable)) {
				t.Fatalf("err=%v want ok=%v", err, tc.ok)
			}
		})
	}
	if err := newTestClient(t, EnvSandbox, &fakeECPay{}).Probe(context.Background(), Credentials{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty creds err=%v", err)
	}
}

func TestClientNewClientRejectsEnvironment(t *testing.T) {
	if _, err := NewClient("STAGING", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err=%v", err)
	}
	c, err := NewClient(EnvLive, nil)
	if err != nil || c.String() != "ecpay.Client{redacted}" {
		t.Fatalf("nil transport client: %v %v", c, err)
	}
}

func TestSecretsNeverFormat(t *testing.T) {
	k, _ := LoadKeyring(envOf(ringJSON("k1", "k1")))
	for _, v := range []any{tCr, goodPayload(), k, *k} {
		for _, s := range []string{fmtS(v), fmtV(v), fmtJSON(v)} {
			if strings.Contains(s, tKey) || strings.Contains(s, tIV) || strings.Contains(s, "0987654321") {
				t.Fatalf("secret leaked through %T: %s", v, s)
			}
		}
	}
}

func TestKeyringNeverFormatsKeyBytes(t *testing.T) {
	k, _ := LoadKeyring(envOf(ringJSON("k1", "k1")))
	for _, s := range []string{fmtS(k), fmtV(k), fmtJSON(k), fmtV(*k), fmtS(*k)} {
		if strings.Contains(s, keyB64(1)) || strings.Contains(s, "k1") {
			t.Fatalf("keyring formatted with material: %s", s)
		}
	}
}
