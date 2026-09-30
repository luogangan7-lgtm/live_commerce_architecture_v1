// form_test.go: unit tests for the map/print forms and the map-return / status parsers (F3, F7, F8, X10).
package ecpay

import (
	"crypto/sha256"
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestMapFormFields(t *testing.T) {
	a, f := MapForm(EnvSandbox, "2000933", tTradeNo, "UNIMARTC2C", tHooks, false)
	if a != "https://logistics-stage.ecpay.com.tw/Express/map" || f["Device"] != "0" || f["IsCollection"] != "N" ||
		f["LogisticsType"] != "CVS" || f["ServerReplyURL"] != tHooks || f["MerchantTradeNo"] != tTradeNo || f["LogisticsSubType"] != "UNIMARTC2C" {
		t.Fatalf("%s %v", a, f)
	}
	if _, present := f["CheckMacValue"]; present {
		t.Error("the map request carries no CheckMacValue (F1)")
	}
	a, f = MapForm(EnvLive, "1", tTradeNo, "FAMIC2C", tHooks, true)
	if a != "https://logistics.ecpay.com.tw/Express/map" || f["Device"] != "1" {
		t.Fatalf("%s %v", a, f)
	}
}

func TestMapReturnParse(t *testing.T) {
	ok := "MerchantID=2000933&MerchantTradeNo=" + tTradeNo + "&LogisticsSubType=UNIMARTC2C&CVSStoreID=131386++&CVSStoreName=x&CVSAddress=y&CVSTelephone=&CVSOutSide=0&ExtraData=&Forged=1"
	got, err := ParseMapReturn([]byte(ok))
	want := MapReturn{MerchantID: "2000933", MerchantTradeNo: tTradeNo, SubType: "UNIMARTC2C", StoreID: "131386", Outside: "0"}
	if err != nil || got != want {
		t.Fatalf("got %+v err=%v", got, err)
	}
	lead, err := ParseMapReturn([]byte(strings.Replace(ok, "131386++", "006598", 1)))
	if err != nil || lead.StoreID != "006598" {
		t.Fatalf("leading zeros: %+v %v", lead, err)
	}
	pct, err := ParseMapReturn([]byte(strings.Replace(ok, "131386++", "1328%20", 1)))
	if err != nil || pct.StoreID != "1328" {
		t.Fatalf("percent-encoded padding: %+v %v", pct, err)
	}
	// Shape is the definer's bad_store_id (§4.3 step 3, after the nonce), not a parse error: these reach record_cvs_map_return.
	for _, id := range []string{"1234567890", "12-34"} {
		if got, err := ParseMapReturn([]byte(strings.Replace(ok, "131386++", id, 1))); err != nil || got.StoreID != id {
			t.Errorf("store id %q must pass to the definer: %+v %v", id, got, err)
		}
	}
	for name, body := range map[string]string{
		"empty":       "",
		"duplicate":   ok + "&CVSStoreID=999999",
		"oversize id": strings.Replace(ok, "131386++", strings.Repeat("1", maxMapStoreID+1), 1),
		"no store":    strings.Replace(ok, "&CVSStoreID=131386++", "", 1),
		"bad subtype": strings.Replace(ok, "UNIMARTC2C", "NOPE", 1),
		"bad outside": strings.Replace(ok, "CVSOutSide=0", "CVSOutSide=7", 1),
		"bad trade":   strings.Replace(ok, tTradeNo, "has space", 1),
		"bad merch":   strings.Replace(ok, "2000933", "abc", 1),
		"oversize":    ok + "&X=" + strings.Repeat("a", maxMapReturnBody),
	} {
		if _, err := ParseMapReturn([]byte(body)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err=%v", name, err)
		}
	}
}

func statusForm() url.Values {
	return url.Values{
		"MerchantID": {"2000933"}, "MerchantTradeNo": {tTradeNo}, "RtnCode": {"2030"}, "RtnMsg": {"配送中"},
		"AllPayLogisticsID": {"1234567"}, "LogisticsType": {"CVS"}, "LogisticsSubType": {"UNIMARTC2C"},
		"GoodsAmount": {"350"}, "UpdateStatusDate": {"2026/09/30 13:00:00"},
		"ReceiverName": {"王小明"}, "ReceiverPhone": {"0212345678"}, "ReceiverCellPhone": {"0912345678"},
		"ReceiverEmail": {"pii@example.test"}, "ReceiverAddress": {"某地址"}, "CVSPaymentNo": {"C1234567"},
		"CVSValidationNo": {"9876"}, "BookingNote": {"bn"},
	}
}

func encodedStatus(v url.Values) string {
	v.Set(macField, CheckMac(v, tKey, tIV))
	return v.Encode()
}

func TestStatusParse(t *testing.T) {
	body := encodedStatus(statusForm())
	got, err := ParseStatus([]byte(body), tCr)
	if err != nil {
		t.Fatal(err)
	}
	if got.MerchantID != "2000933" || got.MerchantTradeNo != tTradeNo || got.LogisticsID != "1234567" || got.RtnCode != "2030" ||
		got.RtnMsg != "配送中" || got.UpdateDate != "2026/09/30 13:00:00" || got.PaymentNo != "C1234567" || got.ValidationNo != "9876" {
		t.Fatalf("%+v", got)
	}
	if got.BodySHA256 != sha256.Sum256([]byte(body)) {
		t.Error("BodySHA256 must be over the raw body")
	}
	// PII fields never appear in the report (compile-time: no such fields; runtime: no value copied).
	if s := strings.Join([]string{got.RtnMsg, got.PaymentNo, got.MerchantTradeNo}, "|"); strings.Contains(s, "pii@") || strings.Contains(s, "某地址") {
		t.Error("recipient PII leaked into the report")
	}
	// Extra unknown fields are MAC-covered: tampering with one fails.
	v := statusForm()
	v.Set("Unknown", "a")
	extra := encodedStatus(v)
	if _, err := ParseStatus([]byte(extra), tCr); err != nil {
		t.Fatalf("a signed extra field is accepted then dropped: %v", err)
	}
	if _, err := ParseStatus([]byte(strings.Replace(extra, "Unknown=a", "Unknown=b", 1)), tCr); !errors.Is(err, ErrMAC) {
		t.Errorf("tampered extra field err=%v", err)
	}
	for name, tc := range map[string]struct {
		body []byte
		cr   Credentials
		want error
	}{
		"tampered code":  {[]byte(strings.Replace(body, "RtnCode=2030", "RtnCode=2067", 1)), tCr, ErrMAC},
		"wrong key":      {[]byte(body), Credentials{MerchantID: "2000933", HashKey: tKey + "x", HashIV: tIV}, ErrMAC},
		"wrong merchant": {[]byte(body), Credentials{MerchantID: "2000132", HashKey: tKey, HashIV: tIV}, ErrInvalid},
		"no mac":         {[]byte(strings.Split(body, "&CheckMacValue")[0]), tCr, ErrMAC},
		"duplicate key":  {[]byte(body + "&RtnCode=2067"), tCr, ErrInvalid},
		"oversize":       {[]byte(body + "&X=" + strings.Repeat("a", maxStatusBody)), tCr, ErrInvalid},
		"empty":          {nil, tCr, ErrInvalid},
		"no keys":        {[]byte(body), Credentials{MerchantID: "2000933"}, ErrInvalid},
	} {
		if _, err := ParseStatus(tc.body, tc.cr); !errors.Is(err, tc.want) {
			t.Errorf("%s: err=%v want %v", name, err, tc.want)
		}
	}
	// Control characters and invalid UTF-8 in RtnMsg are made SQL-safe, not rejected.
	v = statusForm()
	v.Set("RtnMsg", "a\x00b\xffc")
	got, err = ParseStatus([]byte(encodedStatus(v)), tCr)
	if err != nil || got.RtnMsg != "abc" {
		t.Fatalf("sanitised RtnMsg %q err=%v", got.RtnMsg, err)
	}
}

func TestPrintFormSigned(t *testing.T) {
	for _, tc := range []struct {
		sub, path string
		keys      []string
	}{
		{"UNIMARTC2C", "/Express/PrintUniMartC2COrderInfo", []string{"CVSPaymentNo", "CVSValidationNo"}},
		{"FAMIC2C", "/Express/PrintFAMIC2COrderInfo", []string{"CVSPaymentNo"}},
		{"HILIFEC2C", "/Express/PrintHILIFEC2COrderInfo", []string{"CVSPaymentNo"}},
		{"UNIMART", "/helper/printTradeDocument", nil},
	} {
		a, f, err := PrintForm(EnvSandbox, tCr, tc.sub, "1234567", "C1234567", "9876", false)
		if err != nil || a != "https://logistics-stage.ecpay.com.tw"+tc.path {
			t.Fatalf("%s: %s %v", tc.sub, a, err)
		}
		v := url.Values{}
		for k, x := range f {
			v.Set(k, x)
		}
		if !VerifyMac(v, tKey, tIV) || f["AllPayLogisticsID"] != "1234567" || f["MerchantID"] != "2000933" {
			t.Errorf("%s: bad form %v", tc.sub, f)
		}
		for _, k := range tc.keys {
			if f[k] == "" {
				t.Errorf("%s: missing %s", tc.sub, k)
			}
		}
		if _, has := f["PrintMode"]; has {
			t.Errorf("%s: PrintMode only when thermal", tc.sub)
		}
	}
	_, f, err := PrintForm(EnvLive, tCr, "FAMIC2C", "1234567", "C1", "", true)
	if err != nil || f["PrintMode"] != "2" {
		t.Fatalf("thermal: %v %v", f, err)
	}
	for name, args := range map[string][]string{
		"okmart":         {"OKMARTC2C", "1234567", "C1", "9"},
		"unknown":        {"NOPE", "1234567", "C1", "9"},
		"bad id":         {"FAMIC2C", "bad id", "C1", "9"},
		"missing pay no": {"FAMIC2C", "1234567", "", "9"},
		"missing valid":  {"UNIMARTC2C", "1234567", "C1", ""},
	} {
		if _, _, err := PrintForm(EnvSandbox, tCr, args[0], args[1], args[2], args[3], false); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err=%v", name, err)
		}
	}
	if _, _, err := PrintForm("X", tCr, "FAMIC2C", "1", "C", "", false); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad env err=%v", err)
	}
}
