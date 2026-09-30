// helpers_test.go: shared fixtures for the stripe package unit tests (SP01–SP05, SP19).
// Non-goal: no network. Every key and secret here is a fake sentinel; any appearance of
// these strings in formatted output is a redaction failure.
// Callers: the *_test.go files in this package.

package stripe

import (
	"net/http"
	"strings"
)

const (
	fakeTestKey    = "sk_" + "test_FAKESENTINELKEY0000000000" // split so secret scanners see no key literal
	fakeRAKTestKey = "rk_" + "test_FAKESENTINELKEY0000000000"
	fakeLiveKey    = "sk_" + "live_FAKESENTINELKEY0000000000"
	fakeRAKLiveKey = "rk_" + "live_FAKESENTINELKEY0000000000" // LD3: the only live key shape LIVE admits
	fakeAccount    = "acct_1FakeAccount000"
	fakeWhsecA     = "whsec" + "_vectorSecretA_not_real_0123456789" // split: not a real secret
	fakeWhsecB     = "whsec_" + "vectorSecretB_not_real_9876543210"
	fxAttempt      = "0b5e3c1a-7d2f-4e8a-9c61-2f4d8e7a1b30"
	fxOrder        = "9f1c2e4d-5a6b-4c7d-8e9f-0a1b2c3d4e5f"
	fxReturnURL    = "https://shop.example.test/payment/return"
	fxSessionURL   = "https://checkout.stripe.com/c/pay/cs_test_SENTINELURL"
)

func sandboxConfig() Config {
	return Config{SecretKey: fakeTestKey, AccountID: fakeAccount, Environment: "SANDBOX"}
}

// fixtureFields is the complete §5.4 key set for the fixture attempt.
func fixtureFields() map[string]string {
	return map[string]string{
		"adaptive_pricing[enabled]":                     "false",
		"managed_payments[enabled]":                     "false",
		"automatic_tax[enabled]":                        "false",
		"cancel_url":                                    fxReturnURL,
		"success_url":                                   fxReturnURL,
		"client_reference_id":                           fxAttempt,
		"expires_at":                                    "1790000000",
		"line_items[0][price_data][currency]":           "hkd",
		"line_items[0][price_data][product_data][name]": "Order 9F1C2E4D",
		"line_items[0][price_data][unit_amount]":        "12345",
		"line_items[0][quantity]":                       "1",
		"locale":                                        "zh-TW",
		"metadata[lc_attempt]":                          fxAttempt,
		"metadata[lc_order]":                            fxOrder,
		"metadata[lc_profile]":                          "SANDBOX",
		"metadata[lc_v]":                                "1",
		"mode":                                          "payment",
		"payment_intent_data[metadata][lc_attempt]":     fxAttempt,
		"payment_intent_data[metadata][lc_order]":       fxOrder,
		"payment_method_types[0]":                       "card",
		"submit_type":                                   "pay",
		"ui_mode":                                       "hosted_page",
	}
}

func fixtureParams() CreateParams { return CreateParams{Fields: fixtureFields()} }

// sessionJSON is a minimal valid checkout.session body for the fixture attempt.
func sessionJSON(id, status, payStatus string, livemode bool) string {
	live := "false"
	if livemode {
		live = "true"
	}
	return `{"id":"` + id + `","object":"checkout.session","livemode":` + live +
		`,"status":"` + status + `","payment_status":"` + payStatus + `","currency":"hkd",` +
		`"amount_total":12345,"amount_subtotal":12345,"total_details":{"amount_discount":0,"amount_tax":0,"amount_shipping":0},` +
		`"presentment_details":null,"currency_conversion":null,"client_reference_id":"` + fxAttempt + `",` +
		`"metadata":{"lc_attempt":"` + fxAttempt + `","lc_order":"` + fxOrder + `","lc_profile":"SANDBOX","lc_v":"1"},` +
		`"expires_at":1790000000,"created":1789997600,"mode":"payment","payment_method_types":["card"],` +
		`"payment_intent":null,"customer_details":{"email":"buyer@example.test"},"url":"` + fxSessionURL + `"}`
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// containsAny reports whether s leaks any of the sentinels.
func containsAny(s string, sentinels ...string) bool {
	for _, x := range sentinels {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}
