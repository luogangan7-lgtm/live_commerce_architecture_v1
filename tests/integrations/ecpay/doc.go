// Package ecpay_test holds the independent black-box gates of internal/integrations/shipping/ecpay (files *_test.go, package
// ecpay_test): TCV01 (MAC, recipient and store-code golden tables, trade number), TCV07/TCV10 (SANDBOX, build tag sandbox).
// It owns no production code.
//
// It never contacts ECPay without ECPAY_LOGISTICS_SANDBOX=1 and stage keys in the environment (~/.config/livecommerce/secrets.env),
// never stores keys, and reports NOT_RUN (t.Skip) rather than PASS when they are absent. The golden tables under testdata/ are
// shared with the SQL legs in tests/foundation (TCV14/TCV15).
package ecpay_test
