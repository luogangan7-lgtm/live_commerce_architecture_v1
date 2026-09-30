// route_test.go: unit (MOCK) tests of the ecpay.cvs_create route hooks against a fake row source and
// an in-process ECPay transport: no PG, no network. The REAL_PG legs (lease fence, Finish under real
// roles, kill/restart) are cvs-tests' TCV05 and need cvs-core's SQL.

package ecpayroute

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"livecommerce/internal/integrations/core"
	"livecommerce/internal/integrations/shipping/ecpay"
)

const (
	opID     = "0f8fad5b-d9cb-469f-a165-70867728950e"
	tenantID = "11111111-1111-4111-8111-111111111111"
	storeID  = "22222222-2222-4222-8222-222222222222"
	connID   = "33333333-3333-4333-8333-333333333333"
	orderID  = "44444444-4444-4444-8444-444444444444"
	endpoint = "55555555-5555-4555-8555-555555555555"
	hooks    = "https://hooks.example.test"
)

// Synthetic credentials assembled from parts (PROCESS §6); not any ECPay account's keys.
var (
	tKey = "fake" + "key0123456ab"
	tIV  = "fake" + "iv0123456789"
)

// ---- fakes ---------------------------------------------------------------------------------------

type fakeRow struct {
	vals []any
	err  error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.vals) {
		return errors.New("fakeRow: column count mismatch")
	}
	for i, d := range dest {
		switch p := d.(type) {
		case *string:
			*p = r.vals[i].(string)
		case *int64:
			*p = r.vals[i].(int64)
		case *int32:
			*p = r.vals[i].(int32)
		case **int32:
			*p, _ = r.vals[i].(*int32)
		case *[]byte:
			*p = r.vals[i].([]byte)
		case *time.Time:
			*p = r.vals[i].(time.Time)
		default:
			return errors.New("fakeRow: unsupported dest")
		}
	}
	return nil
}

type fakeQ struct {
	row  fakeRow
	args []any
	sql  string
}

func (q *fakeQ) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	q.sql, q.args = sql, args
	return q.row
}

type fakeExec struct {
	sql  string
	args []any
	err  error
}

func (e *fakeExec) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	e.sql, e.args = sql, args
	return pgconn.CommandTag{}, e.err
}

type fakeECPay struct {
	mu    sync.Mutex
	paths []string
	forms []url.Values
	h     func(path string, form url.Values) (int, string)
}

func (f *fakeECPay) RoundTrip(r *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(b))
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.Path)
	f.forms = append(f.forms, form)
	f.mu.Unlock()
	st, body := f.h(r.URL.Path, form)
	return &http.Response{StatusCode: st, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

func (f *fakeECPay) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.paths {
		if p == path {
			n++
		}
	}
	return n
}

// ---- fixtures ------------------------------------------------------------------------------------

func testRing(t *testing.T) *ecpay.Keyring {
	t.Helper()
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	doc, _ := json.Marshal(map[string]any{"active": "k1", "keys": []map[string]string{{"id": "k1", "key_base64": base64.StdEncoding.EncodeToString(b)}}})
	k, err := ecpay.LoadKeyring(func(string) string { return string(doc) })
	if err != nil {
		t.Fatal(err)
	}
	return k
}

type fixture struct {
	keys   *ecpay.Keyring
	client *ecpay.Client
	ec     *fakeECPay
	a      *adapter
	scope  ecpay.Scope
	keyID  string
	nonce  []byte
	ct     []byte
}

func newFixture(t *testing.T, env ecpay.Environment, cfg ecpay.Config, h func(string, url.Values) (int, string)) *fixture {
	t.Helper()
	f := &fixture{keys: testRing(t), ec: &fakeECPay{h: h}}
	if h == nil {
		f.ec.h = func(string, url.Values) (int, string) { return 500, "" }
	}
	c, err := ecpay.NewClient(env, f.ec)
	if err != nil {
		t.Fatal(err)
	}
	f.client = c
	f.scope = ecpay.Scope{TenantID: tenantID, StoreID: storeID, ConnectionID: connID, MerchantID: "2000933", Environment: env, Version: 2}
	f.keyID, f.nonce, f.ct, err = f.keys.Seal(f.scope, ecpay.Payload{HashKey: tKey, HashIV: tIV, SenderName: "陳大文", SenderCellPhone: "0987654321"})
	if err != nil {
		t.Fatal(err)
	}
	f.a = &adapter{keys: f.keys, client: c, cfg: cfg}
	return f
}

func (f *fixture) row(mut func(v []any)) fakeRow {
	coll := int32(350)
	v := []any{tenantID, storeID, connID, string(f.scope.Environment), int64(2), "2000933", f.keyID, f.nonce, f.ct, endpoint,
		"UNIMARTC2C", "131386", ecpay.MerchantTradeNo(opID),
		time.Date(2026, 9, 30, 4, 0, 5, 0, time.UTC), int32(350), &coll, "王小明", "+886912345678"}
	if mut != nil {
		mut(v)
	}
	return fakeRow{vals: v}
}

func claim(mode string) core.SecretClaim {
	return core.SecretClaim{OperationID: opID, Generation: 7, LeaseToken: []byte("lease"), Mode: mode}
}

func enabled() ecpay.Config { return ecpay.Config{Enabled: true, HooksOrigin: hooks} }

// ---- Check ---------------------------------------------------------------------------------------

func TestRouteCheck(t *testing.T) {
	f := newFixture(t, ecpay.EnvSandbox, enabled(), nil)
	ok := core.DispatchRequest{Request: json.RawMessage(`{"order_id":"` + orderID + `","attempt":1}`)}
	if err := f.a.check(context.Background(), ok); err != nil {
		t.Fatalf("valid request: %v", err)
	}
	for name, body := range map[string]string{
		"extra field": `{"order_id":"` + orderID + `","attempt":1,"name":"x"}`,
		"no order":    `{"attempt":1}`,
		"bad uuid":    `{"order_id":"nope","attempt":1}`,
		"upper uuid":  `{"order_id":"AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA","attempt":1}`,
		"attempt 0":   `{"order_id":"` + orderID + `","attempt":0}`,
		"trailing":    `{"order_id":"` + orderID + `","attempt":1}{}`,
		"not json":    `oops`,
		"empty":       ``,
	} {
		err := f.a.check(context.Background(), core.DispatchRequest{Request: json.RawMessage(body)})
		if !errors.Is(err, core.ErrPolicyDenied) {
			t.Errorf("%s: err=%v", name, err)
		}
	}
	off := newFixture(t, ecpay.EnvSandbox, ecpay.Config{HooksOrigin: hooks}, nil)
	if err := off.a.check(context.Background(), ok); !errors.Is(err, core.ErrPolicyDenied) {
		t.Errorf("CVS_ECPAY_ENABLED=0 must deny: %v", err)
	}
}

// ---- LoadSecret ----------------------------------------------------------------------------------

func TestRouteLoadSecretHappyPath(t *testing.T) {
	f := newFixture(t, ecpay.EnvSandbox, enabled(), nil)
	q := &fakeQ{row: f.row(nil)}
	s, err := f.a.load(context.Background(), q, claim("dispatch"))
	if err != nil {
		t.Fatal(err)
	}
	if q.args[0] != opID || q.args[1] != int64(7) || string(q.args[2].([]byte)) != "lease" || q.args[3] != "dispatch" {
		t.Fatalf("loader args %v", q.args)
	}
	if !strings.Contains(q.sql, "integration.load_cvs_create") {
		t.Fatalf("sql %s", q.sql)
	}
	d, err := decodeSecret(s)
	if err != nil {
		t.Fatal(err)
	}
	want := secretDoc{
		MerchantID: "2000933", HashKey: tKey, HashIV: tIV, SenderName: "陳大文", SenderCellPhone: "0987654321",
		TradeNo: ecpay.MerchantTradeNo(opID), TradeDate: "2026/09/30 12:00:05", SubType: "UNIMARTC2C", StoreCode: "131386",
		RecipientName: "王小明", RecipientPhone: "+886912345678",
		ServerReplyURL: hooks + "/v1/cvs/ecpay/status/" + endpoint, Goods: 350, Collection: 350,
	}
	if d != want {
		t.Fatalf("got %+v\nwant %+v", d, want)
	}
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		if strings.Contains(strings.TrimSpace(fmtAny(format, s)), tKey) {
			t.Fatalf("Secret leaked through %s", format)
		}
	}
	// A card order carries a NULL collection_amount.
	q = &fakeQ{row: f.row(func(v []any) { v[15] = (*int32)(nil) })}
	s, err = f.a.load(context.Background(), q, claim("dispatch"))
	if d, _ = decodeSecret(s); err != nil || d.Collection != 0 {
		t.Fatalf("card order: %+v %v", d, err)
	}
}

func TestRouteLoadSecretErrorMapping(t *testing.T) {
	f := newFixture(t, ecpay.EnvSandbox, enabled(), nil)
	mk := func(code string) error { return &pgconn.PgError{Code: code, Message: "secret detail " + tKey} }
	if _, err := f.a.load(context.Background(), &fakeQ{row: fakeRow{err: mk("PT409")}}, claim("dispatch")); !errors.Is(err, core.ErrPolicyDenied) {
		t.Errorf("PT409 must be a policy denial: %v", err)
	}
	for _, code := range []string{"40001", "22023", "P0002", "XX000", "42501"} {
		_, err := f.a.load(context.Background(), &fakeQ{row: fakeRow{err: mk(code)}}, claim("dispatch"))
		if err == nil || errors.Is(err, core.ErrPolicyDenied) || strings.Contains(err.Error(), tKey) {
			t.Errorf("%s: want a plain error without detail, got %v", code, err)
		}
	}
	for _, e := range []error{pgx.ErrNoRows, errors.New("connection reset"), context.DeadlineExceeded} {
		if _, err := f.a.load(context.Background(), &fakeQ{row: fakeRow{err: e}}, claim("dispatch")); err == nil || errors.Is(err, core.ErrPolicyDenied) {
			t.Errorf("%v must be a plain error, got %v", e, err)
		}
	}
}

func TestRouteLoadSecretGates(t *testing.T) {
	ctx := context.Background()
	denied := func(name string, f *fixture, mut func([]any), mode string) {
		t.Helper()
		if _, err := f.a.load(ctx, &fakeQ{row: f.row(mut)}, claim(mode)); !errors.Is(err, core.ErrPolicyDenied) {
			t.Errorf("%s: want policy denial, got %v", name, err)
		}
	}
	allowed := func(name string, f *fixture, mut func([]any), mode string) {
		t.Helper()
		if _, err := f.a.load(ctx, &fakeQ{row: f.row(mut)}, claim(mode)); err != nil {
			t.Errorf("%s: want success, got %v", name, err)
		}
	}
	sb := newFixture(t, ecpay.EnvSandbox, enabled(), nil)
	// LIVE credential vs stage client (and the reverse) is refused in both modes: keys never cross
	// hosts. The ciphertext is sealed under the LIVE scope so decryption would succeed: only the
	// explicit environment pin stops it.
	liveScope := sb.scope
	liveScope.Environment = ecpay.EnvLive
	lk, ln, lc, err := sb.keys.Seal(liveScope, ecpay.Payload{HashKey: tKey, HashIV: tIV, SenderName: "陳大文", SenderCellPhone: "0987654321"})
	if err != nil {
		t.Fatal(err)
	}
	asLive := func(v []any) { v[3], v[6], v[7], v[8] = "LIVE", lk, ln, lc }
	denied("row LIVE, client SANDBOX", sb, asLive, "dispatch")
	denied("row LIVE, client SANDBOX reconcile", sb, asLive, "reconcile")
	denied("row junk env", sb, func(v []any) { v[3] = "PROD" }, "dispatch")
	// Trade number must be the E2 derivation of the operation.
	denied("trade no mismatch", sb, func(v []any) { v[12] = "LCAAAAAAAAAAAAAAAAAA" }, "dispatch")
	// Recipient rule is checked before dispatch only; a query never needs it.
	denied("bad recipient phone", sb, func(v []any) { v[17] = "0212345678" }, "dispatch")
	denied("bad recipient name", sb, func(v []any) { v[16] = "王1" }, "dispatch")
	allowed("bad recipient, reconcile", sb, func(v []any) { v[17] = "0212345678" }, "reconcile")
	// Decrypt failures are denials: wrong version, wrong merchant, wrong key id.
	denied("wrong version", sb, func(v []any) { v[4] = int64(3) }, "dispatch")
	denied("wrong merchant", sb, func(v []any) { v[5] = "2000132" }, "dispatch")
	denied("unknown key id", sb, func(v []any) { v[6] = "k9" }, "dispatch")
	denied("tampered ciphertext", sb, func(v []any) { ct := append([]byte(nil), sb.ct...); ct[1] ^= 1; v[8] = ct }, "dispatch")

	// LIVE needs CVS_ECPAY_LIVE_CREATE=1 to dispatch (§0.3 P4); a reconcile query is read-only.
	live := newFixture(t, ecpay.EnvLive, enabled(), nil)
	denied("LIVE without flag", live, nil, "dispatch")
	allowed("LIVE without flag, reconcile", live, nil, "reconcile")
	liveOpen := newFixture(t, ecpay.EnvLive, ecpay.Config{Enabled: true, LiveCreate: true, HooksOrigin: hooks}, nil)
	allowed("LIVE with flag", liveOpen, nil, "dispatch")
	allowed("SANDBOX", sb, nil, "dispatch")
}

// ---- dispatch / reconcile ------------------------------------------------------------------------

func signed(v url.Values) string {
	v.Set("CheckMacValue", ecpay.CheckMac(v, tKey, tIV))
	var parts []string
	for k := range v {
		parts = append(parts, k+"="+v.Get(k))
	}
	return strings.Join(parts, "&")
}

func tradeReply(status string) url.Values {
	return url.Values{
		"MerchantID": {"2000933"}, "MerchantTradeNo": {ecpay.MerchantTradeNo(opID)}, "RtnCode": {"300"}, "RtnMsg": {"ok"},
		"AllPayLogisticsID": {"1234567"}, "LogisticsStatus": {status}, "CVSPaymentNo": {"C1234567"}, "CVSValidationNo": {"9876"},
	}
}

func loadedSecret(t *testing.T, f *fixture, mode string) core.Secret {
	t.Helper()
	s, err := f.a.load(context.Background(), &fakeQ{row: f.row(nil)}, claim(mode))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRouteDispatchCreates(t *testing.T) {
	f := newFixture(t, ecpay.EnvSandbox, enabled(), func(p string, form url.Values) (int, string) {
		if p != "/Express/Create" || !ecpay.VerifyMac(form, tKey, tIV) {
			t.Errorf("bad create %s %v", p, form)
		}
		return 200, "1|" + signed(tradeReply("300"))
	})
	out, err := f.a.dispatch(context.Background(), core.DispatchRequest{Mode: "dispatch"}, loadedSecret(t, f, "dispatch"))
	if err != nil {
		t.Fatal(err)
	}
	want := core.Outcome{State: "SUCCEEDED", Code: "ecpay.created", ProviderReference: "1234567",
		Detail: detail{LogisticsID: "1234567", PaymentNo: "C1234567", ValidationNo: "9876", StatusCode: "300"}}
	if out != want {
		t.Fatalf("got %+v want %+v", out, want)
	}
	form := f.ec.forms[0]
	if form.Get("IsCollection") != "Y" || form.Get("CollectionAmount") != "350" || form.Get("GoodsAmount") != "350" ||
		form.Get("MerchantTradeNo") != ecpay.MerchantTradeNo(opID) || form.Get("MerchantTradeDate") != "2026/09/30 12:00:05" ||
		form.Get("ServerReplyURL") != hooks+"/v1/cvs/ecpay/status/"+endpoint || form.Get("ReceiverStoreID") != "131386" ||
		form.Get("ReceiverCellPhone") != "0912345678" {
		t.Fatalf("frozen fields not sent: %v", form)
	}
	if len(f.ec.paths) != 1 {
		t.Fatalf("requests=%d", len(f.ec.paths))
	}
}

func TestRouteDispatchClassificationNeverRepeats(t *testing.T) {
	for name, tc := range map[string]struct {
		status  int
		body    string
		state   string
		code    string
		wantRef string
	}{
		"rejected":    {200, "0|balance", "FAILED_FINAL", "ecpay.rejected", ""},
		"rate limit":  {403, "", "UNKNOWN", "ecpay.rate_limited", ""},
		"server 500":  {500, "", "UNKNOWN", "ecpay.uncertain", ""},
		"bad mac":     {200, "1|MerchantID=2000933&CheckMacValue=00", "UNKNOWN", "ecpay.uncertain", ""},
		"garbage 200": {200, "hello", "UNKNOWN", "ecpay.uncertain", ""},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, ecpay.EnvSandbox, enabled(), func(string, url.Values) (int, string) { return tc.status, tc.body })
			out, err := f.a.dispatch(context.Background(), core.DispatchRequest{Mode: "dispatch"}, loadedSecret(t, f, "dispatch"))
			if err != nil || out.State != tc.state || out.Code != tc.code || out.ProviderReference != tc.wantRef {
				t.Fatalf("got %+v err=%v", out, err)
			}
			if f.ec.count("/Express/Create") != 1 {
				t.Fatalf("exactly one create expected, got %d", f.ec.count("/Express/Create"))
			}
		})
	}
}

func TestRouteDispatchRefusedInReconcileMode(t *testing.T) {
	f := newFixture(t, ecpay.EnvSandbox, enabled(), nil)
	if _, err := f.a.dispatch(context.Background(), core.DispatchRequest{Mode: "reconcile"}, loadedSecret(t, f, "reconcile")); err == nil {
		t.Fatal("Create must never run in reconcile mode (I06)")
	}
	if len(f.ec.paths) != 0 {
		t.Fatal("no request may be sent")
	}
	if _, err := f.a.dispatch(context.Background(), core.DispatchRequest{Mode: "dispatch"}, core.NewSecret([]byte("not json"))); err == nil {
		t.Fatal("malformed loaded secret must fail before any request")
	}
}

func TestRouteReconcileQueriesOnly(t *testing.T) {
	f := newFixture(t, ecpay.EnvSandbox, enabled(), func(p string, form url.Values) (int, string) {
		if p != "/Helper/QueryLogisticsTradeInfo/V5" || form.Get("MerchantTradeNo") != ecpay.MerchantTradeNo(opID) || !ecpay.VerifyMac(form, tKey, tIV) {
			t.Errorf("bad query %s %v", p, form)
		}
		return 200, signed(tradeReply("2030"))
	})
	out, err := f.a.reconcile(context.Background(), core.DispatchRequest{Mode: "reconcile"}, loadedSecret(t, f, "reconcile"))
	if err != nil || out.State != "SUCCEEDED" || out.Code != "ecpay.query_found" || out.ProviderReference != "1234567" ||
		out.Detail.(detail).StatusCode != "2030" {
		t.Fatalf("got %+v err=%v", out, err)
	}
	if f.ec.count("/Express/Create") != 0 {
		t.Fatal("reconcile must never create")
	}
	// A trade ECPay cannot prove stays UNKNOWN (never FAILED, never a second create).
	f = newFixture(t, ecpay.EnvSandbox, enabled(), func(string, url.Values) (int, string) { return 200, "0|not found" })
	out, err = f.a.reconcile(context.Background(), core.DispatchRequest{}, loadedSecret(t, f, "reconcile"))
	if err != nil || out.State != "UNKNOWN" || f.ec.count("/Express/Create") != 0 {
		t.Fatalf("got %+v err=%v", out, err)
	}
}

// ---- Finish --------------------------------------------------------------------------------------

func TestRouteFinishArgsAndNulls(t *testing.T) {
	f := newFixture(t, ecpay.EnvSandbox, enabled(), nil)
	ex := &fakeExec{}
	out := core.Outcome{State: "SUCCEEDED", Code: "ecpay.created", ProviderReference: "1234567",
		Detail: detail{LogisticsID: "1234567", PaymentNo: "C1234567", ValidationNo: "9876", ShipmentNo: "", StatusCode: "300"}}
	if err := f.a.finishOn(context.Background(), ex, claim("dispatch"), out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ex.sql, "integration.finish_cvs_create") || len(ex.args) != 10 {
		t.Fatalf("sql=%s args=%d", ex.sql, len(ex.args))
	}
	want := []any{opID, int64(7), []byte("lease"), "SUCCEEDED", "ecpay.created", "1234567", "C1234567", "9876", nil, "300"}
	for i, w := range want {
		if wb, isB := w.([]byte); isB {
			if string(ex.args[i].([]byte)) != string(wb) {
				t.Errorf("arg %d", i)
			}
		} else if ex.args[i] != w {
			t.Errorf("arg %d = %#v want %#v", i, ex.args[i], w)
		}
	}
	// Dispatcher-made outcomes carry no Detail: every code is NULL, never an empty string.
	ex = &fakeExec{}
	if err := f.a.finishOn(context.Background(), ex, claim("dispatch"), core.Outcome{State: "BLOCKED_POLICY", Code: "credential_unavailable"}); err != nil {
		t.Fatal(err)
	}
	for i := 5; i < 10; i++ {
		if ex.args[i] != nil {
			t.Errorf("arg %d must be NULL, got %#v", i, ex.args[i])
		}
	}
	if ex.args[3] != "BLOCKED_POLICY" || ex.args[4] != "credential_unavailable" {
		t.Errorf("state/code %v %v", ex.args[3], ex.args[4])
	}
}

func TestRouteFinishTotalOverProviderData(t *testing.T) {
	f := newFixture(t, ecpay.EnvSandbox, enabled(), nil)
	// Whatever the provider sent (control characters, huge or multibyte values) is passed through as
	// data; only a database fault is an error, and it carries no detail.
	weird := detail{LogisticsID: "1234567", PaymentNo: strings.Repeat("字", 100), ValidationNo: "\x07\x1b[31m", StatusCode: "not-a-number"}
	if err := f.a.finishOn(context.Background(), &fakeExec{}, claim("reconcile"), core.Outcome{State: "UNKNOWN", Code: "ecpay.uncertain", Detail: weird}); err != nil {
		t.Fatalf("provider data must never be an error: %v", err)
	}
	err := f.a.finishOn(context.Background(), &fakeExec{err: &pgconn.PgError{Code: "40001", Message: "detail " + tKey}}, claim("dispatch"), core.Outcome{State: "UNKNOWN", Code: "x"})
	if err == nil || strings.Contains(err.Error(), tKey) || !errors.Is(err, errFinish) {
		t.Fatalf("db fault must be errFinish without detail: %v", err)
	}
}

// ---- Routes / dispatcher shape -------------------------------------------------------------------

func TestRoutesShape(t *testing.T) {
	f := newFixture(t, ecpay.EnvSandbox, enabled(), nil)
	routes, err := newRoutes(f.keys, f.client, enabled())
	if err != nil || len(routes) != 1 {
		t.Fatalf("%v %d", err, len(routes))
	}
	r := routes[0]
	if r.Provider != "ecpay_logistics" || r.Action != "ecpay.cvs_create" || r.Purpose != "transactional" ||
		r.Check == nil || r.LoadSecret == nil || r.DispatchWithSecret == nil || r.ReconcileWithSecret == nil || r.Finish == nil ||
		r.Dispatch != nil || r.Reconcile != nil {
		t.Fatalf("route shape %+v", r)
	}
	// The dispatcher's route compiler accepts the shape: NewDispatcher gets past compileDispatchRoutes
	// and fails only on the (nil) worker pool.
	_, err = core.NewDispatcher(context.Background(), nil, routes, core.DefaultDispatcherOptions())
	if err == nil || !strings.Contains(err.Error(), "worker database pool required") {
		t.Fatalf("route rejected by the dispatcher compiler or wrong failure: %v", err)
	}
	if _, err := newRoutes(nil, f.client, enabled()); !errors.Is(err, errConfig) {
		t.Error("nil keyring")
	}
	if _, err := newRoutes(f.keys, nil, enabled()); !errors.Is(err, errConfig) {
		t.Error("nil client")
	}
	if _, err := Routes(nil, f.keys, f.client, enabled()); !errors.Is(err, errConfig) {
		t.Error("nil pool")
	}
}

func TestRouteOutcomesAreValidForTheDispatcher(t *testing.T) {
	// Every outcome the route can produce satisfies the dispatcher's own shape rules (state, code
	// pattern, reference), so it is never demoted to adapter_result_invalid.
	code := func(c string) bool {
		return len(c) >= 1 && len(c) <= 80 && strings.Trim(c, "abcdefghijklmnopqrstuvwxyz0123456789_.") == ""
	}
	for _, r := range []ecpay.Result{
		{Outcome: "SUCCEEDED", Code: ecpay.CodeCreated, LogisticsID: "1234567"},
		{Outcome: "SUCCEEDED", Code: ecpay.CodeQueryFound, LogisticsID: "a_b-1"},
		{Outcome: "FAILED_FINAL", Code: ecpay.CodeRejected},
		{Outcome: "FAILED_FINAL", Code: ecpay.CodeInvalidRequest},
		{Outcome: "UNKNOWN", Code: ecpay.CodeRateLimited},
		{Outcome: "UNKNOWN", Code: ecpay.CodeUncertain},
	} {
		o := outcomeOf(r)
		if !code(o.Code) || (o.State != "SUCCEEDED" && o.ProviderReference != "") {
			t.Errorf("%+v", o)
		}
	}
}
