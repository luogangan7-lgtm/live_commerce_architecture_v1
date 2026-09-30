// client.go: the ECPay HTTP client (connect probe, Express/Create, Query/V5). One Client serves one
// environment. It never retries: Create is classified once and an UNKNOWN result may only be followed
// by a query, never a second create (I06); Query is read-only.
//
// Transport rules (contract §7.1, F11): TLS >= 1.2, no cookie jar, no redirect following (a 3xx is an
// unexpected answer, so UNKNOWN), 10 s per call, response cap 256 KiB except GetStoreList (32 MiB,
// 30 s, streamed). Nothing here logs a body, key, recipient field, trade number or code.

package ecpay

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	callTimeout      = 10 * time.Second
	storeListTimeout = 30 * time.Second
	maxResponseBody  = 256 << 10
	maxStoreListBody = 32 << 20
	goodsName        = "商品" // fixed, symbol-safe, PII-free (§7.1); never a product title
	userAgent        = "livecommerce-ecpay/1"
)

// Result codes (E7). They match ^[a-z0-9_.]{1,80}$ and are recorded as operation result codes.
const (
	CodeCreated        = "ecpay.created"
	CodeQueryFound     = "ecpay.query_found"
	CodeRejected       = "ecpay.rejected"        // 0|msg: nothing was bought
	CodeInvalidRequest = "ecpay.invalid_request" // failed local F5 validation: nothing was sent
	CodeRateLimited    = "ecpay.rate_limited"    // HTTP 403 (F11: 30 min ban)
	CodeUncertain      = "ecpay.uncertain"       // bad MAC, malformed, timeout, 5xx, anything else
)

// Client performs the ECPay calls of one environment.
type Client struct {
	env  Environment
	host string
	hc   *http.Client
	now  func() time.Time // tests only

	mu   sync.Mutex
	dirs map[string]*dirEntry // by CvsType; the environment is fixed per Client (§7.2)
}

func (*Client) String() string { return "ecpay.Client{redacted}" }

// Environment reports the environment the client talks to. The dispatcher route compares it with
// the environment of the credential it loaded so a LIVE credential can never be sent to the stage
// host (additive to the frozen interface).
func (c *Client) Environment() Environment { return c.env }

// NewClient returns a client for env (SANDBOX or LIVE). rt nil selects a TLS >= 1.2 transport; tests
// pass a fake's transport (the fake answers for the ECPay hostnames, nothing leaves the process).
func NewClient(env Environment, rt http.RoundTripper) (*Client, error) {
	host, ok := hostFor(env)
	if !ok {
		return nil, ErrInvalid
	}
	if rt == nil {
		rt = &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout: callTimeout,
			MaxIdleConnsPerHost: 4,
		}
	}
	return &Client{
		env: env, host: host, now: time.Now, dirs: map[string]*dirEntry{},
		hc: &http.Client{
			Transport:     rt,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// post sends one form POST. The caller closes resp.Body and calls cancel. A transport error, a
// timeout or a cancelled context are all reported as err: the request may or may not have landed.
func (c *Client) post(ctx context.Context, timeout time.Duration, path string, form url.Values) (*http.Response, context.CancelFunc, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, c.host+path, strings.NewReader(form.Encode()))
	if err != nil {
		cancel()
		return nil, nil, ErrInvalid
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.hc.Do(req)
	if err != nil {
		cancel()
		return nil, nil, ErrUnavailable // deliberately drops the error text (may embed the URL)
	}
	return resp, cancel, nil
}

// readBody reads at most max bytes; a longer body is an error (never a truncated "success").
func readBody(r io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil || int64(len(b)) > max {
		return nil, ErrUnavailable
	}
	return b, nil
}

func unknown(code string) Result { return Result{Outcome: "UNKNOWN", Code: code} }

// classifyStatus maps a non-200 HTTP status to the UNKNOWN code; ok is true for 200.
func classifyStatus(status int) (code string, ok bool) {
	switch status {
	case http.StatusOK:
		return "", true
	case http.StatusForbidden:
		return CodeRateLimited, false
	}
	return CodeUncertain, false
}

var (
	tradeDateRE = regexp.MustCompile(`^[0-9]{4}/[0-9]{2}/[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}$`)
	storeCodeRE = regexp.MustCompile(`^[0-9A-Za-z]{1,6}$`) // F17 S(6) is a maximum, not an exact length
)

// createForm validates r and p against F5 and builds the signed Create form. ok=false means nothing
// may be sent. Sender fields come from the sealed payload; SenderCellPhone is required for the
// C2C chains that need it (F5).
func createForm(cr Credentials, p Payload, r CreateRequest) (url.Values, bool) {
	phone, pok := normalizePhone(r.ReceiverPhone)
	sender := ""
	if p.SenderCellPhone != "" {
		if n, ok := normalizePhone(p.SenderCellPhone); ok {
			sender = n
		}
	}
	needSenderPhone := r.SubType == "UNIMARTC2C" || r.SubType == "HILIFEC2C" || r.SubType == "OKMARTC2C"
	switch {
	case !merchantIDRE.MatchString(cr.MerchantID) || cr.HashKey == "" || cr.HashIV == "":
	case subTypes[r.SubType] == "" && r.SubType != "OKMARTC2C":
	case !tradeNoRE.MatchString(r.MerchantTradeNo) || !tradeDateRE.MatchString(r.MerchantTradeDate):
	case !storeCodeRE.MatchString(r.ReceiverStoreID) || !pok || !nameOK(r.ReceiverName):
	case !nameOK(p.SenderName) || (p.SenderCellPhone != "" && sender == "") || (needSenderPhone && sender == ""):
	case r.GoodsAmount < 1 || r.GoodsAmount > 20000:
	case r.CollectionAmount != 0 && r.CollectionAmount != r.GoodsAmount: // F20; also bounds it to 1..20000
	case !strings.HasPrefix(r.ServerReplyURL, "https://") || len(r.ServerReplyURL) > 200 || !isASCII(r.ServerReplyURL):
	default:
		v := url.Values{
			"MerchantID":        {cr.MerchantID},
			"MerchantTradeNo":   {r.MerchantTradeNo},
			"MerchantTradeDate": {r.MerchantTradeDate},
			"LogisticsType":     {"CVS"},
			"LogisticsSubType":  {r.SubType},
			"GoodsAmount":       {strconv.Itoa(r.GoodsAmount)},
			"IsCollection":      {"N"},
			"GoodsName":         {goodsName},
			"SenderName":        {p.SenderName},
			"ReceiverName":      {r.ReceiverName},
			"ReceiverCellPhone": {phone},
			"ReceiverStoreID":   {r.ReceiverStoreID},
			"ServerReplyURL":    {r.ServerReplyURL},
			"PlatformID":        {""}, // blank for ordinary merchants (F5)
		}
		if sender != "" {
			v.Set("SenderCellPhone", sender)
		}
		if r.CollectionAmount > 0 {
			v.Set("IsCollection", "Y")
			v.Set("CollectionAmount", strconv.Itoa(r.CollectionAmount))
		}
		v.Set(macField, CheckMac(v, cr.HashKey, cr.HashIV))
		return v, true
	}
	return nil, false
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// Create sends Express/Create exactly once and classifies the answer (§7.4, E7):
//   - local F5 validation failure: FAILED_FINAL ecpay.invalid_request, nothing sent;
//   - "0|msg": FAILED_FINAL ecpay.rejected (the label was not bought; msg text is discarded);
//   - "1|k=v&..." with a valid MAC, our MerchantID/MerchantTradeNo and a conforming AllPayLogisticsID:
//     SUCCEEDED ecpay.created;
//   - HTTP 403: UNKNOWN ecpay.rate_limited; everything else (transport error, timeout, 3xx/4xx/5xx,
//     oversize, bad MAC, malformed): UNKNOWN ecpay.uncertain.
//
// Every UNKNOWN means the create may exist at ECPay. The caller must never send another Create for
// this trade number (a reused MerchantTradeNo has undocumented behaviour, and a new one would buy a
// second label; I06): it may only call Query with the same MerchantTradeNo.
func (c *Client) Create(ctx context.Context, cr Credentials, p Payload, r CreateRequest) Result {
	form, ok := createForm(cr, p, r)
	if !ok {
		return Result{Outcome: "FAILED_FINAL", Code: CodeInvalidRequest}
	}
	resp, cancel, err := c.post(ctx, callTimeout, pathCreate, form)
	if err != nil {
		return unknown(CodeUncertain) // may have landed: query-only from here
	}
	defer cancel()
	defer resp.Body.Close()
	if code, ok := classifyStatus(resp.StatusCode); !ok {
		return unknown(code)
	}
	body, err := readBody(resp.Body, maxResponseBody)
	if err != nil {
		return unknown(CodeUncertain)
	}
	text := string(body)
	if strings.HasPrefix(text, "0|") {
		return Result{Outcome: "FAILED_FINAL", Code: CodeRejected}
	}
	rest, isOK := strings.CutPrefix(text, "1|")
	if !isOK {
		return unknown(CodeUncertain)
	}
	v, err := parseKV(rest)
	if err != nil || !VerifyMac(v, cr.HashKey, cr.HashIV) ||
		v.Get("MerchantID") != cr.MerchantID || v.Get("MerchantTradeNo") != r.MerchantTradeNo ||
		!logisticsRE.MatchString(v.Get("AllPayLogisticsID")) {
		return unknown(CodeUncertain) // an unproven "success" must not be trusted or repeated
	}
	return foundResult(v, CodeCreated, v.Get("RtnCode"))
}

func foundResult(v url.Values, code, status string) Result {
	return Result{
		Outcome: "SUCCEEDED", Code: code, LogisticsID: v.Get("AllPayLogisticsID"),
		PaymentNo: clean(v.Get("CVSPaymentNo"), 256), ValidationNo: clean(v.Get("CVSValidationNo"), 256),
		ShipmentNo: clean(v.Get("ShipmentNo"), 256), StatusCode: clean(status, 32),
	}
}

// Query looks a trade up by its frozen MerchantTradeNo (Query/V5; TimeStamp = now, valid 3 min, F10).
// It is read-only and idempotent. A MAC-verified response naming AllPayLogisticsID for that trade is
// SUCCEEDED ecpay.query_found (StatusCode = LogisticsStatus); anything else is UNKNOWN, including a
// "trade not found" answer: absence at ECPay does not prove the create never landed, so it stays
// UNKNOWN until the caller's budget ends (§7.4) rather than authorising a second create.
func (c *Client) Query(ctx context.Context, cr Credentials, merchantTradeNo string) Result {
	if !merchantIDRE.MatchString(cr.MerchantID) || cr.HashKey == "" || cr.HashIV == "" || !tradeNoRE.MatchString(merchantTradeNo) {
		return unknown(CodeUncertain)
	}
	form := url.Values{
		"MerchantID": {cr.MerchantID}, "MerchantTradeNo": {merchantTradeNo},
		"TimeStamp": {strconv.FormatInt(c.now().Unix(), 10)},
	}
	form.Set(macField, CheckMac(form, cr.HashKey, cr.HashIV))
	resp, cancel, err := c.post(ctx, callTimeout, pathQuery, form)
	if err != nil {
		return unknown(CodeUncertain)
	}
	defer cancel()
	defer resp.Body.Close()
	if code, ok := classifyStatus(resp.StatusCode); !ok {
		return unknown(code)
	}
	body, err := readBody(resp.Body, maxResponseBody)
	if err != nil {
		return unknown(CodeUncertain)
	}
	v, err := parseKV(string(body))
	if err != nil || !VerifyMac(v, cr.HashKey, cr.HashIV) || v.Get("MerchantID") != cr.MerchantID ||
		v.Get("MerchantTradeNo") != merchantTradeNo || !logisticsRE.MatchString(v.Get("AllPayLogisticsID")) {
		return unknown(CodeUncertain)
	}
	return foundResult(v, CodeQueryFound, v.Get("LogisticsStatus"))
}
