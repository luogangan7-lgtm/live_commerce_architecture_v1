package ecpaytest

// Self-tests of the fake (gate unit cvs-tests). They pin the fake to the ECPay documents (official CheckMacValue vector, wire shapes)
// so a test that passes against it is not passing against a fake that drifted from the docs. Published stage keys are written split
// (PROCESS §6).

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

var stage = Merchant{ID: "2000933", Key: "XBERn1YO" + "vpM9nfZc", IV: "h1ONHk4P" + "4yqbl5LK"}

func do(t *testing.T, f *Fake, host, path string, form url.Values) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", "https://"+host+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := f.Transport().RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func signed(m Merchant, v url.Values) url.Values {
	v.Set("MerchantID", m.ID)
	v.Set("CheckMacValue", Mac(v, m.Key, m.IV))
	return v
}

func createForm(tn string) url.Values {
	return url.Values{"MerchantTradeNo": {tn}, "MerchantTradeDate": {"2026/09/30 10:00:00"}, "LogisticsType": {"CVS"}, "LogisticsSubType": {"UNIMARTC2C"},
		"GoodsAmount": {"1000"}, "IsCollection": {"Y"}, "CollectionAmount": {"1000"}, "ReceiverStoreID": {"131386"}, "ReceiverName": {"王小明先生"},
		"ReceiverCellPhone": {"0900000000"}, "ServerReplyURL": {"https://hooks.example.invalid/v1/cvs/ecpay/status/x"}}
}

func TestMacOfficialVector(t *testing.T) {
	v := url.Values{"MerchantID": {"2000933"}, "MerchantTradeNo": {"A20130312153023"}, "MerchantTradeDate": {"2013/03/12 15:30:23"},
		"LogisticsType": {"CVS"}, "LogisticsSubType": {"FAMIC2C"}, "GoodsAmount": {"1000"}, "IsCollection": {"N"}, "SenderName": {"寄件者姓名"},
		"ReceiverName": {"收件者姓名"}, "ReceiverStoreID": {"001779"}, "ServerReplyURL": {"https://www.ecpay.com.tw/ServerReplyURL"}}
	if got := Mac(v, stage.Key, stage.IV); got != "692FD6E2CDB539CCDB7206C76DC239AD" {
		t.Fatalf("official ECPay 7424 vector: got %s", got)
	}
}

func TestCreateQueryCountersAndModes(t *testing.T) {
	f := New()
	f.AddMerchant(stage)
	host := "logistics-stage.ecpay.com.tw"
	code, body := do(t, f, host, "/Express/Create", signed(stage, createForm("LCAAAAAAAAAAAAAAAAAA")))
	fields, ok := ParseCreateBody(body)
	if code != 200 || !ok || Mac(fields, stage.Key, stage.IV) != fields.Get("CheckMacValue") || fields.Get("AllPayLogisticsID") == "" {
		t.Fatalf("create: %d %q", code, body)
	}
	if tr, _ := f.Trade("LCAAAAAAAAAAAAAAAAAA"); tr.IsCollection != "Y" || tr.CollectionAmount != "1000" || tr.Env != "SANDBOX" || f.CreateCalls("LCAAAAAAAAAAAAAAAAAA") != 1 {
		t.Fatalf("trade record %+v", tr)
	}
	// bad request MAC -> "0|"
	bad := createForm("LCBBBBBBBBBBBBBBBBBB")
	bad.Set("MerchantID", stage.ID)
	bad.Set("CheckMacValue", "0000")
	if _, b := do(t, f, host, "/Express/Create", bad); !strings.HasPrefix(b, "0|") || f.DistinctTradeNos() != 1 {
		t.Fatalf("unsigned create must be refused and unrecorded: %q", b)
	}
	// query by trade no, MAC-valid response, then not found
	q := signed(stage, url.Values{"MerchantTradeNo": {"LCAAAAAAAAAAAAAAAAAA"}, "TimeStamp": {itoa(time.Now().Unix())}})
	_, qb := do(t, f, host, "/Helper/QueryLogisticsTradeInfo/V5", q)
	qv, _ := url.ParseQuery(qb)
	if qv.Get("AllPayLogisticsID") != fields.Get("AllPayLogisticsID") || Mac(qv, stage.Key, stage.IV) != qv.Get("CheckMacValue") || qv.Get("LogisticsStatus") != "300" {
		t.Fatalf("query: %q", qb)
	}
	old := signed(stage, url.Values{"MerchantTradeNo": {"LCAAAAAAAAAAAAAAAAAA"}, "TimeStamp": {itoa(time.Now().Add(-10 * time.Minute).Unix())}})
	if _, b := do(t, f, host, "/Helper/QueryLogisticsTradeInfo/V5", old); !strings.HasPrefix(b, "0|") {
		t.Fatalf("stale TimeStamp must be refused: %q", b)
	}
	// modes
	f.SetCreateMode(Create403, "")
	if c, _ := do(t, f, host, "/Express/Create", signed(stage, createForm("LCCCCCCCCCCCCCCCCCCC"))); c != 403 || f.DistinctTradeNos() != 1 {
		t.Fatalf("403 mode records nothing: %d", c)
	}
	f.SetCreateMode(CreateBadMAC, "")
	_, b := do(t, f, host, "/Express/Create", signed(stage, createForm("LCDDDDDDDDDDDDDDDDDD")))
	if bv, _ := ParseCreateBody(b); Mac(bv, stage.Key, stage.IV) == bv.Get("CheckMacValue") || f.DistinctTradeNos() != 2 {
		t.Fatalf("bad-MAC mode must sign wrongly and record: %q", b)
	}
	f.SetCreateMode(CreateReject, "balance too low")
	if _, b := do(t, f, host, "/Express/Create", signed(stage, createForm("LCEEEEEEEEEEEEEEEEEE"))); b != "0|balance too low" {
		t.Fatalf("reject: %q", b)
	}
	f.SetCreateMode(CreateTimeoutLost, "")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://"+host+"/Express/Create", strings.NewReader(signed(stage, createForm("LCFFFFFFFFFFFFFFFFFF")).Encode()))
	if _, err := f.Transport().RoundTrip(req); err == nil {
		t.Fatal("timeout mode must block until the context ends")
	}
	if _, ok := f.Trade("LCFFFFFFFFFFFFFFFFFF"); !ok {
		t.Fatal("timeout-lost mode records the trade")
	}
	f.SetCreateMode(CreateOK, "")
	f.SetCodes("", strings.Repeat("9", 41), "")
	_, b = do(t, f, host, "/Express/Create", signed(stage, createForm("LCGGGGGGGGGGGGGGGGGG")))
	if bv, _ := ParseCreateBody(b); len(bv.Get("CVSPaymentNo")) != 41 {
		t.Fatalf("41-char CVSPaymentNo: %q", b)
	}
	if _, err := f.Transport().RoundTrip(mustReq("https://evil.example.invalid/Express/Create")); err == nil {
		t.Fatal("a foreign host must fail, never pass through")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func mustReq(u string) *http.Request {
	r, _ := http.NewRequest("POST", u, strings.NewReader(""))
	return r
}

func TestDirectoryMapPrintAndStatusSigning(t *testing.T) {
	f := New()
	f.AddMerchant(stage)
	host := "logistics-stage.ecpay.com.tw"
	f.SetStoreList("UNIMART", []Store{{ID: "131386", Name: "a", Addr: "b", Phone: "c"}})
	f.SetStoreList("FAMI", []Store{{ID: "006598", Name: "d", Addr: "e"}})
	_, b := do(t, f, host, "/Helper/GetStoreList", signed(stage, url.Values{"CvsType": {"UNIMART"}}))
	if !strings.Contains(b, `"StoreId":"131386"`) || strings.Contains(b, "006598") || !strings.Contains(b, `"RtnCode":1`) {
		t.Fatalf("UNIMART list: %s", b)
	}
	_, b = do(t, f, host, "/Helper/GetStoreList", signed(stage, url.Values{"CvsType": {"All"}}))
	if !strings.Contains(b, "006598") || !strings.Contains(b, "131386") {
		t.Fatalf("All list: %s", b)
	}
	_, b = do(t, f, host, "/Helper/GetStoreList", url.Values{"MerchantID": {stage.ID}, "CvsType": {"All"}, "CheckMacValue": {"00"}})
	if !strings.Contains(b, `"RtnCode":10200073`) || strings.Contains(b, "131386") {
		t.Fatalf("unsigned list must be refused: %s", b)
	}
	if len(BigStoreList(20000)) != 20000 {
		t.Fatal("big list")
	}
	mv := f.MapReturn(url.Values{"MerchantID": {stage.ID}, "MerchantTradeNo": {"n"}, "LogisticsSubType": {"UNIMARTC2C"}})
	if mv.Get("CVSStoreID") != "131386" || mv.Get("CheckMacValue") != "" {
		t.Fatalf("map return has no MAC (F3): %v", mv)
	}
	if c, _ := do(t, f, host, "/Express/PrintUniMartC2COrderInfo", signed(stage, url.Values{"AllPayLogisticsID": {"1"}})); c != 200 {
		t.Fatalf("print: %d", c)
	}
	tr := Trade{MerchantID: stage.ID, TradeNo: "LCX", LogisticsID: "5", SubType: "UNIMARTC2C", GoodsAmount: "100", PaymentNo: "1"}
	s := SignStatus(stage, StatusFields(tr, "2030", "2026/09/30 10:00:00", "SENTINELNAME"))
	if s.Get("CheckMacValue") == "" || Mac(s, stage.Key, stage.IV) != s.Get("CheckMacValue") {
		t.Fatal("status signing")
	}
	s.Set("Extra", "x")
	if Mac(s, stage.Key, stage.IV) == s.Get("CheckMacValue") {
		t.Fatal("an added field must break the MAC")
	}
	if f.CountCalls("GetStoreList") != 3 {
		t.Fatalf("calls %v", f.Calls())
	}
}

func TestHandlerServesOverRealHTTP(t *testing.T) {
	f := New()
	f.AddMerchant(stage)
	f.SetStoreList("UNIMART", []Store{{ID: "131386", Name: "a", Addr: "b"}})
	srv := httptest.NewServer(f.Handler())
	defer srv.Close()
	rt := ForwardTransport(srv.URL)
	form := signed(stage, url.Values{"CvsType": {"UNIMART"}})
	req, _ := http.NewRequest("POST", "https://logistics-stage.ecpay.com.tw/Helper/GetStoreList", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"StoreId":"131386"`) {
		t.Fatalf("over HTTP: %s", b)
	}
	if got := f.Calls(); len(got) != 1 || got[0].Env != "SANDBOX" {
		t.Fatalf("the environment follows the dialled host: %v", got)
	}
}
