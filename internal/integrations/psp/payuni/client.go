// Package payuni owns the narrow PAYUNi UPP v2.0 wire profile: hosted-form signing, notification
// verification, trade query and the TWD amount rule.
//
// It never decides whether money has settled or changes an order (internal/payments applies
// captures), never stores a merchant key (accounts custody does), and never calls any host but the
// configured PAYUNi endpoint.
package payuni

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	ErrInvalid        = errors.New("payuni: invalid input")
	ErrTransport      = errors.New("payuni: transport failure")
	ErrAuthentication = errors.New("payuni: authentication failure")
	ErrMismatch       = errors.New("payuni: trade mismatch")
	ErrProtocol       = errors.New("payuni: invalid protocol data")
	ErrUncertain      = errors.New("payuni: outcome uncertain")
)

const (
	maxWireBytes  = 128 << 10
	maxPlainBytes = 48 << 10
	maxFields     = 256
	maxKeyBytes   = 120
	maxUnix       = 253402300799
)

var (
	merchantPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	tradePattern    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,25}$`)
	providerTrade   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	rowFieldPattern = regexp.MustCompile(`^Result\[0\]\[([A-Za-z0-9_]+)\]$`)
	taiwanTime      = time.FixedZone("Taiwan", 8*60*60)
)

type Config struct {
	Environment string
	MerchantID  string
	HashKey     string
	HashIV      string
	ReturnURL   string
	NotifyURL   string
}

// Config and Client deliberately hide their entire configuration in formatting.
func (Config) String() string               { return "payuni.Config{redacted}" }
func (c Config) GoString() string           { return c.String() }
func (Config) MarshalJSON() ([]byte, error) { return []byte(`"payuni.Config{redacted}"`), nil }

type Client struct {
	config     Config
	httpClient *http.Client
	queryOnly  bool
}

func (Client) String() string               { return "payuni.Client{redacted}" }
func (c Client) GoString() string           { return c.String() }
func (Client) MarshalJSON() ([]byte, error) { return []byte(`"payuni.Client{redacted}"`), nil }

type HostedRequest struct {
	MerTradeNo        string
	AmountTWD         int64
	Timestamp         int64
	Description       string
	Method            string
	Installments      []int
	ExpireDate        string
	PageExpirySeconds int
	Language          string
}

type HostedForm struct {
	Action string
	Fields url.Values
}

type ExpectedTrade struct {
	MerTradeNo   string
	TradeNo      string
	AmountTWD    int64
	Currency     string
	Method       string
	Installments []int
}

// Observation is an authenticated provider report, not a paid or settled flag.
type Observation struct {
	MerTradeNo          string
	TradeNo             string
	AmountTWD           int64
	PaymentType         string
	TradeStatus         string
	Status              string
	AuthType            string
	CardInst            int
	DataSource          string
	CloseStatus         string
	CloseAmountTWD      *int64 `json:"CloseAmountTWD,omitempty"`
	CardRefundType      string `json:"CardRefundType,omitempty"`
	CardRefundStatus    string `json:"CardRefundStatus,omitempty"`
	CardRefundAmountTWD *int64 `json:"CardRefundAmountTWD,omitempty"`
	CardRefundDay       string `json:"CardRefundDay,omitempty"`
	CardRemainAmountTWD *int64 `json:"CardRemainAmountTWD,omitempty"`
}

func New(config Config) (*Client, error) {
	return newClient(config, false, nil)
}

// NewQuery constructs a client that can only authenticate and query a trade.
// Query does not use callback URLs, and this client cannot build hosted forms
// or verify notifications.
func NewQuery(config Config, transport ...http.RoundTripper) (*Client, error) {
	if len(transport) > 1 || (len(transport) == 1 && (transport[0] == nil || config.Environment != "SANDBOX")) {
		return nil, ErrInvalid
	}
	if len(transport) == 1 {
		return newClient(config, true, transport[0])
	}
	return newClient(config, true, nil)
}

func newClient(config Config, queryOnly bool, transport http.RoundTripper) (*Client, error) {
	if (config.Environment != "SANDBOX" && config.Environment != "LIVE") ||
		!merchantPattern.MatchString(config.MerchantID) ||
		!printableASCII(config.HashKey, 32) || strings.TrimSpace(config.HashKey) != config.HashKey ||
		!printableASCII(config.HashIV, 16) || strings.TrimSpace(config.HashIV) != config.HashIV ||
		(queryOnly && (config.ReturnURL != "" || config.NotifyURL != "")) ||
		(!queryOnly && (!validCallback(config.ReturnURL) || !validCallback(config.NotifyURL))) {
		return nil, ErrInvalid
	}
	if transport == nil {
		// A query client is one-shot; do not retain idle sockets per leased job.
		transport = &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
			DisableKeepAlives: queryOnly}
	}
	return &Client{config: config, queryOnly: queryOnly, httpClient: &http.Client{
		Timeout:       10 * time.Second,
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func printableASCII(s string, length int) bool {
	if len(s) != length {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

func validCallback(raw string) bool {
	if len(raw) == 0 || len(raw) > 2048 || !utf8.ValidString(raw) || strings.ContainsAny(raw, "\r\n\t#") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") || net.ParseIP(host) != nil ||
		(!strings.Contains(host, ".") || strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".")) ||
		(u.Port() != "" && u.Port() != "443") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, ch := range label {
			if !(ch >= 'a' && ch <= 'z') && !(ch >= '0' && ch <= '9') && ch != '-' {
				return false
			}
		}
	}
	for _, ch := range host[strings.LastIndexByte(host, '.')+1:] {
		if ch >= 'a' && ch <= 'z' {
			return true
		}
	}
	return false
}

func validTimestamp(timestamp int64) bool { return timestamp > 0 && timestamp <= maxUnix }

func validInstallments(values []int, required bool) bool {
	if !required {
		return len(values) == 0
	}
	if len(values) == 0 || len(values) > 4 {
		return false
	}
	previous := 0
	for _, n := range values {
		if n <= previous || (n != 3 && n != 6 && n != 9 && n != 12) {
			return false
		}
		previous = n
	}
	return true
}

func validMethodAmount(method string, amount int64, installments []int) bool {
	if !validInstallments(installments, method == "payuni_installment") {
		return false
	}
	switch method {
	case "payuni_credit", "payuni_installment", "payuni_linepay":
		return amount >= 1 && amount <= 199999
	case "payuni_atm":
		return amount >= 15 && amount <= 49999
	case "payuni_cvs":
		return amount >= 30 && amount <= 20000
	default:
		return false
	}
}

func validExpected(expected ExpectedTrade) bool {
	return tradePattern.MatchString(expected.MerTradeNo) &&
		(expected.TradeNo == "" || providerTrade.MatchString(expected.TradeNo)) &&
		expected.Currency == "TWD" &&
		validMethodAmount(expected.Method, expected.AmountTWD, expected.Installments)
}

func validDescription(s string) bool {
	if len(s) == 0 || len(s) > 550 || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

func validExpiry(timestamp int64, method, date string) bool {
	if method != "payuni_atm" && method != "payuni_cvs" {
		return date == ""
	}
	if len(date) != len("2006-01-02") {
		return false
	}
	expiry, err := time.ParseInLocation("2006-01-02", date, taiwanTime)
	if err != nil || expiry.Format("2006-01-02") != date {
		return false
	}
	now := time.Unix(timestamp, 0).In(taiwanTime)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, taiwanTime)
	maxDays := 180
	if method == "payuni_cvs" {
		maxDays = 7
	}
	return !expiry.Before(today.AddDate(0, 0, 1)) && !expiry.After(today.AddDate(0, 0, maxDays))
}

func (c *Client) endpoint(path string) string {
	if c.config.Environment == "LIVE" {
		return "https://api.payuni.com.tw" + path
	}
	return "https://sandbox-api.payuni.com.tw" + path
}

func (c *Client) BuildHosted(in HostedRequest) (HostedForm, error) {
	if c == nil || c.queryOnly {
		return HostedForm{}, ErrInvalid
	}
	if !tradePattern.MatchString(in.MerTradeNo) || !validTimestamp(in.Timestamp) ||
		!validMethodAmount(in.Method, in.AmountTWD, in.Installments) || !validDescription(in.Description) ||
		!validExpiry(in.Timestamp, in.Method, in.ExpireDate) ||
		in.PageExpirySeconds < 60 || in.PageExpirySeconds > 600 ||
		(in.Language != "zh-tw" && in.Language != "en") {
		return HostedForm{}, ErrInvalid
	}
	inner := url.Values{
		"MerID":           {c.config.MerchantID},
		"MerTradeNo":      {in.MerTradeNo},
		"TradeAmt":        {strconv.FormatInt(in.AmountTWD, 10)},
		"Timestamp":       {strconv.FormatInt(in.Timestamp, 10)},
		"ReturnURL":       {c.config.ReturnURL},
		"NotifyURL":       {c.config.NotifyURL},
		"ProdDesc":        {in.Description},
		"TradeLExpireSec": {strconv.Itoa(in.PageExpirySeconds)},
		"Lang":            {in.Language},
	}
	switch in.Method {
	case "payuni_credit":
		inner.Set("Credit", "1")
	case "payuni_installment":
		parts := make([]string, len(in.Installments))
		for i, tenor := range in.Installments {
			parts[i] = strconv.Itoa(tenor)
		}
		inner.Set("CreditInst", strings.Join(parts, ","))
	case "payuni_atm":
		inner.Set("ATM", "1")
	case "payuni_cvs":
		inner.Set("CVS", "1")
	case "payuni_linepay":
		inner.Set("LinePay", "1")
	}
	if in.ExpireDate != "" {
		inner.Set("ExpireDate", in.ExpireDate)
	}
	encryptInfo, hashInfo, err := c.seal(inner.Encode())
	if err != nil {
		return HostedForm{}, err
	}
	return HostedForm{Action: c.endpoint("/api/upp"), Fields: url.Values{
		"MerID": {c.config.MerchantID}, "Version": {"2.0"},
		"EncryptInfo": {encryptInfo}, "HashInfo": {hashInfo},
	}}, nil
}

func (c *Client) seal(plain string) (encryptInfo, hashInfo string, err error) {
	if c == nil || len(plain) > maxPlainBytes || !utf8.ValidString(plain) {
		return "", "", ErrInvalid
	}
	block, err := aes.NewCipher([]byte(c.config.HashKey))
	if err != nil {
		return "", "", ErrInvalid
	}
	aead, err := cipher.NewGCMWithNonceSize(block, 16)
	if err != nil {
		return "", "", ErrInvalid
	}
	sealed := aead.Seal(nil, []byte(c.config.HashIV), []byte(plain), nil)
	ciphertext, tag := sealed[:len(sealed)-aead.Overhead()], sealed[len(sealed)-aead.Overhead():]
	wire := base64.StdEncoding.EncodeToString(ciphertext) + ":::" + base64.StdEncoding.EncodeToString(tag)
	encryptInfo = hex.EncodeToString([]byte(wire))
	hash := sha256.Sum256([]byte(c.config.HashKey + encryptInfo + c.config.HashIV))
	return encryptInfo, strings.ToUpper(hex.EncodeToString(hash[:])), nil
}

func (c *Client) open(encryptInfo, hashInfo string) (string, error) {
	if c == nil || len(encryptInfo) == 0 || len(encryptInfo) > maxWireBytes ||
		len(hashInfo) != 64 || hashInfo != strings.ToUpper(hashInfo) {
		return "", ErrProtocol
	}
	providedHash, err := hex.DecodeString(hashInfo)
	if err != nil {
		return "", ErrProtocol
	}
	expectedHash := sha256.Sum256([]byte(c.config.HashKey + encryptInfo + c.config.HashIV))
	if subtle.ConstantTimeCompare(providedHash, expectedHash[:]) != 1 {
		return "", ErrAuthentication
	}
	wire, err := hex.DecodeString(encryptInfo)
	if err != nil || len(wire) > maxWireBytes || !bytes.Equal([]byte(encryptInfo), []byte(hex.EncodeToString(wire))) {
		return "", ErrProtocol
	}
	parts := bytes.Split(wire, []byte(":::"))
	if len(parts) != 2 {
		return "", ErrProtocol
	}
	ciphertext, err := base64.StdEncoding.Strict().DecodeString(string(parts[0]))
	if err != nil || len(ciphertext) > maxPlainBytes {
		return "", ErrProtocol
	}
	tag, err := base64.StdEncoding.Strict().DecodeString(string(parts[1]))
	if err != nil || len(tag) != 16 {
		return "", ErrProtocol
	}
	block, err := aes.NewCipher([]byte(c.config.HashKey))
	if err != nil {
		return "", ErrProtocol
	}
	aead, err := cipher.NewGCMWithNonceSize(block, 16)
	if err != nil {
		return "", ErrProtocol
	}
	plain, err := aead.Open(nil, []byte(c.config.HashIV), append(ciphertext, tag...), nil)
	if err != nil {
		return "", ErrAuthentication
	}
	if len(plain) > maxPlainBytes || !utf8.Valid(plain) {
		return "", ErrProtocol
	}
	return string(plain), nil
}

func parseForm(raw []byte) (map[string]string, error) {
	if len(raw) == 0 || len(raw) > maxWireBytes || !utf8.Valid(raw) {
		return nil, ErrProtocol
	}
	parts := bytes.Split(raw, []byte{'&'})
	if len(parts) > maxFields {
		return nil, ErrProtocol
	}
	out := make(map[string]string, len(parts))
	for _, part := range parts {
		keyRaw, valRaw, found := bytes.Cut(part, []byte{'='})
		if !found || len(keyRaw) == 0 {
			return nil, ErrProtocol
		}
		key, err := url.QueryUnescape(string(keyRaw))
		if err != nil || len(key) == 0 || len(key) > maxKeyBytes || !utf8.ValidString(key) {
			return nil, ErrProtocol
		}
		value, err := url.QueryUnescape(string(valRaw))
		if err != nil || !utf8.ValidString(value) {
			return nil, ErrProtocol
		}
		if _, exists := out[key]; exists {
			return nil, ErrProtocol
		}
		out[key] = value
	}
	return out, nil
}

func parseJSONEnvelope(raw []byte) (map[string]string, error) {
	if len(raw) == 0 || len(raw) > maxWireBytes || !utf8.Valid(raw) {
		return nil, ErrProtocol
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return nil, ErrProtocol
	}
	out := make(map[string]string)
	for decoder.More() {
		if len(out) >= maxFields {
			return nil, ErrProtocol
		}
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, ErrProtocol
		}
		key, ok := keyToken.(string)
		if !ok || len(key) == 0 || len(key) > maxKeyBytes || strings.ContainsRune(key, utf8.RuneError) {
			return nil, ErrProtocol
		}
		if _, exists := out[key]; exists {
			return nil, ErrProtocol
		}
		valueToken, err := decoder.Token()
		if err != nil {
			return nil, ErrProtocol
		}
		value, ok := valueToken.(string)
		if !ok || strings.ContainsRune(value, utf8.RuneError) {
			return nil, ErrProtocol
		}
		out[key] = value
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		return nil, ErrProtocol
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, ErrProtocol
	}
	return out, nil
}

func (c *Client) authenticate(outer map[string]string, query bool) (map[string]string, error) {
	if outer["EncryptInfo"] == "" || outer["HashInfo"] == "" {
		return nil, ErrUncertain
	}
	if outer["MerID"] != c.config.MerchantID || outer["Version"] != "2.0" {
		return nil, ErrMismatch
	}
	plain, err := c.open(outer["EncryptInfo"], outer["HashInfo"])
	if err != nil {
		return nil, err
	}
	inner, err := parseForm([]byte(plain))
	if err != nil {
		return nil, err
	}
	if !query && inner["MerID"] != c.config.MerchantID {
		return nil, ErrMismatch
	}
	return inner, nil
}

func (c *Client) VerifyNotification(body []byte, expected ExpectedTrade) (Observation, error) {
	if c == nil || c.queryOnly || !validExpected(expected) {
		return Observation{}, ErrInvalid
	}
	outer, err := parseForm(body)
	if err != nil {
		return Observation{}, err
	}
	inner, err := c.authenticate(outer, false)
	if err != nil {
		return Observation{}, err
	}
	statusPair := outer["Status"] == inner["Status"]
	if inner["Status"] == "UNAPPROVED" {
		statusPair = outer["Status"] == "Unapproved"
	}
	if !statusPair ||
		(inner["Status"] != "SUCCESS" && inner["Status"] != "UNKNOWN" && inner["Status"] != "UNAPPROVED") {
		return Observation{}, ErrUncertain
	}
	for key := range inner {
		if strings.ContainsAny(key, "[]") {
			return Observation{}, ErrProtocol
		}
	}
	return project(inner, expected, false)
}

func queryRow(inner map[string]string) (map[string]string, error) {
	row := make(map[string]string)
	for key, value := range inner {
		if key == "Result" || strings.HasPrefix(key, "Result[") || strings.ContainsAny(key, "[]") {
			match := rowFieldPattern.FindStringSubmatch(key)
			if match == nil {
				return nil, ErrUncertain
			}
			if _, exists := row[match[1]]; exists {
				return nil, ErrUncertain
			}
			row[match[1]] = value
		}
	}
	if len(row) == 0 {
		return nil, ErrUncertain
	}
	return row, nil
}

func (c *Client) Query(ctx context.Context, expected ExpectedTrade, timestamp int64) (Observation, error) {
	if c == nil || !validExpected(expected) || !validTimestamp(timestamp) || ctx == nil {
		return Observation{}, ErrInvalid
	}
	inner := url.Values{"MerID": {c.config.MerchantID}, "Timestamp": {strconv.FormatInt(timestamp, 10)}}
	if expected.TradeNo != "" {
		inner.Set("TradeNo", expected.TradeNo)
	} else {
		inner.Set("MerTradeNo", expected.MerTradeNo)
	}
	encryptInfo, hashInfo, err := c.seal(inner.Encode())
	if err != nil {
		return Observation{}, err
	}
	form := url.Values{"MerID": {c.config.MerchantID}, "Version": {"2.0"},
		"EncryptInfo": {encryptInfo}, "HashInfo": {hashInfo}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/api/trade/query"), strings.NewReader(form.Encode()))
	if err != nil {
		return Observation{}, ErrTransport
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "payuni")
	response, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Observation{}, ctx.Err()
		}
		return Observation{}, ErrTransport
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Observation{}, ErrTransport
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxWireBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return Observation{}, ctx.Err()
		}
		return Observation{}, ErrTransport
	}
	outer, err := parseJSONEnvelope(body)
	if err != nil {
		return Observation{}, err
	}
	innerResponse, err := c.authenticate(outer, true)
	if err != nil {
		return Observation{}, err
	}
	if outer["Status"] != "SUCCESS" || innerResponse["Status"] != "SUCCESS" {
		return Observation{}, ErrUncertain
	}
	row, err := queryRow(innerResponse)
	if err != nil {
		return Observation{}, err
	}
	if row["MerID"] != "" && row["MerID"] != c.config.MerchantID {
		return Observation{}, ErrMismatch
	}
	row["Status"] = innerResponse["Status"]
	return project(row, expected, true)
}

func project(fields map[string]string, expected ExpectedTrade, query bool) (Observation, error) {
	amount, err := strconv.ParseInt(fields["TradeAmt"], 10, 64)
	if err != nil || amount != expected.AmountTWD || fields["MerTradeNo"] != expected.MerTradeNo ||
		(expected.TradeNo != "" && fields["TradeNo"] != expected.TradeNo) ||
		(fields["TradeNo"] != "" && !providerTrade.MatchString(fields["TradeNo"])) || fields["Gateway"] != "2" {
		return Observation{}, ErrMismatch
	}
	paymentType := map[string]string{"payuni_credit": "1", "payuni_installment": "1", "payuni_atm": "2",
		"payuni_cvs": "3", "payuni_linepay": "9"}[expected.Method]
	if fields["PaymentType"] != paymentType {
		return Observation{}, ErrMismatch
	}
	allowedStatus := map[string]bool{"0": true, "1": true, "2": true, "3": true, "8": true}
	if query {
		allowedStatus["4"], allowedStatus["9"] = true, true
	}
	if !allowedStatus[fields["TradeStatus"]] {
		return Observation{}, ErrUncertain
	}
	out := Observation{MerTradeNo: fields["MerTradeNo"], TradeNo: fields["TradeNo"], AmountTWD: amount,
		PaymentType: fields["PaymentType"], TradeStatus: fields["TradeStatus"], Status: fields["Status"]}
	if expected.Method == "payuni_credit" || expected.Method == "payuni_installment" {
		want := "1"
		if expected.Method == "payuni_installment" {
			want = "2"
		}
		if fields["AuthType"] != want {
			return Observation{}, ErrMismatch
		}
		out.AuthType = want
		if expected.Method == "payuni_installment" {
			out.CardInst, err = strconv.Atoi(fields["CardInst"])
			if err != nil || !containsInt(expected.Installments, out.CardInst) {
				return Observation{}, ErrMismatch
			}
		} else if fields["CardInst"] != "" && fields["CardInst"] != "0" {
			return Observation{}, ErrMismatch
		}
	} else if fields["AuthType"] != "" || fields["CardInst"] != "" {
		return Observation{}, ErrMismatch
	}
	if query {
		if fields["DataSource"] != "A" && fields["DataSource"] != "B" {
			return Observation{}, ErrUncertain
		}
		out.DataSource = fields["DataSource"]
	}
	if fields["CloseStatus"] != "" {
		switch fields["CloseStatus"] {
		case "1", "2", "3", "7", "9":
			out.CloseStatus = fields["CloseStatus"]
		default:
			return Observation{}, ErrUncertain
		}
	}
	if query && paymentType == "1" {
		out.CloseAmountTWD, err = optionalQueryAmount(fields["CloseAmt"])
		if err != nil {
			return Observation{}, err
		}
		if typeCode := fields["RefundType"]; typeCode != "" && typeCode != "2" && typeCode != "3" {
			return Observation{}, ErrUncertain
		}
		if status := fields["RefundStatus"]; status != "" && status != "1" && status != "2" && status != "3" && status != "8" {
			return Observation{}, ErrUncertain
		}
		if day := fields["RefundDay"]; day != "" && !validQueryLocalDay(day) {
			return Observation{}, ErrUncertain
		}
		out.CardRefundType = fields["RefundType"]
		out.CardRefundStatus = fields["RefundStatus"]
		out.CardRefundDay = fields["RefundDay"]
		out.CardRefundAmountTWD, err = optionalQueryAmount(fields["RefundAmount"])
		if err != nil {
			return Observation{}, err
		}
		out.CardRemainAmountTWD, err = optionalQueryAmount(fields["RemainAmount"])
		if err != nil {
			return Observation{}, err
		}
	}
	return out, nil
}

func optionalQueryAmount(raw string) (*int64, error) {
	if raw == "" {
		return nil, nil
	}
	if len(raw) > 6 || (raw != "0" && (raw[0] < '1' || raw[0] > '9')) {
		return nil, ErrUncertain
	}
	for i := 1; i < len(raw); i++ {
		if raw[i] < '0' || raw[i] > '9' {
			return nil, ErrUncertain
		}
	}
	amount, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || amount > 199999 {
		return nil, ErrUncertain
	}
	return &amount, nil
}

func validQueryLocalDay(day string) bool {
	if len(day) != 19 {
		return false
	}
	parsed, err := time.Parse("2006-01-02 15:04:05", day)
	return err == nil && parsed.Year() > 0 && parsed.Format("2006-01-02 15:04:05") == day
}

func containsInt(values []int, needle int) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}
