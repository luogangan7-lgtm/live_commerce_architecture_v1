// body_test.go: SP02 (currency vectors, UNIT part) and SP03 (golden create body,
// exact key set, headers, byte-identical resend). Non-goal: SP02 Go↔SQL parity needs
// migration 0061 (integrator) and is NOT_RUN here.
// Callers: go test ./internal/integrations/psp/stripe/...

package stripe

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
)

func TestStripeSP02Currency(t *testing.T) {
	ok := []struct {
		cur string
		amt int64
	}{
		{"HKD", 400}, {"HKD", 401}, {"HKD", 99_999_999},
		{"USD", 50}, {"USD", 99_999_999},
		{"SGD", 50}, {"SGD", 12_345},
		{"MYR", 200}, {"MYR", 99_999_999},
		// TWD min 2500: Stripe SANDBOX rejected 100/1200, accepted 2500 (2026-09-29)
		{"TWD", 2500}, {"TWD", 12_300}, {"TWD", 99_999_900},
	}
	for _, v := range ok {
		got, err := UnitAmount(v.cur, v.amt)
		if err != nil || got != v.amt {
			t.Fatalf("%s %d: got %d %v", v.cur, v.amt, got, err)
		}
	}
	bad := []struct {
		cur string
		amt int64
	}{
		{"HKD", 399}, {"HKD", 100_000_000}, {"HKD", 0}, {"HKD", -400},
		{"USD", 49}, {"SGD", 49}, {"MYR", 199}, {"MYR", 100_000_000},
		{"TWD", 99}, {"TWD", 100}, {"TWD", 1200}, {"TWD", 2400}, {"TWD", 2499}, {"TWD", 150}, {"TWD", 12_345}, {"TWD", 99_999_901}, {"TWD", 100_000_000},
		// Rejected currencies: JPY (superseded by §0.1), ×100 and payout-special cases,
		// 3-decimal currencies, lowercase, empty and unknown codes.
		{"JPY", 500}, {"ISK", 500}, {"UGX", 500}, {"HUF", 500}, {"BHD", 500}, {"KWD", 500},
		{"EUR", 500}, {"hkd", 500}, {"", 500}, {"HKD ", 500},
		{"HKD", math.MaxInt64}, {"HKD", math.MinInt64},
	}
	for _, v := range bad {
		if _, err := UnitAmount(v.cur, v.amt); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%q %d admitted", v.cur, v.amt)
		}
	}
}

// goldenBody is written by hand from §5.4 (sorted keys, url.QueryEscape), not produced by
// EncodeCreateBody, so a change in encoding is caught.
const goldenBody = "adaptive_pricing%5Benabled%5D=false" +
	"&automatic_tax%5Benabled%5D=false" +
	"&cancel_url=https%3A%2F%2Fshop.example.test%2Fpayment%2Freturn" +
	"&client_reference_id=0b5e3c1a-7d2f-4e8a-9c61-2f4d8e7a1b30" +
	"&expires_at=1790000000" +
	"&line_items%5B0%5D%5Bprice_data%5D%5Bcurrency%5D=hkd" +
	"&line_items%5B0%5D%5Bprice_data%5D%5Bproduct_data%5D%5Bname%5D=Order+9F1C2E4D" +
	"&line_items%5B0%5D%5Bprice_data%5D%5Bunit_amount%5D=12345" +
	"&line_items%5B0%5D%5Bquantity%5D=1" +
	"&locale=zh-TW" +
	"&managed_payments%5Benabled%5D=false" +
	"&metadata%5Blc_attempt%5D=0b5e3c1a-7d2f-4e8a-9c61-2f4d8e7a1b30" +
	"&metadata%5Blc_order%5D=9f1c2e4d-5a6b-4c7d-8e9f-0a1b2c3d4e5f" +
	"&metadata%5Blc_profile%5D=SANDBOX" +
	"&metadata%5Blc_v%5D=1" +
	"&mode=payment" +
	"&payment_intent_data%5Bmetadata%5D%5Blc_attempt%5D=0b5e3c1a-7d2f-4e8a-9c61-2f4d8e7a1b30" +
	"&payment_intent_data%5Bmetadata%5D%5Blc_order%5D=9f1c2e4d-5a6b-4c7d-8e9f-0a1b2c3d4e5f" +
	"&payment_method_types%5B0%5D=card" +
	"&submit_type=pay" +
	"&success_url=https%3A%2F%2Fshop.example.test%2Fpayment%2Freturn" +
	"&ui_mode=hosted_page"

func TestStripeSP03CreateBody(t *testing.T) {
	body, err := EncodeCreateBody(fixtureParams())
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != goldenBody {
		t.Fatalf("golden mismatch:\n got %s\nwant %s", body, goldenBody)
	}
	for i := 0; i < 20; i++ { // map iteration order must not matter
		again, _ := EncodeCreateBody(fixtureParams())
		if string(again) != goldenBody {
			t.Fatal("non-deterministic body")
		}
	}
	for _, forbidden := range []string{"allow_promotion_codes", "discounts", "shipping", "optional_items",
		"adjustable_quantity", "after_expiration", "customer", "phone_number", "custom_fields", "custom_text",
		"consent_collection", "invoice_creation", "saved_payment_method", "capture_method", "CHECKOUT_SESSION_ID"} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("forbidden param %s present", forbidden)
		}
	}
	if k := CreateIdempotencyKey(fxAttempt); k != "lc:stripe:cs-create:v1:"+fxAttempt || len(k) > 255 {
		t.Fatalf("create key %q", k)
	}
	if k := ExpireIdempotencyKey(fxAttempt, 7); k != "lc:stripe:cs-expire:v1:"+fxAttempt+":7" {
		t.Fatalf("expire key %q", k)
	}

	t.Run("key set and values", func(t *testing.T) {
		mutate := map[string]func(map[string]string){
			"extra key":              func(f map[string]string) { f["customer_email"] = "a@b.test" },
			"extra forbidden":        func(f map[string]string) { f["allow_promotion_codes"] = "true" },
			"missing key":            func(f map[string]string) { delete(f, "ui_mode") },
			"swap key":               func(f map[string]string) { delete(f, "ui_mode"); f["ui_mode "] = "hosted_page" },
			"adaptive on":            func(f map[string]string) { f["adaptive_pricing[enabled]"] = "true" },
			"managed on":             func(f map[string]string) { f["managed_payments[enabled]"] = "true" },
			"quantity 2":             func(f map[string]string) { f["line_items[0][quantity]"] = "2" },
			"method link":            func(f map[string]string) { f["payment_method_types[0]"] = "link" },
			"mode setup":             func(f map[string]string) { f["mode"] = "setup" },
			"embedded":               func(f map[string]string) { f["ui_mode"] = "embedded" },
			"ref differs":            func(f map[string]string) { f["client_reference_id"] = fxOrder },
			"pi attempt differs":     func(f map[string]string) { f["payment_intent_data[metadata][lc_attempt]"] = fxOrder },
			"pi order differs":       func(f map[string]string) { f["payment_intent_data[metadata][lc_order]"] = fxAttempt },
			"uppercase uuid":         func(f map[string]string) { up(f, strings.ToUpper(fxAttempt)) },
			"bad profile":            func(f map[string]string) { f["metadata[lc_profile]"] = "PROD" },
			"bad locale":             func(f map[string]string) { f["locale"] = "zh-CN" },
			"product name catalog":   func(f map[string]string) { f["line_items[0][price_data][product_data][name]"] = "Red shoes" },
			"product name lowercase": func(f map[string]string) { f["line_items[0][price_data][product_data][name]"] = "Order 9f1c2e4d" },
			"uppercase currency":     func(f map[string]string) { f["line_items[0][price_data][currency]"] = "HKD" },
			"jpy":                    func(f map[string]string) { f["line_items[0][price_data][currency]"] = "jpy" },
			"below min":              func(f map[string]string) { f["line_items[0][price_data][unit_amount]"] = "399" },
			"leading zero amount":    func(f map[string]string) { f["line_items[0][price_data][unit_amount]"] = "012345" },
			"decimal amount":         func(f map[string]string) { f["line_items[0][price_data][unit_amount]"] = "123.45" },
			"twd not whole": func(f map[string]string) {
				f["line_items[0][price_data][currency]"] = "twd"
				f["line_items[0][price_data][unit_amount]"] = "12345"
			},
			"expires negative":   func(f map[string]string) { f["expires_at"] = "-1" },
			"expires overflow":   func(f map[string]string) { f["expires_at"] = "99999999999999" },
			"http url":           func(f map[string]string) { setURL(f, "http://shop.example.test/r") },
			"template url":       func(f map[string]string) { setURL(f, "https://shop.example.test/r?s={CHECKOUT_SESSION_ID}") },
			"userinfo url":       func(f map[string]string) { setURL(f, "https://u:p@shop.example.test/r") },
			"fragment url":       func(f map[string]string) { setURL(f, "https://shop.example.test/r#x") },
			"relative url":       func(f map[string]string) { setURL(f, "/payment/return") },
			"urls differ":        func(f map[string]string) { f["cancel_url"] = "https://shop.example.test/other" },
			"lc_v 2":             func(f map[string]string) { f["metadata[lc_v]"] = "2" },
			"automatic tax":      func(f map[string]string) { f["automatic_tax[enabled]"] = "true" },
			"submit_type donate": func(f map[string]string) { f["submit_type"] = "donate" },
		}
		for name, m := range mutate {
			f := fixtureFields()
			m(f)
			if _, err := EncodeCreateBody(CreateParams{Fields: f}); !errors.Is(err, ErrInvalid) {
				t.Fatalf("%s: admitted", name)
			}
		}
		if _, err := EncodeCreateBody(CreateParams{}); !errors.Is(err, ErrInvalid) {
			t.Fatal("nil fields admitted")
		}
		f := fixtureFields()
		f["line_items[0][price_data][currency]"] = "twd"
		f["line_items[0][price_data][unit_amount]"] = "12300"
		if _, err := EncodeCreateBody(CreateParams{Fields: f}); err != nil {
			t.Fatalf("whole TWD refused: %v", err)
		}
	})

	t.Run("headers and resend", func(t *testing.T) {
		var bodies []string
		rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			b, _ := io.ReadAll(r.Body)
			bodies = append(bodies, string(b))
			h := r.Header
			if r.Method != http.MethodPost || r.URL.String() != "https://api.stripe.com/v1/checkout/sessions" ||
				h.Get("Authorization") != "Bearer "+fakeTestKey || h.Get("Stripe-Version") != "2026-08-26.dahlia" ||
				h.Get("User-Agent") != "livecommerce-stripe/1" ||
				h.Get("Content-Type") != "application/x-www-form-urlencoded" ||
				h.Get("Idempotency-Key") != "lc:stripe:cs-create:v1:"+fxAttempt || h.Get("Stripe-Account") != "" {
				t.Errorf("unexpected request %s %s headers=%v", r.Method, r.URL, redactHeaders(h))
			}
			return jsonResponse(200, sessionJSON("cs_test_a1", "open", "unpaid", false), nil), nil
		})
		c, err := NewWithMockTransport(sandboxConfig(), rt)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ {
			s, _, err := c.CreateCheckoutSession(t.Context(), fixtureParams())
			if err != nil || s.ID != "cs_test_a1" {
				t.Fatalf("create: %v", err)
			}
		}
		for _, b := range bodies {
			if b != goldenBody {
				t.Fatal("resend not byte-identical to golden")
			}
		}
		sum := sha256.Sum256([]byte(goldenBody))
		t.Logf("golden create_body_sha256=%s", hex.EncodeToString(sum[:]))
	})
}

func up(f map[string]string, attempt string) {
	f["metadata[lc_attempt]"], f["client_reference_id"], f["payment_intent_data[metadata][lc_attempt]"] = attempt, attempt, attempt
}

func setURL(f map[string]string, u string) { f["cancel_url"], f["success_url"] = u, u }

func redactHeaders(h http.Header) http.Header {
	c := h.Clone()
	if c.Get("Authorization") != "" {
		c.Set("Authorization", "redacted")
	}
	return c
}

func jsonResponse(status int, body string, header http.Header) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	header.Set("Content-Type", "application/json")
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}
}
