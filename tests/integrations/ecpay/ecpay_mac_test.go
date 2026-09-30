package ecpay_test

// TCV01 TestEcpayMac (contracts/taiwan-cvs-logistics-v1.md §10 TCV01, §7.1, §4.3 ecpay_recipient_ok, §16.1 store-code table, C2/E2).
// Tier UNIT. Written from the contract and the ECPay documents (7424 CheckMacValue, retrieved 2026-09-30), not from the adapter.
// Exercises: ecpay.CheckMac, ecpay.VerifyMac, ecpay.RecipientOK, ecpay.MerchantTradeNo, fulfillment.ValidBuyerStoreCode.
// The published ECPay stage keys are written split (PROCESS §6).

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"livecommerce/internal/fulfillment"
	"livecommerce/internal/integrations/shipping/ecpay"
	"livecommerce/internal/integrations/shipping/ecpay/ecpaytest"
)

const (
	keyC2C = "XBERn1YO" + "vpM9nfZc" // ECPay docs 7424 published C2C test HashKey, split per PROCESS section 6
	ivC2C  = "h1ONHk4P" + "4yqbl5LK"
)

// official7424 is the worked example of https://developers.ecpay.com.tw/7424/ (retrieved 2026-09-30).
func official7424() url.Values {
	return url.Values{
		"MerchantID": {"2000933"}, "MerchantTradeNo": {"A20130312153023"}, "MerchantTradeDate": {"2013/03/12 15:30:23"},
		"LogisticsType": {"CVS"}, "LogisticsSubType": {"FAMIC2C"}, "GoodsAmount": {"1000"}, "IsCollection": {"N"},
		"SenderName": {"寄件者姓名"}, "ReceiverName": {"收件者姓名"}, "ReceiverStoreID": {"001779"},
		"ServerReplyURL": {"https://www.ecpay.com.tw/ServerReplyURL"},
	}
}

func md5Upper(s string) string {
	h := md5.Sum([]byte(s))
	return strings.ToUpper(hex.EncodeToString(h[:]))
}

func TestEcpayMac(t *testing.T) {
	const want = "692FD6E2CDB539CCDB7206C76DC239AD"

	t.Run("F6 official vector", func(t *testing.T) {
		if got := ecpay.CheckMac(official7424(), keyC2C, ivC2C); got != want {
			t.Fatalf("CheckMac = %s, want the ECPay 7424 example %s", got, want)
		}
		// Independent of the adapter: the fake's separate implementation must agree.
		if got := ecpaytest.Mac(official7424(), keyC2C, ivC2C); got != want {
			t.Fatalf("fake Mac = %s (the fake drifted from the docs)", got)
		}
		// the field CheckMacValue itself is never part of the digest
		v := official7424()
		v.Set("CheckMacValue", "IGNORED")
		if got := ecpay.CheckMac(v, keyC2C, ivC2C); got != want {
			t.Fatalf("CheckMacValue must be excluded from its own digest, got %s", got)
		}
	})

	t.Run(".NET encode table", func(t *testing.T) {
		// Hand-written expectations: the string that is hashed is lower-cased after encoding, so the hex of an encoded byte is
		// lower case and "-_.!*()" stay literal (docs 7424 note 5), space is "+", "~" is %7e in .NET.
		cases := []struct{ value, enc string }{
			{"a b", "a+b"}, {"!*()-_.", "!*()-_."}, {"~", "%7e"}, {"/", "%2f"}, {":", "%3a"}, {"&", "%26"}, {"=", "%3d"},
			{"+", "%2b"}, {"%", "%25"}, {"#", "%23"}, {"?", "%3f"}, {"@", "%40"}, {"'", "%27"}, {`"`, "%22"}, {"é", "%c3%a9"}, {"王", "%e7%8e%8b"},
			{"ABC", "abc"}, {"A/B C", "a%2fb+c"},
		}
		for _, c := range cases {
			v := url.Values{"p": {c.value}}
			hashed := "hashkey%3d" + strings.ToLower(keyC2C) + "%26p%3d" + c.enc + "%26hashiv%3d" + strings.ToLower(ivC2C)
			if got, w := ecpay.CheckMac(v, keyC2C, ivC2C), md5Upper(hashed); got != w {
				t.Errorf("value %q: CheckMac %s, expected digest of %q = %s", c.value, got, hashed, w)
			}
		}
	})

	t.Run("verify accepts the genuine, refuses tampering", func(t *testing.T) {
		v := official7424()
		v.Set("CheckMacValue", want)
		if !ecpay.VerifyMac(v, keyC2C, ivC2C) {
			t.Fatal("the official example must verify")
		}
		for name, mut := range map[string]func(url.Values){
			"field value changed": func(v url.Values) { v.Set("GoodsAmount", "1001") },
			"field removed":       func(v url.Values) { v.Del("IsCollection") },
			"field added":         func(v url.Values) { v.Set("Extra", "1") },
			"mac blanked":         func(v url.Values) { v.Set("CheckMacValue", "") },
			"mac removed":         func(v url.Values) { v.Del("CheckMacValue") },
			"mac one char off":    func(v url.Values) { v.Set("CheckMacValue", want[:31]+"0") },
			"mac truncated":       func(v url.Values) { v.Set("CheckMacValue", want[:16]) },
		} {
			c := url.Values{}
			for k, vs := range v {
				c[k] = append([]string(nil), vs...)
			}
			mut(c)
			if ecpay.VerifyMac(c, keyC2C, ivC2C) {
				t.Errorf("%s: verification must fail", name)
			}
		}
		if ecpay.VerifyMac(v, keyC2C+"x", ivC2C) || ecpay.VerifyMac(v, keyC2C, ivC2C+"x") || ecpay.VerifyMac(v, ivC2C, keyC2C) {
			t.Error("a wrong or swapped HashKey/HashIV must fail")
		}
	})

	t.Run("constant-time comparison is used (source guard)", func(t *testing.T) {
		// A timing property cannot be observed in a unit test; §7.1 mandates subtle.ConstantTimeCompare, so guard the source.
		src, err := os.ReadFile("../../../internal/integrations/shipping/ecpay/mac.go")
		if err != nil {
			t.Fatal(err)
		}
		s := string(src)
		if !strings.Contains(s, "subtle.ConstantTimeCompare") {
			t.Fatal("mac.go must verify with subtle.ConstantTimeCompare (§7.1)")
		}
		if regexp.MustCompile(`(?m)got\s*==\s*\w*[Dd]igest|== *CheckMac\(`).MatchString(s) {
			t.Fatal("mac.go compares a MAC with ==")
		}
	})

	t.Run("recipient golden table", func(t *testing.T) {
		var rows []struct {
			Why             string `json:"why"`
			Name            string `json:"name"`
			Phone           string `json:"phone"`
			PhoneNormalized string `json:"phone_normalized"`
			OK              bool   `json:"ok"`
		}
		mustJSON(t, "testdata/recipient.json", &rows)
		if len(rows) < 20 {
			t.Fatal("golden table shrank")
		}
		for _, r := range rows {
			got, ok := ecpay.RecipientOK(r.Name, r.Phone)
			if ok != r.OK {
				t.Errorf("%s: RecipientOK(%q,%q) ok=%v want %v", r.Why, r.Name, r.Phone, ok, r.OK)
			}
			if r.OK && got != r.PhoneNormalized {
				t.Errorf("%s: normalised phone %q want %q", r.Why, got, r.PhoneNormalized)
			}
		}
	})

	t.Run("store code golden table", func(t *testing.T) {
		var rows []struct {
			Kind string `json:"kind"`
			Code string `json:"code"`
			OK   bool   `json:"ok"`
		}
		mustJSON(t, "testdata/store_code.json", &rows)
		for _, r := range rows {
			if got := fulfillment.ValidBuyerStoreCode(r.Kind, r.Code); got != r.OK {
				t.Errorf("ValidBuyerStoreCode(%s,%q)=%v want %v", r.Kind, r.Code, got, r.OK)
			}
		}
	})

	t.Run("trade number", func(t *testing.T) {
		re := regexp.MustCompile(`^LC[A-Z2-7]{18}$`)
		seen := map[string]string{}
		for _, id := range []string{"00000000-0000-4000-8000-000000000000", "11111111-1111-4111-8111-111111111111", "c0ffee00-1234-4abc-9def-0123456789ab",
			"C0FFEE00-1234-4ABC-9DEF-0123456789AB", "ffffffff-ffff-4fff-bfff-ffffffffffff"} {
			tn := ecpay.MerchantTradeNo(id)
			if !re.MatchString(tn) {
				t.Errorf("%s -> %q does not match ^LC[A-Z2-7]{18}$", id, tn)
			}
			if again := ecpay.MerchantTradeNo(id); again != tn {
				t.Errorf("not deterministic for %s", id)
			}
			if prev, dup := seen[tn]; dup && !strings.EqualFold(prev, id) {
				t.Errorf("collision %s / %s", prev, id)
			}
			seen[tn] = id
		}
		// the same uuid in either case is one operation (uuid text is case-insensitive)
		if ecpay.MerchantTradeNo("c0ffee00-1234-4abc-9def-0123456789ab") != ecpay.MerchantTradeNo("C0FFEE00-1234-4ABC-9DEF-0123456789AB") {
			t.Error("a UUID differing only in case must derive the same trade number (one operation, one MerchantTradeNo)")
		}
	})
}

func mustJSON(t *testing.T, path string, into any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, into); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}
