// form.go: the browser-facing forms (e-map request, print) and the parsers of the two unsigned or
// provider-signed inbound bodies (map return, status notification). No I/O.
//
// The e-map request and return carry no CheckMacValue (F1/F3): the return is browser-forgeable, so
// ParseMapReturn yields only identifiers and the trusted name/address always come from the directory
// (TD3). The status notification is MAC-verified with the connection's keys (F7, TD7).

package ecpay

import (
	"crypto/sha256"
	"net/url"
	"regexp"
	"strings"
)

const (
	maxMapReturnBody = 8 << 10
	maxStatusBody    = 16 << 10
)

var (
	merchantIDRE = regexp.MustCompile(`^[0-9]{1,10}$`)
	tradeNoRE    = regexp.MustCompile(`^[A-Za-z0-9]{1,20}$`)
	logisticsRE  = regexp.MustCompile(`^[0-9A-Za-z_-]{1,40}$`)
	paymentNoRE  = regexp.MustCompile(`^[0-9A-Za-z]{1,32}$`)
)

// MapForm returns the e-map form action and fields the buyer's browser POSTs (same tab, no iframe,
// F4). IsCollection is always "N": store selection precedes the payment choice (§16.3). Device is "1"
// for a mobile user agent and "0" otherwise (B20/E6). The caller validates its inputs.
func MapForm(env Environment, merchantID, tradeNo, subType, serverReplyURL string, mobile bool) (action string, fields map[string]string) {
	host, _ := hostFor(env)
	device := "0"
	if mobile {
		device = "1"
	}
	return host + pathMap, map[string]string{
		"MerchantID":       merchantID,
		"MerchantTradeNo":  tradeNo,
		"LogisticsType":    "CVS",
		"LogisticsSubType": subType,
		"IsCollection":     "N",
		"ServerReplyURL":   serverReplyURL,
		"Device":           device,
	}
}

// PrintForm returns the signed print form for a created shipment (F8; the browser opens it in a new
// tab, never an iframe). C2C forms need CVSPaymentNo (7-ELEVEN also CVSValidationNo); B2C forms
// print by AllPayLogisticsID. thermal adds PrintMode=2 (A6, contract §8). OKMARTC2C has no print API
// and any unknown subtype is ErrInvalid.
// ponytail: PrintMode=2 is contract text, unverified against a stage response until TCV07.
func PrintForm(env Environment, cr Credentials, subType, logisticsID, paymentNo, validationNo string, thermal bool) (action string, fields map[string]string, err error) {
	host, ok := hostFor(env)
	path := printPaths[subType]
	if !ok || path == "" || !merchantIDRE.MatchString(cr.MerchantID) || cr.HashKey == "" || cr.HashIV == "" || !logisticsRE.MatchString(logisticsID) {
		return "", nil, ErrInvalid
	}
	v := url.Values{"MerchantID": {cr.MerchantID}, "AllPayLogisticsID": {logisticsID}}
	if isC2C(subType) {
		if !paymentNoRE.MatchString(paymentNo) {
			return "", nil, ErrInvalid
		}
		v.Set("CVSPaymentNo", paymentNo)
		if subType == "UNIMARTC2C" {
			if !paymentNoRE.MatchString(validationNo) {
				return "", nil, ErrInvalid
			}
			v.Set("CVSValidationNo", validationNo)
		}
	}
	if thermal {
		v.Set("PrintMode", "2")
	}
	v.Set(macField, CheckMac(v, cr.HashKey, cr.HashIV))
	fields = make(map[string]string, len(v))
	for k := range v {
		fields[k] = v.Get(k)
	}
	return host + path, fields, nil
}

// maxMapStoreID only bounds the lookup key. The store-id shape (F3 S(9) alnum) is decided by fulfillment.record_cvs_map_return
// AFTER its nonce check (contract §4.3 check order), so a malformed id REJECTs the selection with bad_store_id instead of
// leaving it OPEN (TCV03).
const maxMapStoreID = 32

// ParseMapReturn parses the e-map return (the browser's auto-POST to our ServerReplyURL): at most
// 8 KiB, duplicate keys are ErrInvalid, unknown fields are dropped. Trailing spaces are trimmed from
// values (7-ELEVEN pads them, X10) and the store id stays a string with its leading zeros. The
// result is unsigned data: it is a lookup key, never a trusted store description.
func ParseMapReturn(body []byte) (MapReturn, error) {
	if len(body) == 0 || len(body) > maxMapReturnBody {
		return MapReturn{}, ErrInvalid
	}
	v, err := parseKV(string(body))
	if err != nil {
		return MapReturn{}, err
	}
	get := func(k string) string { return strings.TrimRight(clean(v.Get(k), 256), " ") }
	out := MapReturn{
		MerchantID: get("MerchantID"), MerchantTradeNo: get("MerchantTradeNo"), SubType: get("LogisticsSubType"),
		StoreID: get("CVSStoreID"), Outside: get("CVSOutSide"),
	}
	if _, known := subTypes[out.SubType]; !known || !merchantIDRE.MatchString(out.MerchantID) ||
		!tradeNoRE.MatchString(out.MerchantTradeNo) || len(out.StoreID) == 0 || len(out.StoreID) > maxMapStoreID ||
		(out.Outside != "" && out.Outside != "0" && out.Outside != "1") {
		return MapReturn{}, ErrInvalid
	}
	return out, nil
}

// ParseStatus parses and authenticates a status notification (F7): at most 16 KiB, duplicate keys
// are ErrInvalid, the MAC covers every received field (ErrMAC), and MerchantID must equal the
// connection's. Only the F7 fields we persist are returned; recipient PII fields are dropped here,
// after MAC verification (TD8, I11). BodySHA256 is over the raw bytes so ECPay's identical retries
// dedupe.
func ParseStatus(body []byte, c Credentials) (StatusReport, error) {
	if len(body) == 0 || len(body) > maxStatusBody || c.HashKey == "" || c.HashIV == "" {
		return StatusReport{}, ErrInvalid
	}
	v, err := parseKV(string(body))
	if err != nil {
		return StatusReport{}, err
	}
	if !VerifyMac(v, c.HashKey, c.HashIV) {
		return StatusReport{}, ErrMAC
	}
	if v.Get("MerchantID") != c.MerchantID {
		return StatusReport{}, ErrInvalid
	}
	return StatusReport{
		MerchantID:      clean(v.Get("MerchantID"), 32),
		MerchantTradeNo: clean(v.Get("MerchantTradeNo"), 64),
		LogisticsID:     clean(v.Get("AllPayLogisticsID"), 64),
		RtnCode:         clean(v.Get("RtnCode"), 32),
		RtnMsg:          clean(v.Get("RtnMsg"), 1024),
		UpdateDate:      clean(v.Get("UpdateStatusDate"), 64),
		PaymentNo:       clean(v.Get("CVSPaymentNo"), 256),
		ValidationNo:    clean(v.Get("CVSValidationNo"), 256),
		BodySHA256:      sha256.Sum256(body),
	}, nil
}
