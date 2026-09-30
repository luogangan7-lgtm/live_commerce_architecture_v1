// mac_test.go: unit tests for mac.go (F6 MAC, recipient twin, trade number, subtype tables). Test
// names deliberately avoid the TCV gate prefixes owned by cvs-tests.

package ecpay

import (
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestEncodeNETTable(t *testing.T) {
	// Expected values come from an independent Python rendering of the documented .NET table.
	for in, want := range map[string]string{
		"a b!*()-_.~é/&=+": "a+b!*()-_.%7e%c3%a9%2f%26%3d%2b",
		"HashKey=AbC":      "HashKey%3dAbC",
		"商":                "%e5%95%86",
	} {
		if got := encodeNET(in); got != want {
			t.Errorf("encodeNET(%q)=%q want %q", in, got, want)
		}
	}
}

func TestWireMacKnownAnswer(t *testing.T) {
	p := url.Values{
		"MerchantID": {"2000933"}, "MerchantTradeNo": {"LCABCDEFGHIJKLMNOPQR"}, "LogisticsType": {"CVS"},
		"GoodsName": {"商品"}, "ReceiverName": {"王小明"}, "Note": {"a b!*()-_.~"},
		"ServerReplyURL": {"https://hooks.example.test/v1/cvs/ecpay/status/x~y"},
	}
	if got, want := CheckMac(p, tKey, tIV), "4E6DF436F63546B51B764ADDD5938954"; got != want {
		t.Fatalf("CheckMac=%s want %s", got, want)
	}
	// An existing CheckMacValue field never feeds the MAC.
	p.Set("CheckMacValue", "IGNORED")
	if got := CheckMac(p, tKey, tIV); got != "4E6DF436F63546B51B764ADDD5938954" {
		t.Fatalf("CheckMacValue must be excluded, got %s", got)
	}
}

func TestWireMacVerify(t *testing.T) {
	v := url.Values{"MerchantID": {"2000933"}, "RtnCode": {"300"}, "RtnMsg": {"訂單處理中(已收到訂單資料)"}}
	v.Set(macField, CheckMac(v, tKey, tIV))
	if !VerifyMac(v, tKey, tIV) {
		t.Fatal("genuine MAC rejected")
	}
	low := url.Values{}
	for k := range v {
		low[k] = v[k]
	}
	low.Set(macField, strings.ToLower(v.Get(macField)))
	if !VerifyMac(low, tKey, tIV) {
		t.Error("lowercase hex must verify (case-insensitive hex)")
	}
	tamper := url.Values{}
	for k := range v {
		tamper[k] = append([]string(nil), v[k]...)
	}
	tamper.Set("RtnCode", "301")
	if VerifyMac(tamper, tKey, tIV) {
		t.Error("tampered field verified")
	}
	extra := url.Values{}
	for k := range v {
		extra[k] = v[k]
	}
	extra.Set("Unknown", "x")
	if VerifyMac(extra, tKey, tIV) {
		t.Error("an extra field must be MAC-covered")
	}
	if VerifyMac(v, tKey+"x", tIV) || VerifyMac(v, tKey, tIV+"x") {
		t.Error("wrong key or IV verified")
	}
	v.Del(macField)
	if VerifyMac(v, tKey, tIV) {
		t.Error("missing MAC verified")
	}
}

func TestWireMacKeyOrdering(t *testing.T) {
	// CVSPaymentNo vs CollectionAmount is the case-fold vs byte-order difference; signing uses fold,
	// verifying accepts either (documented ambiguity).
	p := url.Values{"CVSPaymentNo": {"1"}, "CollectionAmount": {"5"}}
	if got := CheckMac(p, tKey, tIV); got != "A8DBED8F71900B0D976041B7BDEBFD5D" {
		t.Fatalf("fold order MAC=%s", got)
	}
	p.Set(macField, "BD056F808CA9CB253AC013ECF074846F") // byte-order candidate, from the Python reference
	if !VerifyMac(p, tKey, tIV) {
		t.Error("byte-order candidate must verify")
	}
}

func TestRecipientRule(t *testing.T) {
	cases := []struct {
		name, phone string
		want        string // "" = refused
	}{
		{"王小明", "0912345678", "0912345678"},
		{"王小明", "+886912345678", "0912345678"},
		{"王小明", " 0912-345 (678) ", "0912345678"},
		{"陳大", "0912345678", "0912345678"},    // 2 CJK = width 4
		{"王小明明明", "0912345678", "0912345678"}, // 5 CJK = width 10
		{"王小明明明明", "0912345678", ""},          // width 12
		{"王", "0912345678", ""},               // width 2
		{"Anna", "0912345678", "0912345678"},  // ASCII letters width 1 -> 4
		{"Ann", "0912345678", ""},             // width 3
		{"王小明1", "0912345678", ""},            // digit
		{"王小明😀", "0912345678", ""},            // emoji
		{"王小 明", "0912345678", ""},            // space is a symbol
		{"王小明!", "0912345678", ""},            // ASCII symbol
		{"ＡＢ", "0912345678", "0912345678"},    // full-width letters width 2 each
		{"王小明", "0212345678", ""},             // landline
		{"王小明", "091234567", ""},              // 9 digits
		{"王小明", "09123456789", ""},            // 11 digits
		{"王小明", "+8860912345678", ""},         // trunk 0 after +886
		{"王小明", "+85291234567", ""},           // overseas
		{"王小明", "０９１２３４５６７８", ""},             // full-width digits
		{"", "0912345678", ""},
	}
	for _, c := range cases {
		got, ok := RecipientOK(c.name, c.phone)
		if (c.want != "") != ok || got != c.want {
			t.Errorf("RecipientOK(%q,%q)=(%q,%v) want %q", c.name, c.phone, got, ok, c.want)
		}
	}
}

func TestTradeNoDerivation(t *testing.T) {
	re := regexp.MustCompile(`^LC[A-Z2-7]{18}$`)
	id := "0f8fad5b-d9cb-469f-a165-70867728950e"
	a := MerchantTradeNo(id)
	if !re.MatchString(a) || a != MerchantTradeNo(strings.ToUpper(id)) || a == MerchantTradeNo("1f8fad5b-d9cb-469f-a165-70867728950e") {
		t.Fatalf("trade no %q must match the pattern, ignore case and differ per operation", a)
	}
}

func TestSubtypeTables(t *testing.T) {
	for in, want := range map[string]string{"UNIMARTC2C": "UNIMART", "UNIMART": "UNIMART", "FAMIC2C": "FAMI", "FAMI": "FAMI", "HILIFEC2C": "HILIFE", "HILIFE": "HILIFE"} {
		if got, err := CVSType(in); err != nil || got != want {
			t.Errorf("CVSType(%s)=%q,%v", in, got, err)
		}
	}
	if _, err := CVSType("OKMARTC2C"); err != ErrNoDirectory {
		t.Errorf("OKMARTC2C err=%v", err)
	}
	for _, bad := range []string{"", "UNIMARTFREEZE", "unimart", "CVS"} {
		if _, err := CVSType(bad); err != ErrInvalid {
			t.Errorf("CVSType(%q) err=%v", bad, err)
		}
	}
}
