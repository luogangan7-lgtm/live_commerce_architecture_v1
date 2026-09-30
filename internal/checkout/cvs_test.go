package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"livecommerce/internal/command"
	"livecommerce/internal/fulfillment"
)

func TestNewNonce(t *testing.T) {
	seen := map[string]bool{}
	alphabet := regexp.MustCompile(`^[A-Za-z0-9]{20}$`)
	for i := 0; i < 500; i++ {
		n, err := newNonce()
		if err != nil || !alphabet.MatchString(n) || seen[n] {
			t.Fatalf("nonce %q: %v (dup=%v)", n, err, seen[n])
		}
		seen[n] = true
	}
}

func TestBoundedText(t *testing.T) {
	for _, tc := range []struct {
		value    string
		min, max int
		ok       bool
	}{
		{"7-ELEVEN 忠孝門市", 1, 40, true}, {"台北市中正區忠孝西路一段 1 號", 5, 120, true},
		{"", 1, 40, false}, {" padded", 1, 40, false}, {"padded ", 1, 40, false}, {"tab\there", 1, 40, false},
		{"line\nbreak", 1, 40, false}, {strings.Repeat("店", 41), 1, 40, false}, {strings.Repeat("店", 40), 1, 40, true},
		{"1234", 5, 120, false}, {string([]byte{0xff, 0xfe}), 1, 40, false},
	} {
		if got := boundedText(tc.value, tc.min, tc.max); got != tc.ok {
			t.Errorf("boundedText(%q,%d,%d)=%v want %v", tc.value, tc.min, tc.max, got, tc.ok)
		}
	}
}

func TestUnavailableReason(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		kind                  string
		ok, hilife, ecpayIsOn bool
		want                  string
	}{
		{"kill switch beats everything", "cvs_711", true, true, false, "temporarily_unavailable"},
		{"OK mart unverified", "cvs_okmart", false, true, true, "coming_soon"},
		{"OK mart verified", "cvs_okmart", true, false, true, ""},
		{"Hi-Life unverified", "cvs_hilife", true, false, true, "coming_soon"},
		{"Hi-Life verified", "cvs_hilife", false, true, true, ""},
		{"7-ELEVEN needs no gate", "cvs_711", false, false, true, ""},
		{"FamilyMart needs no gate", "cvs_familymart", false, false, true, ""},
	} {
		if got := unavailableReason(tc.kind, tc.ok, tc.hilife, tc.ecpayIsOn); got != tc.want {
			t.Errorf("%s: %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestCVSRefusalMapping(t *testing.T) {
	var refusal *fulfillment.CVSError
	err := safeError(context.Background(), &pgconn.PgError{Code: "PT422", Message: "cvs_amount_exceeds"})
	if !errors.As(err, &refusal) || refusal.Status != 422 || refusal.Code != "cvs_amount_exceeds" {
		t.Fatalf("PT422: %v", err)
	}
	err = safeError(context.Background(), &pgconn.PgError{Code: "PT429", Message: "pay_at_pickup_limit"})
	if !errors.As(err, &refusal) || refusal.Status != 429 || refusal.Code != "pay_at_pickup_limit" || refusal.RetryAfter != 60 {
		t.Fatalf("PT429: %v", err)
	}
	if err = safeError(context.Background(), &pgconn.PgError{Code: "PT422", Message: "Not A Code!"}); !errors.Is(err, command.ErrInvalid) {
		t.Fatalf("ungrammatical PT422: %v", err)
	}
	if err = cvsError(context.Background(), &pgconn.PgError{Code: "PT409", Message: "selection_replay_new_key"}); !errors.As(err, &refusal) ||
		refusal.Status != 409 || refusal.Code != "selection_replay_new_key" {
		t.Fatalf("replay: %v", err)
	}
	if err = cvsError(context.Background(), &pgconn.PgError{Code: "PT409", Message: "cart changed"}); !errors.Is(err, command.ErrConflict) {
		t.Fatalf("uncoded PT409 stays a plain conflict: %v", err)
	}
}

func TestBeginInputPaymentMode(t *testing.T) {
	in := Input{QuoteID: "11111111-1111-4111-8111-111111111111", DestinationID: "22222222-2222-4222-8222-222222222222", CartVersion: 1, ServiceVersion: 1, AllocationVersion: 1}
	for _, mode := range []string{"", "card", "pay_at_pickup"} {
		in.PaymentMode = mode
		if !validInput(in) {
			t.Errorf("payment_mode %q rejected", mode)
		}
	}
	for _, mode := range []string{"cash", "PAY_AT_PICKUP", "cod", " card"} {
		in.PaymentMode = mode
		if validInput(in) {
			t.Errorf("payment_mode %q accepted", mode)
		}
	}
	// omitempty: a card request digests exactly as it did before the CVS migration, so old replays still match.
	in.PaymentMode = ""
	raw, _ := json.Marshal(in)
	if strings.Contains(string(raw), "payment_mode") {
		t.Fatalf("empty payment_mode changes the request digest: %s", raw)
	}
	if commercialAtPlacement("pay_at_pickup") != "CONFIRMED" || commercialAtPlacement("card") != "DRAFT" {
		t.Fatal("placement state")
	}
}

func TestServiceCopiesCarryPaymentEnvironmentAndCVS(t *testing.T) {
	var nilService *Service
	if nilService.WithPaymentEnvironment("LIVE") != nil || nilService.WithBuyerCVS(nil) != nil || nilService.CVS() != nil {
		t.Fatal("nil service must stay nil")
	}
	base := &Service{}
	live := base.WithPaymentEnvironment("LIVE")
	if live == base || live.paymentEnv != "LIVE" || base.paymentEnv != "" {
		t.Fatal("WithPaymentEnvironment must copy")
	}
	if bogus := base.WithPaymentEnvironment("PROVIDER_MOCK"); bogus.paymentEnv != "" {
		t.Fatalf("an unmapped environment must mean ECPay off, got %q", bogus.paymentEnv)
	}
	buyerCVS := &BuyerCVS{}
	withCVS := live.WithBuyerCVS(buyerCVS)
	if withCVS.CVS() != buyerCVS || live.CVS() != nil || withCVS.paymentEnv != "LIVE" {
		t.Fatal("WithBuyerCVS must copy and keep the environment")
	}
}

func TestOpenAndEnterStoreRefusalsBeforeSQL(t *testing.T) {
	b := &BuyerCVS{cfg: fulfillment.CVSConfig{PaymentEnvironment: "SANDBOX"}}
	ctx, token := context.Background(), strings.Repeat("t", 43)
	const store, market = "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"
	good := SelectionOpenInput{CartVersion: 1, MarketID: market, ServiceCode: "cvs_711", ReturnPath: "/zh-TW/products/abc"}
	code := func(err error) string {
		var ce *fulfillment.CVSError
		if errors.As(err, &ce) {
			return ce.Code
		}
		return ""
	}
	for name, tc := range map[string]struct {
		edit func(*SelectionOpenInput)
		want string
	}{
		"claim return path":         {func(i *SelectionOpenInput) { i.ReturnPath = "/zh-TW/claim" }, "bad_return_path"},
		"other locale":              {func(i *SelectionOpenInput) { i.ReturnPath = "/fr/products/abc" }, "bad_return_path"},
		"traversal":                 {func(i *SelectionOpenInput) { i.ReturnPath = "/zh-TW/products/../x" }, "bad_return_path"},
		"query in path":             {func(i *SelectionOpenInput) { i.ReturnPath = "/en/products/abc?x=1" }, "bad_return_path"},
		"ECPay off (no config yet)": {func(i *SelectionOpenInput) {}, "service_unavailable"},
	} {
		in := good
		tc.edit(&in)
		if _, err := b.Open(ctx, token, store, "open-key-0001", "https://shop.example.test", false, in); code(err) != tc.want {
			t.Errorf("%s: %v (%q)", name, err, code(err))
		}
	}
	for name, in := range map[string]SelectionOpenInput{
		"cart version": {MarketID: market, ServiceCode: "cvs_711", ReturnPath: good.ReturnPath},
		"market":       {CartVersion: 1, MarketID: "x", ServiceCode: "cvs_711", ReturnPath: good.ReturnPath},
		"service code": {CartVersion: 1, MarketID: market, ServiceCode: "Bad Code", ReturnPath: good.ReturnPath},
	} {
		if _, err := b.Open(ctx, token, store, "open-key-0001", "https://shop.example.test", false, in); !errors.Is(err, command.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	entry := StoreEntryInput{CartVersion: 1, MarketID: market, ServiceCode: "cvs_711", StoreCode: "123456", StoreName: "門市", StoreAddress: "台北市中正區忠孝西路一段 1 號"}
	for name, tc := range map[string]struct {
		edit func(*StoreEntryInput)
		want string
	}{
		"letters in code": {func(i *StoreEntryInput) { i.StoreCode = "12a456" }, "bad_store_code"},
		"short code":      {func(i *StoreEntryInput) { i.StoreCode = "12" }, "bad_store_code"},
		"empty name":      {func(i *StoreEntryInput) { i.StoreName = "" }, "bad_store_name"},
		"control in name": {func(i *StoreEntryInput) { i.StoreName = "a\x00b" }, "bad_store_name"},
		"short address":   {func(i *StoreEntryInput) { i.StoreAddress = "abc" }, "bad_store_address"},
		"padded address":  {func(i *StoreEntryInput) { i.StoreAddress = " 台北市中正區忠孝西路一段" }, "bad_store_address"},
		"long address":    {func(i *StoreEntryInput) { i.StoreAddress = strings.Repeat("址", 121) }, "bad_store_address"},
	} {
		in := entry
		tc.edit(&in)
		if _, err := b.EnterStore(ctx, token, store, "enter-key-0001", in); code(err) != tc.want {
			t.Errorf("%s: %v (%q)", name, err, code(err))
		}
	}
	if _, err := b.EnterStore(ctx, token, store, "bad", entry); !errors.Is(err, command.ErrInvalid) {
		t.Fatalf("bad key: %v", err)
	}
	var nilB *BuyerCVS
	if _, err := nilB.Get(ctx, token, store, market); !errors.Is(err, command.ErrInvalid) {
		t.Fatalf("nil surface: %v", err)
	}
}

func TestSelectionRowView(t *testing.T) {
	reject := "expired"
	pick := &SelectionPickup{PickupID: "44444444-4444-4444-8444-444444444444", Kind: "cvs_711", Code: "131386", Name: "n", Address: "a"}
	ok := map[string]selectionRow{
		"open":     {SelectionID: "s", State: "OPEN"},
		"returned": {SelectionID: "s", State: "RETURNED"},
		"expired":  {SelectionID: "s", State: "EXPIRED"},
		"verified": {SelectionID: "s", State: "VERIFIED", Pickup: pick},
		"rejected": {SelectionID: "s", State: "REJECTED", RejectCode: &reject},
	}
	for name, row := range ok {
		if _, err := row.view(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	bad := map[string]selectionRow{
		"verified without pickup": {State: "VERIFIED"},
		"open with pickup":        {State: "OPEN", Pickup: pick},
		"rejected without code":   {State: "REJECTED"},
		"open with reject code":   {State: "OPEN", RejectCode: &reject},
		"unknown state":           {State: "DONE"},
	}
	for name, row := range bad {
		if _, err := row.view(); !errors.Is(err, command.ErrConflict) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}

func TestSearchURLsAreHTTPSAndCoverEveryChain(t *testing.T) {
	for _, kind := range []string{"cvs_711", "cvs_familymart", "cvs_hilife", "cvs_okmart"} {
		if u := cvsSearchURLs[kind]; !strings.HasPrefix(u, "https://") {
			t.Errorf("%s search url %q", kind, u)
		}
	}
	if len(cvsSearchURLs) != 4 {
		t.Fatal("unexpected search url table")
	}
}
