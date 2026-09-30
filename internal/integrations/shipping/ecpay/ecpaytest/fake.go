// Package ecpaytest is an in-process fake of the ECPay logistics HTTP API (gate unit cvs-tests, TCV01/TCV05/TCV06/TCV16).
//
// It owns: a http.RoundTripper that answers for logistics-stage.ecpay.com.tw and logistics.ecpay.com.tw (nothing leaves the
// process), per-merchant HashKey/HashIV, the map / GetStoreList / Express/Create / Query V5 / print endpoints, settable failure
// modes, request counters, and helpers that produce what the ECPay servers would POST back (map return, status notification).
// It never imports the adapter under test: the MAC and the wire shapes are written from the ECPay documents.
//
// Wire constants (docs retrieved 2026-09-30 UTC, https://developers.ecpay.com.tw/):
//   - CheckMacValue 7424: sort A-Z (first letter, then second ...), HashKey=..&..&HashIV=.., .NET urlencode, lower, MD5, upper.
//   - map 8795, GetStoreList 47496 (JSON {RtnCode,RtnMsg,StoreList:[{CvsType,StoreInfo:[{StoreId,StoreName,StoreAddr,StorePhone}]}]}),
//     Create 8809 (success "1|k=v&..&CheckMacValue=..", failure "0|msg"), Query V5 7418 (TimeStamp valid 3 min), status 7420.
//
// Non-goals: not a conformance oracle for ECPay (the not-found answer of Query and the stage status codes are ASSUMPTIONS,
// F19 - real values need TCV07), no rate limiting, no status simulation on its own (tests post signed status via SignStatus).
package ecpaytest

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Merchant is one ECPay account known to the fake.
type Merchant struct{ ID, Key, IV string }

// Store is one directory row.
type Store struct{ ID, Name, Addr, Phone string }

// CreateMode selects how Express/Create answers.
type CreateMode int

const (
	CreateOK             CreateMode = iota // "1|..." MAC-valid
	CreateReject                           // "0|<message>"
	Create403                              // HTTP 403 (rate limited), nothing recorded
	CreateTimeoutLost                      // trade recorded, response never arrives (blocks until the request context ends)
	CreateTimeoutNothing                   // nothing recorded, response never arrives
	CreateBadMAC                           // "1|..." with a wrong CheckMacValue (trade recorded)
	CreateMalformed                        // 200 with a body that is neither "1|k=v" nor "0|msg" (trade recorded)
	CreateServerError                      // HTTP 500 (nothing recorded)
)

// Trade is what the fake remembers for one MerchantTradeNo.
type Trade struct {
	MerchantID, TradeNo, LogisticsID, SubType   string
	PaymentNo, ValidationNo, LogisticsStatus    string
	IsCollection                                string
	CollectionAmount, GoodsAmount               string
	ReceiverStoreID, ReceiverName, ReceiverCell string
	ServerReplyURL                              string
	Env                                         string // "SANDBOX" | "LIVE" (by host)
	Creates                                     int
}

// Call is one request the fake received.
type Call struct{ Env, Path, MerchantID string }

// Fake is the in-process ECPay.
type Fake struct {
	mu                                   sync.Mutex
	merchants                            map[string]Merchant
	lists                                map[string][]Store // CvsType -> rows
	mapStore                             map[string]Store   // LogisticsSubType -> fixed stage store
	trades                               map[string]*Trade  // MerchantTradeNo -> trade (per fake; trade numbers are unique per merchant in tests)
	calls                                []Call
	mode                                 CreateMode
	msg                                  string
	paymentNo, validationNo, logisticsID string
	seq                                  int
	byteOrder                            bool
	queryFail                            bool
	now                                  func() time.Time
}

// New returns an empty fake (no merchants; add with AddMerchant).
func New() *Fake {
	return &Fake{
		merchants: map[string]Merchant{}, lists: map[string][]Store{}, trades: map[string]*Trade{}, now: time.Now,
		mapStore: map[string]Store{
			"UNIMARTC2C": {ID: "131386", Name: "Stage 7-ELEVEN", Addr: "Stage address 7"},
			"FAMIC2C":    {ID: "006598", Name: "Stage FamilyMart", Addr: "Stage address F"},
			"HILIFEC2C":  {ID: "2001", Name: "Stage Hi-Life", Addr: "Stage address H"},
		},
	}
}

// AddMerchant registers an account whose MAC keys the fake verifies and signs with.
func (f *Fake) AddMerchant(m Merchant) { f.mu.Lock(); f.merchants[m.ID] = m; f.mu.Unlock() }

// SetStoreList replaces the GetStoreList rows of one CvsType (FAMI, UNIMART, HILIFE, UNIMARTFREEZE).
func (f *Fake) SetStoreList(cvsType string, rows []Store) {
	f.mu.Lock()
	f.lists[cvsType] = append([]Store(nil), rows...)
	f.mu.Unlock()
}

// BigStoreList makes n synthetic rows whose JSON is at least minBytes long in total (F18: the live list is >= 2 MiB).
func BigStoreList(n int) []Store {
	rows := make([]Store, n)
	for i := range rows {
		rows[i] = Store{ID: fmt.Sprintf("%06d", i+1), Name: fmt.Sprintf("Synthetic store %d", i+1),
			Addr: "Synthetic address " + strings.Repeat("x", 60) + strconv.Itoa(i), Phone: "0200000000"}
	}
	return rows
}

// SetMapStore sets the fixed store the stage map returns for a subtype.
func (f *Fake) SetMapStore(subType string, s Store) {
	f.mu.Lock()
	f.mapStore[subType] = s
	f.mu.Unlock()
}

// SetCreateMode sets how every following Create answers (msg is used by CreateReject).
func (f *Fake) SetCreateMode(m CreateMode, msg string) {
	f.mu.Lock()
	f.mode, f.msg = m, msg
	f.mu.Unlock()
}

// SetCodes overrides the codes a successful Create returns ("" keeps the generated ones).
func (f *Fake) SetCodes(logisticsID, paymentNo, validationNo string) {
	f.mu.Lock()
	f.logisticsID, f.paymentNo, f.validationNo = logisticsID, paymentNo, validationNo
	f.mu.Unlock()
}

// SetLogisticsStatus sets the Query V5 LogisticsStatus of a recorded trade.
func (f *Fake) SetLogisticsStatus(tradeNo, code string) {
	f.mu.Lock()
	if t := f.trades[tradeNo]; t != nil {
		t.LogisticsStatus = code
	}
	f.mu.Unlock()
}

// Trade returns a copy of the recorded trade.
func (f *Fake) Trade(tradeNo string) (Trade, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.trades[tradeNo]
	if t == nil {
		return Trade{}, false
	}
	return *t, true
}

// CreateCalls is the number of Create requests received for tradeNo (a duplicate Create for one trade number is a bug).
func (f *Fake) CreateCalls(tradeNo string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t := f.trades[tradeNo]; t != nil {
		return t.Creates
	}
	return 0
}

// TotalCreates is the number of Create requests received (recorded or not).
func (f *Fake) TotalCreates() int { return f.countPath("/Express/Create") }

// DistinctTradeNos is the number of distinct MerchantTradeNo values a Create ever carried.
func (f *Fake) DistinctTradeNos() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.trades) }

// Calls returns every request received, in order.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// CountCalls counts requests whose path contains pathPart.
func (f *Fake) CountCalls(pathPart string) int { return f.countPath(pathPart) }

func (f *Fake) countPath(part string) (n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.Contains(c.Path, part) {
			n++
		}
	}
	return
}

// Transport returns the RoundTripper to hand to ecpay.NewClient.
func (f *Fake) Transport() http.RoundTripper { return roundTripper{f} }

type roundTripper struct{ f *Fake }

func (rt roundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return rt.f.serve(req) }

func respond(req *http.Request, status int, ctype, body string) *http.Response {
	return &http.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header:  http.Header{"Content-Type": {ctype}},
		Body:    io.NopCloser(strings.NewReader(body)),
		Request: req, ContentLength: int64(len(body))}
}

func (f *Fake) serve(req *http.Request) (*http.Response, error) {
	env := ""
	switch req.URL.Host {
	case "logistics-stage.ecpay.com.tw":
		env = "SANDBOX"
	case "logistics.ecpay.com.tw":
		env = "LIVE"
	default:
		return nil, fmt.Errorf("ecpaytest: unexpected host") // a dial to anything else is a failure, never a pass-through
	}
	if req.Method != http.MethodPost {
		return respond(req, 405, "text/plain", "method"), nil
	}
	raw, _ := io.ReadAll(req.Body)
	_ = req.Body.Close()
	form, err := url.ParseQuery(string(raw))
	if err != nil {
		return respond(req, 400, "text/plain", "bad form"), nil
	}
	path := req.URL.Path
	f.mu.Lock()
	f.calls = append(f.calls, Call{Env: env, Path: path, MerchantID: form.Get("MerchantID")})
	f.mu.Unlock()
	switch {
	case path == "/Express/map":
		return f.serveMap(req, form), nil
	case path == "/Helper/GetStoreList":
		return f.serveList(req, form), nil
	case path == "/Express/Create":
		return f.serveCreate(req, form, env)
	case path == "/Helper/QueryLogisticsTradeInfo/V5":
		return f.serveQuery(req, form), nil
	case strings.HasPrefix(strings.ToLower(path), "/express/print") || strings.EqualFold(path, "/helper/printtradedocument"):
		return f.servePrint(req, form), nil
	}
	return respond(req, 404, "text/plain", "not found"), nil
}

func (f *Fake) merchant(id string) (Merchant, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.merchants[id]
	return m, ok
}

// verified reports whether form carries a valid CheckMacValue of a known merchant.
func (f *Fake) verified(form url.Values) (Merchant, bool) {
	m, ok := f.merchant(form.Get("MerchantID"))
	if !ok {
		return m, false
	}
	got := form.Get("CheckMacValue")
	return m, strings.EqualFold(got, Mac(form, m.Key, m.IV)) || strings.EqualFold(got, MacByteOrder(form, m.Key, m.IV))
}

// serveMap: the stage map has no CheckMacValue (F1) and answers HTML that auto-posts the fixed store (F3).
func (f *Fake) serveMap(req *http.Request, form url.Values) *http.Response {
	if _, ok := f.merchant(form.Get("MerchantID")); !ok || form.Get("LogisticsType") != "CVS" {
		return respond(req, 200, "text/html", "<html>找不到加密金鑰</html>")
	}
	ret := f.MapReturn(form)
	var b strings.Builder
	b.WriteString(`<html><body><form method="post" action="` + form.Get("ServerReplyURL") + `">`)
	for k, v := range ret {
		b.WriteString(`<input type="hidden" name="` + k + `" value="` + v[0] + `">`)
	}
	b.WriteString(`</form><script>document.forms[0].submit()</script></body></html>`)
	return respond(req, 200, "text/html; charset=utf-8", b.String())
}

// MapReturn builds the fields the stage map POSTs to the ServerReplyURL for a map request form (F3; no CheckMacValue).
func (f *Fake) MapReturn(form url.Values) url.Values {
	f.mu.Lock()
	s, ok := f.mapStore[form.Get("LogisticsSubType")]
	f.mu.Unlock()
	if !ok {
		s = Store{ID: "000001", Name: "Stage store", Addr: "Stage address"}
	}
	v := url.Values{
		"MerchantID": {form.Get("MerchantID")}, "MerchantTradeNo": {form.Get("MerchantTradeNo")},
		"LogisticsSubType": {form.Get("LogisticsSubType")}, "CVSStoreID": {s.ID}, "CVSStoreName": {s.Name},
		"CVSAddress": {s.Addr}, "CVSTelephone": {""}, "CVSOutSide": {"0"}, "ExtraData": {form.Get("ExtraData")},
	}
	return v
}

func (f *Fake) serveList(req *http.Request, form url.Values) *http.Response {
	if _, ok := f.verified(form); !ok {
		return respond(req, 200, "application/json", `{"RtnCode":10200073,"RtnMsg":"CheckMacValue Error","StoreList":[]}`)
	}
	type info struct{ StoreId, StoreName, StoreAddr, StorePhone string }
	type entry struct {
		CvsType   string
		StoreInfo []info
	}
	want := form.Get("CvsType")
	f.mu.Lock()
	var list []entry
	types := make([]string, 0, len(f.lists))
	for k := range f.lists {
		types = append(types, k)
	}
	sort.Strings(types)
	for _, k := range types {
		if want != "All" && want != k {
			continue
		}
		e := entry{CvsType: k}
		for _, s := range f.lists[k] {
			e.StoreInfo = append(e.StoreInfo, info{s.ID, s.Name, s.Addr, s.Phone})
		}
		list = append(list, e)
	}
	f.mu.Unlock()
	body, _ := json.Marshal(map[string]any{"RtnCode": 1, "RtnMsg": "成功", "StoreList": list})
	return respond(req, 200, "application/json; charset=utf-8", string(body))
}

func (f *Fake) serveCreate(req *http.Request, form url.Values, env string) (*http.Response, error) {
	f.mu.Lock()
	mode, msg := f.mode, f.msg
	f.mu.Unlock()
	if mode == Create403 {
		return respond(req, 403, "text/plain", "Forbidden"), nil
	}
	if mode == CreateServerError {
		return respond(req, 500, "text/plain", "error"), nil
	}
	if mode == CreateTimeoutNothing {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
	m, ok := f.verified(form)
	if !ok {
		return respond(req, 200, "text/html", "0|CheckMacValue Error"), nil
	}
	if mode == CreateReject {
		return respond(req, 200, "text/html", "0|"+msg), nil
	}
	tn := form.Get("MerchantTradeNo")
	f.mu.Lock()
	t := f.trades[tn]
	if t == nil {
		f.seq++
		t = &Trade{MerchantID: m.ID, TradeNo: tn, LogisticsID: fmt.Sprintf("%d", 1000000+f.seq), SubType: form.Get("LogisticsSubType"),
			PaymentNo: fmt.Sprintf("%08d", 20000000+f.seq), LogisticsStatus: "300", Env: env}
		if strings.HasPrefix(t.SubType, "UNIMART") {
			t.ValidationNo = fmt.Sprintf("%04d", 1000+f.seq%9000)
		}
		f.trades[tn] = t
	}
	t.Creates++
	t.IsCollection, t.CollectionAmount, t.GoodsAmount = form.Get("IsCollection"), form.Get("CollectionAmount"), form.Get("GoodsAmount")
	t.ReceiverStoreID, t.ReceiverName, t.ReceiverCell, t.ServerReplyURL = form.Get("ReceiverStoreID"), form.Get("ReceiverName"), form.Get("ReceiverCellPhone"), form.Get("ServerReplyURL")
	if f.logisticsID != "" {
		t.LogisticsID = f.logisticsID
	}
	if f.paymentNo != "" {
		t.PaymentNo = f.paymentNo
	}
	if f.validationNo != "" {
		t.ValidationNo = f.validationNo
	}
	snapshot := *t
	f.mu.Unlock()
	switch mode {
	case CreateTimeoutLost:
		<-req.Context().Done()
		return nil, req.Context().Err()
	case CreateMalformed:
		return respond(req, 200, "text/html", "<html>maintenance</html>"), nil
	}
	v := url.Values{
		"MerchantID": {m.ID}, "MerchantTradeNo": {tn}, "RtnCode": {"300"}, "BookingNote": {""}, "RtnMsg": {"訂單處理中(已收到訂單資料)"},
		"AllPayLogisticsID": {snapshot.LogisticsID}, "LogisticsType": {"CVS"}, "LogisticsSubType": {snapshot.SubType},
		"GoodsAmount": {snapshot.GoodsAmount}, "UpdateStatusDate": {f.now().Format("2006/01/02 15:04:05")},
		"ReceiverName": {snapshot.ReceiverName}, "ReceiverPhone": {""}, "ReceiverCellPhone": {snapshot.ReceiverCell}, "ReceiverEmail": {""},
		"ReceiverAddress": {""}, "CVSPaymentNo": {snapshot.PaymentNo}, "CVSValidationNo": {snapshot.ValidationNo},
	}
	mac := f.sign(v, m)
	if mode == CreateBadMAC {
		mac = strings.Repeat("0", 32)
	}
	v.Set("CheckMacValue", mac)
	return respond(req, 200, "text/html", "1|"+encodeBody(v)), nil
}

// encodeBody joins k=v with '&' in a fixed order and without escaping (the documented "1|k=v&k=v" shape; values are ASCII-safe
// in the fake except receiver names, which are percent-encoded so the body stays parseable with url.ParseQuery).
func encodeBody(v url.Values) string {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, k+"="+url.QueryEscape(v.Get(k)))
	}
	return strings.Join(parts, "&")
}

func (f *Fake) serveQuery(req *http.Request, form url.Values) *http.Response {
	f.mu.Lock()
	fail := f.queryFail
	f.mu.Unlock()
	if fail {
		return respond(req, 500, "text/plain", "error")
	}
	m, ok := f.verified(form)
	if !ok {
		return respond(req, 200, "text/html", "0|CheckMacValue Error")
	}
	ts, err := strconv.ParseInt(form.Get("TimeStamp"), 10, 64)
	if err != nil || abs(f.now().Unix()-ts) > 180 {
		return respond(req, 200, "text/html", "0|TimeStamp expired")
	}
	f.mu.Lock()
	var t *Trade
	for _, c := range f.trades {
		if c.MerchantID == m.ID && ((form.Get("MerchantTradeNo") != "" && c.TradeNo == form.Get("MerchantTradeNo")) ||
			(form.Get("AllPayLogisticsID") != "" && c.LogisticsID == form.Get("AllPayLogisticsID"))) {
			cp := *c
			t = &cp
		}
	}
	f.mu.Unlock()
	if t == nil {
		// ASSUMPTION (F19-like, UNVERIFIED): a not-found trade answers "0|..." ; TCV07 must record the real answer.
		return respond(req, 200, "text/html", "0|Trade not found")
	}
	v := url.Values{
		"MerchantID": {m.ID}, "MerchantTradeNo": {t.TradeNo}, "AllPayLogisticsID": {t.LogisticsID}, "LogisticsType": {"CVS"},
		"LogisticsSubType": {t.SubType}, "GoodsAmount": {t.GoodsAmount}, "CollectionAmount": {t.CollectionAmount},
		"LogisticsStatus": {t.LogisticsStatus}, "CVSPaymentNo": {t.PaymentNo}, "CVSValidationNo": {t.ValidationNo}, "ShipmentNo": {""},
	}
	v.Set("CheckMacValue", f.sign(v, m))
	return respond(req, 200, "text/html", encodeBody(v))
}

func abs(a int64) int64 {
	if a < 0 {
		return -a
	}
	return a
}

func (f *Fake) servePrint(req *http.Request, form url.Values) *http.Response {
	if _, ok := f.verified(form); !ok {
		return respond(req, 400, "text/plain", "CheckMacValue Error")
	}
	return respond(req, 200, "text/html; charset=utf-8", "<html><body>label</body></html>")
}

// SignStatus returns fields plus the CheckMacValue of merchant m: what ECPay POSTs to a ServerReplyURL (F7). Fields are signed as given
// (every field except CheckMacValue), so an extra unknown field is covered by the MAC.
func SignStatus(m Merchant, fields url.Values) url.Values {
	out := url.Values{}
	for k, v := range fields {
		out[k] = append([]string(nil), v...)
	}
	out.Del("CheckMacValue")
	out.Set("CheckMacValue", Mac(out, m.Key, m.IV))
	return out
}

// StatusFields builds an F7 status notification for a recorded trade with the given RtnCode; recipient fields carry the sentinel
// so tests can prove they are never stored (TD8).
func StatusFields(t Trade, rtnCode, updateDate, recipientSentinel string) url.Values {
	return url.Values{
		"MerchantID": {t.MerchantID}, "MerchantTradeNo": {t.TradeNo}, "RtnCode": {rtnCode}, "RtnMsg": {"status " + rtnCode},
		"AllPayLogisticsID": {t.LogisticsID}, "LogisticsType": {"CVS"}, "LogisticsSubType": {t.SubType}, "GoodsAmount": {t.GoodsAmount},
		"UpdateStatusDate": {updateDate}, "ReceiverName": {recipientSentinel}, "ReceiverPhone": {"0200000000"},
		"ReceiverCellPhone": {"0900000000"}, "ReceiverEmail": {recipientSentinel + "@example.invalid"}, "ReceiverAddress": {recipientSentinel + " road"},
		"CVSPaymentNo": {t.PaymentNo}, "CVSValidationNo": {t.ValidationNo}, "BookingNote": {""},
	}
}

// Mac is CheckMacValue per ECPay 7424, written independently of the adapter. Keys sort case-insensitively (docs: "first letter A to
// Z, then the second ..."; every official SDK folds case). The contract text says "case-sensitive"; MacByteOrder is that variant.
func Mac(params url.Values, key, iv string) string { return mac(params, key, iv, false) }

// MacByteOrder is Mac with plain byte-order key sorting (the other reading of the docs/contract); it differs only for keys such
// as CVSPaymentNo vs CollectionAmount, which the Query V5 response carries together.
func MacByteOrder(params url.Values, key, iv string) string { return mac(params, key, iv, true) }

// SetQueryFail makes every Query V5 answer HTTP 500 (the reconcile cannot learn anything) until it is switched off.
func (f *Fake) SetQueryFail(on bool) { f.mu.Lock(); f.queryFail = on; f.mu.Unlock() }

// SetByteOrderMAC makes the fake sign its own responses (Create, Query) in byte order instead of folded order.
func (f *Fake) SetByteOrderMAC(on bool) { f.mu.Lock(); f.byteOrder = on; f.mu.Unlock() }

func (f *Fake) sign(v url.Values, m Merchant) string {
	f.mu.Lock()
	bo := f.byteOrder
	f.mu.Unlock()
	return mac(v, m.Key, m.IV, bo)
}

func mac(params url.Values, key, iv string, byteOrder bool) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		if k != "CheckMacValue" {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if !byteOrder {
			a, b = strings.ToLower(a), strings.ToLower(b)
			if a != b {
				return a < b
			}
			return keys[i] < keys[j]
		}
		return a < b
	})
	var b bytes.Buffer
	b.WriteString("HashKey=" + key)
	for _, k := range keys {
		b.WriteString("&" + k + "=" + params.Get(k))
	}
	b.WriteString("&HashIV=" + iv)
	sum := md5.Sum([]byte(strings.ToLower(NetEncode(b.String()))))
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// NetEncode is .NET HttpUtility.UrlEncode as ECPay's table lists it: unreserved = letters, digits and -_.!*() ; space is '+';
// every other UTF-8 byte is %xx (lower case after the later ToLower step).
func NetEncode(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', strings.IndexByte("-_.!*()", c) >= 0:
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			fmt.Fprintf(&b, "%%%02x", c)
		}
	}
	return b.String()
}

// ParseCreateBody parses a "1|k=v&.." body into its fields (ok=false for anything else).
func ParseCreateBody(body string) (url.Values, bool) {
	rest, ok := strings.CutPrefix(body, "1|")
	if !ok {
		return nil, false
	}
	v, err := url.ParseQuery(rest)
	return v, err == nil
}

// Handler serves the fake over real HTTP for out-of-process clients (the kill/restart gate): the client names the ECPay host in the
// X-Ecpay-Host header (ForwardTransport sets it), so the environment (stage or live) is still chosen by the host the adapter dialled.
func (f *Fake) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.URL.Scheme, r2.URL.Host = "https", r.Header.Get("X-Ecpay-Host")
		resp, err := f.serve(r2)
		if err != nil {
			if r.Context().Err() == nil {
				http.Error(w, "fake failure", http.StatusBadGateway)
			}
			return
		}
		defer resp.Body.Close()
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})
}

// ForwardTransport returns a RoundTripper that sends every request to the fake's Handler at baseURL, naming the ECPay host in a header.
func ForwardTransport(baseURL string) http.RoundTripper { return forward{baseURL} }

type forward struct{ base string }

func (t forward) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := url.Parse(t.base)
	if err != nil {
		return nil, err
	}
	r2 := req.Clone(req.Context())
	r2.Header.Set("X-Ecpay-Host", req.URL.Host)
	r2.URL.Scheme, r2.URL.Host = u.Scheme, u.Host
	r2.Host = u.Host
	return http.DefaultTransport.RoundTrip(r2)
}
