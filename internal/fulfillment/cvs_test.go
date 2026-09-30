package fulfillment

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"livecommerce/internal/command"
	"livecommerce/internal/integrations/shipping/ecpay"
	"livecommerce/internal/platform"
)

// TestValidBuyerStoreCodeGolden replays the shared golden table (§16.1); the same file feeds the SQL twin in cvs-tests.
func TestValidBuyerStoreCodeGolden(t *testing.T) {
	f, err := os.Open("testdata/cvs_store_codes.golden")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows := 0
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := scan.Text()
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		cols := strings.Split(line, "\t")
		if len(cols) != 3 {
			t.Fatalf("golden row %q", line)
		}
		rows++
		if got := ValidBuyerStoreCode(cols[0], cols[1]); got != (cols[2] == "true") {
			t.Errorf("ValidBuyerStoreCode(%q,%q)=%v, golden says %s", cols[0], cols[1], got, cols[2])
		}
	}
	if rows < 20 {
		t.Fatalf("golden table shrank to %d rows", rows)
	}
}

func TestConnectionIDIsDeterministicUUIDv4(t *testing.T) {
	const tenant, store = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	a := ConnectionID(tenant, store, "SANDBOX")
	if a != ConnectionID(tenant, store, "SANDBOX") || !command.ValidID(a) || a[14] != '4' || !strings.ContainsRune("89ab", rune(a[19])) {
		t.Fatalf("not a stable v4 uuid: %s", a)
	}
	if a == ConnectionID(tenant, store, "LIVE") || a == ConnectionID(store, tenant, "SANDBOX") {
		t.Fatal("connection id ignores environment or scope")
	}
}

func TestMapCVSError(t *testing.T) {
	pg := func(code, msg string) error { return &pgconn.PgError{Code: code, Message: msg} }
	refusal := func(err error, status int, code string, retry int) bool {
		var ce *CVSError
		return errors.As(err, &ce) && ce.Status == status && ce.Code == code && ce.RetryAfter == retry
	}
	if got := mapCVSError(pg("PT409", "version_changed")); !refusal(got, 409, "version_changed", 0) {
		t.Fatalf("PT409 coded: %v", got)
	}
	if got := mapCVSError(pg("PT409", "cart changed")); !errors.Is(got, command.ErrConflict) {
		t.Fatalf("PT409 uncoded must be a plain conflict: %v", got)
	}
	if got := mapCVSError(pg("PT422", "cvs_amount_exceeds")); !refusal(got, 422, "cvs_amount_exceeds", 0) {
		t.Fatalf("PT422: %v", got)
	}
	if got := mapCVSError(pg("PT422", "Not A Code")); !errors.Is(got, command.ErrInvalid) {
		t.Fatalf("PT422 outside the code grammar: %v", got)
	}
	if got := mapCVSError(pg("PT429", "pay_at_pickup_limit")); !refusal(got, 429, "pay_at_pickup_limit", 60) {
		t.Fatalf("PT429: %v", got)
	}
	for code, want := range map[string]error{"PT400": command.ErrInvalid, "22023": command.ErrInvalid, "PT401": platform.ErrUnauthorized,
		"PT403": platform.ErrForbidden, "PT404": command.ErrNotFound, "PT2RP": errCVSReplay} {
		if got := mapCVSError(pg(code, "x")); !errors.Is(got, want) {
			t.Fatalf("%s -> %v, want %v", code, got, want)
		}
	}
	if got := mapCVSError(pg("40P01", "deadlock")); got == nil || errors.Is(got, command.ErrConflict) {
		t.Fatalf("a deadlock must pass through for the retryable 503 class: %v", got)
	}
	if mapCVSError(nil) != nil {
		t.Fatal("nil error")
	}
}

func TestValidSettings(t *testing.T) {
	max := 20000
	ok := Settings{EnabledChains: []string{"cvs_711", "cvs_okmart"}, PayAtPickupEnabled: true, PayAtPickupMaxTWD: &max, PayAtPickupMaxOpen: 20}
	if !validSettings(ok) {
		t.Fatal("valid settings rejected")
	}
	over, zero := 20001, 0
	for name, edit := range map[string]func(*Settings){
		"unknown chain":        func(s *Settings) { s.EnabledChains = []string{"cvs_seven"} },
		"duplicate chain":      func(s *Settings) { s.EnabledChains = []string{"cvs_711", "cvs_711"} },
		"cap above 20000":      func(s *Settings) { s.PayAtPickupMaxTWD = &over },
		"cap zero":             func(s *Settings) { s.PayAtPickupMaxTWD = &zero },
		"enabled without cap":  func(s *Settings) { s.PayAtPickupMaxTWD = nil },
		"open slots zero":      func(s *Settings) { s.PayAtPickupMaxOpen = 0 },
		"open slots above 500": func(s *Settings) { s.PayAtPickupMaxOpen = 501 },
		"negative version":     func(s *Settings) { s.Version = -1 },
		"more than four chains": func(s *Settings) {
			s.EnabledChains = []string{"cvs_711", "cvs_familymart", "cvs_hilife", "cvs_okmart", "x"}
		},
	} {
		s := ok
		edit(&s)
		if validSettings(s) {
			t.Errorf("%s accepted", name)
		}
	}
	off := Settings{EnabledChains: []string{}, PayAtPickupMaxOpen: 20}
	if !validSettings(off) {
		t.Fatal("disabled pay-at-pickup with no cap must be valid")
	}
}

func TestValidShipmentView(t *testing.T) {
	one := 1
	attempt := CVSAttempt{Attempt: 1, State: "CREATED", Environment: "SANDBOX", GoodsAmount: 25, Version: 3, Alerts: []string{}, Events: []CVSEvent{}}
	ok := ShipmentView{CurrentAttempt: &one, ExpectedVersion: 3, Attempts: []CVSAttempt{attempt}}
	if !validShipmentView(ok) {
		t.Fatal("valid view rejected")
	}
	if !validShipmentView(ShipmentView{Attempts: []CVSAttempt{}}) {
		t.Fatal("empty view rejected")
	}
	bad := map[string]func(*ShipmentView){
		"expected version drift": func(v *ShipmentView) { v.ExpectedVersion = 2 },
		"unknown state":          func(v *ShipmentView) { v.Attempts[0].State = "SHIPPED" },
		"collection differs":     func(v *ShipmentView) { c := 30; v.Attempts[0].CollectionAmount = &c },
		"amount above ECPay cap": func(v *ShipmentView) { v.Attempts[0].GoodsAmount = 20001 },
		"attempt number gap":     func(v *ShipmentView) { v.Attempts[0].Attempt = 2 },
		"null events":            func(v *ShipmentView) { v.Attempts[0].Events = nil },
	}
	for name, edit := range bad {
		v := ok
		v.Attempts = append([]CVSAttempt(nil), ok.Attempts...)
		edit(&v)
		if validShipmentView(v) {
			t.Errorf("%s accepted", name)
		}
	}
	if validShipmentView(ShipmentView{CurrentAttempt: &one, Attempts: []CVSAttempt{}}) {
		t.Fatal("current attempt without attempts accepted")
	}
}

func TestEndpointGatesCap(t *testing.T) {
	g := newEndpointGates(4)
	for i := 0; i < 4; i++ {
		if !g.enter("a") {
			t.Fatalf("entry %d refused", i)
		}
	}
	if g.enter("a") {
		t.Fatal("fifth concurrent post admitted")
	}
	if !g.enter("b") {
		t.Fatal("another endpoint must not share the cap")
	}
	g.leave("a")
	if !g.enter("a") {
		t.Fatal("slot not released")
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if g.enter("c") {
				g.leave("c")
			}
		}()
	}
	wg.Wait()
	if len(g.n) != 2 {
		t.Fatalf("gate map leaked entries: %v", g.n)
	}
}

func TestNewUUIDv4(t *testing.T) {
	a, err := newUUIDv4()
	b, _ := newUUIDv4()
	if err != nil || a == b || !command.ValidID(a) || a[14] != '4' {
		t.Fatalf("uuid %s %s %v", a, b, err)
	}
}

// cvsForTests builds a service whose pool is never touched: every case below is refused before any SQL.
func cvsForTests(t *testing.T, cfg CVSConfig) *CVS {
	t.Helper()
	jobs := newTestJobs(t)
	var keys *ecpay.Keyring
	var client *ecpay.Client
	if cfg.ECPay.Enabled {
		var err error
		if client, err = ecpay.NewClient(ecpay.Environment(cfg.PaymentEnvironment), nil); err != nil {
			t.Fatal(err)
		}
		keys = &ecpay.Keyring{}
	}
	c, err := NewCVS(&pgxpool.Pool{}, jobs, keys, client, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewCVSStartupRules(t *testing.T) {
	jobs := newTestJobs(t)
	pool := &pgxpool.Pool{}
	if _, err := NewCVS(nil, jobs, nil, nil, CVSConfig{}); err == nil {
		t.Fatal("nil pool accepted")
	}
	if _, err := NewCVS(pool, nil, nil, nil, CVSConfig{}); err == nil {
		t.Fatal("nil jobs accepted")
	}
	if _, err := NewCVS(pool, jobs, nil, nil, CVSConfig{PaymentEnvironment: "PROVIDER_MOCK"}); err == nil {
		t.Fatal("unmapped payment environment accepted")
	}
	// C9: CVS_ECPAY_ENABLED=1 needs an environment to pin, keys and a client.
	client, _ := ecpay.NewClient("SANDBOX", nil)
	enabled := ecpay.Config{Enabled: true, HooksOrigin: "https://hooks.example.test"}
	if _, err := NewCVS(pool, jobs, &ecpay.Keyring{}, client, CVSConfig{ECPay: enabled}); err == nil {
		t.Fatal("ECPay enabled with buyer payment off accepted")
	}
	if _, err := NewCVS(pool, jobs, nil, client, CVSConfig{ECPay: enabled, PaymentEnvironment: "SANDBOX"}); err == nil {
		t.Fatal("ECPay enabled without keys accepted")
	}
	if _, err := NewCVS(pool, jobs, &ecpay.Keyring{}, client, CVSConfig{ECPay: enabled, PaymentEnvironment: "SANDBOX"}); err != nil {
		t.Fatalf("valid enabled config: %v", err)
	}
	if _, err := NewCVS(pool, jobs, nil, nil, CVSConfig{}); err != nil {
		t.Fatalf("disabled config must not need ECPay: %v", err)
	}
}

func TestConnectRefusalsHappenBeforeAnyIO(t *testing.T) {
	c := cvsForTests(t, CVSConfig{ECPay: ecpay.Config{Enabled: true, HooksOrigin: "https://hooks.example.test"}, PaymentEnvironment: "SANDBOX"})
	in := ConnectInput{ExpectedVersion: 0, Environment: "SANDBOX", Mode: "C2C", MerchantID: "2000933", HashKey: "abcdefghijklmnop", HashIV: "ABCDEFGHIJKLMNOP",
		SenderName: "王小明明", SenderCellPhone: "0912345678"}
	const store = "22222222-2222-4222-8222-222222222222"
	key, token := "connect-key-0001", strings.Repeat("t", 43)
	code := func(err error) string {
		var ce *CVSError
		if errors.As(err, &ce) {
			return ce.Code
		}
		return "other:" + errors.Unwrap(err).Error()
	}
	for name, tc := range map[string]struct {
		edit func(*ConnectInput)
		want string
	}{
		"LIVE store on a SANDBOX deployment": {func(i *ConnectInput) { i.Environment = "LIVE" }, "ecpay_environment_not_allowed"},
		"sender digits in the name":          {func(i *ConnectInput) { i.SenderName = "Amy2" }, "invalid_sender"},
		"landline sender":                    {func(i *ConnectInput) { i.SenderCellPhone = "0223456789" }, "invalid_sender"},
	} {
		x := in
		tc.edit(&x)
		if _, err := c.Connect(context.Background(), token, store, key, x); code(err) != tc.want {
			t.Errorf("%s: %v (%s)", name, err, code(err))
		}
	}
	for name, edit := range map[string]func(*ConnectInput){
		"bad environment": func(i *ConnectInput) { i.Environment = "STAGING" }, "bad mode": func(i *ConnectInput) { i.Mode = "HOME" },
		"long merchant id": func(i *ConnectInput) { i.MerchantID = "20009331234" }, "empty key": func(i *ConnectInput) { i.HashKey = "" },
		"space in iv": func(i *ConnectInput) { i.HashIV = "AB CD" }, "negative version": func(i *ConnectInput) { i.ExpectedVersion = -1 },
	} {
		x := in
		edit(&x)
		if _, err := c.Connect(context.Background(), token, store, key, x); !errors.Is(err, command.ErrInvalid) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
	if _, err := c.Connect(context.Background(), token, store, "short", in); !errors.Is(err, command.ErrInvalid) {
		t.Fatalf("bad idempotency key: %v", err)
	}
	off := cvsForTests(t, CVSConfig{PaymentEnvironment: "SANDBOX"})
	if _, err := off.Connect(context.Background(), token, store, key, in); !errors.Is(err, ErrECPayDisabled) {
		t.Fatalf("connect with ECPay off: %v", err)
	}
	if got := in.String(); strings.Contains(got, "abcdefghijklmnop") || strings.Contains(got, "ABCDEFGHIJKLMNOP") {
		t.Fatalf("secrets in String(): %s", got)
	}
}

func TestRequestAndActionInputChecks(t *testing.T) {
	c := cvsForTests(t, CVSConfig{ECPay: ecpay.Config{Enabled: true, HooksOrigin: "https://hooks.example.test"}, PaymentEnvironment: "SANDBOX"})
	const store, order = "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"
	token, key := strings.Repeat("t", 43), "request-key-0001"
	ctx := context.Background()
	if _, err := c.Request(ctx, token, store, "bad", order, RequestInput{}); !errors.Is(err, command.ErrInvalid) {
		t.Fatalf("bad key: %v", err)
	}
	if _, err := c.Request(ctx, token, store, key, "not-a-uuid", RequestInput{}); !errors.Is(err, command.ErrInvalid) {
		t.Fatalf("bad order: %v", err)
	}
	off := cvsForTests(t, CVSConfig{PaymentEnvironment: "SANDBOX"})
	var ce *CVSError
	if _, err := off.Request(ctx, token, store, key, order, RequestInput{}); !errors.As(err, &ce) || ce.Code != "connection_unavailable" || ce.Status != 422 {
		t.Fatalf("request with ECPay off: %v", err)
	}
	for name, err := range map[string]error{
		"collection state": func() error {
			_, e := c.RecordCollection(ctx, token, store, key, order, CollectionInput{ExpectedState: "PAID", State: "collected"})
			return e
		}(),
		"collection value": func() error {
			_, e := c.RecordCollection(ctx, token, store, key, order, CollectionInput{ExpectedState: "PENDING", State: "restocked"})
			return e
		}(),
		"release action": func() error {
			_, e := c.Release(ctx, token, store, key, order, ReleaseInput{Action: "refund", ExpectedState: "PENDING"})
			return e
		}(),
		"cancel of a returned order": func() error {
			_, e := c.Release(ctx, token, store, key, order, ReleaseInput{Action: "cancel", ExpectedState: "RETURNED"})
			return e
		}(),
		"restock of a pending order": func() error {
			_, e := c.Release(ctx, token, store, key, order, ReleaseInput{Action: "restock", ExpectedState: "PENDING"})
			return e
		}(),
		"abandon without version": func() error { _, e := c.Abandon(ctx, token, store, key, order, AbandonInput{}); return e }(),
		"settings duplicate chain": func() error {
			_, e := c.SetSettings(ctx, token, store, key, SettingsInput{EnabledChains: []string{"cvs_711", "cvs_711"}, PayAtPickupMaxOpen: 5})
			return e
		}(),
	} {
		if err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := c.Enable(ctx, token, store, key, EnableInput{ExpectedVersion: 0}); !errors.Is(err, command.ErrInvalid) {
		t.Fatalf("enable at version 0: %v", err)
	}
}

func TestHooksHandlerRefusalsBeforeAnyDatabaseWork(t *testing.T) {
	disabled := cvsForTests(t, CVSConfig{PaymentEnvironment: "SANDBOX"}).HooksHandler()
	rec := httptest.NewRecorder()
	disabled.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/cvs/ecpay/status/33333333-3333-4333-8333-333333333333", strings.NewReader("x")))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("hooks with ECPay off: %d", rec.Code)
	}
	c := cvsForTests(t, CVSConfig{ECPay: ecpay.Config{Enabled: true, HooksOrigin: "https://hooks.example.test"}, PaymentEnvironment: "SANDBOX"})
	h := c.HooksHandler()
	post := func(path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		return rec
	}
	if rec = post("/v1/cvs/ecpay/map-return/not-a-uuid", "x=y"); rec.Code != 404 || rec.Header().Get("Location") != "" {
		t.Fatalf("bad selection id: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec = post("/v1/cvs/ecpay/status/not-a-uuid", "x=y"); rec.Code != 404 {
		t.Fatalf("bad endpoint id: %d", rec.Code)
	}
	if rec = post("/v1/cvs/ecpay/map-return/33333333-3333-4333-8333-333333333333", strings.Repeat("a", mapReturnMaxBody+1)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize map return: %d", rec.Code)
	}
	if rec = post("/v1/cvs/ecpay/status/33333333-3333-4333-8333-333333333333", strings.Repeat("a", statusMaxBody+1)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status: %d", rec.Code)
	}
	if rec = post("/v1/cvs/ecpay/status/33333333-3333-4333-8333-333333333333?leak=1", "x=y"); rec.Code != http.StatusBadRequest {
		t.Fatalf("query string on the status hook: %d", rec.Code)
	}
	for _, path := range []string{"/v1/cvs/ecpay/other", "/v1/cvs/unknown", "/v1/cvs/ecpay/status/"} {
		if rec = post(path, ""); rec.Code != http.StatusNotFound {
			t.Fatalf("%s: %d", path, rec.Code)
		}
	}
	// The per-endpoint cap answers 503 before the body is even read.
	endpoint := "33333333-3333-4333-8333-333333333333"
	for i := 0; i < 4; i++ {
		c.gates.enter(endpoint)
	}
	if rec = post("/v1/cvs/ecpay/status/"+endpoint, "x=y"); rec.Code != http.StatusServiceUnavailable || rec.Body.String() == "1|OK" {
		t.Fatalf("saturated endpoint: %d %q", rec.Code, rec.Body.String())
	}
	// Close-out P2: the unauthenticated map-return hook is capped per selection and globally; the gate answers 503 "busy" before any
	// transaction (an exhausted gate must never reach the pool), and is taken only after the body was read.
	selection := "44444444-4444-4444-8444-444444444444"
	mapBody := "MerchantID=2000132&MerchantTradeNo=ABC123&LogisticsSubType=UNIMARTC2C&CVSStoreID=131386&CVSOutSide=0"
	for i := 0; i < 4; i++ {
		c.gates.enter("map:" + selection)
	}
	if rec = post("/v1/cvs/ecpay/map-return/"+selection, mapBody); rec.Code != http.StatusServiceUnavailable || rec.Body.String() != "busy" {
		t.Fatalf("saturated selection on the map hook: %d %q", rec.Code, rec.Body.String())
	}
	for i := 0; i < mapGlobalCap; i++ {
		c.mapGates.enter("*")
	}
	if rec = post("/v1/cvs/ecpay/map-return/55555555-5555-4555-8555-555555555555", mapBody); rec.Code != http.StatusServiceUnavailable || rec.Body.String() != "busy" {
		t.Fatalf("saturated global map gate: %d %q", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/cvs/ecpay/status/"+endpoint, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET on the status hook: %d", rec.Code)
	}
}

// Close-out P3: the adapter and ECPay itself use digit-only MerchantIDs (ecpay.merchantIDRE); a lettered id used to pass Connect and
// fail later as ecpay_probe_failed. It is refused as invalid input before any network call.
func TestConnectRefusesLetteredMerchantID(t *testing.T) {
	c := cvsForTests(t, CVSConfig{ECPay: ecpay.Config{Enabled: true, HooksOrigin: "https://hooks.example.test"}, PaymentEnvironment: "SANDBOX"})
	in := ConnectInput{Environment: "SANDBOX", Mode: "C2C", MerchantID: "ABC1234", HashKey: "unit-hash-key", HashIV: "unit-hash-iv",
		SenderName: "寄件人測試", SenderCellPhone: "0911222333"}
	if _, err := c.Connect(context.Background(), strings.Repeat("t", 43), "22222222-2222-4222-8222-222222222222", "connect-key-0001", in); !errors.Is(err, command.ErrInvalid) {
		t.Fatalf("connect with a lettered merchant id: %v", err)
	}
}
