package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func cvsEnv(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestCVSPaymentEnvironmentMapping(t *testing.T) {
	for profile, want := range map[string]string{"": "", "PROVIDER_MOCK": "SANDBOX", "SANDBOX": "SANDBOX", "LIVE": "LIVE"} {
		if got, err := cvsPaymentEnvironment(profile); err != nil || got != want {
			t.Errorf("%q -> %q %v, want %q", profile, got, err, want)
		}
	}
	for _, bad := range []string{"live", "PRODUCTION", "STAGING", " LIVE"} {
		if _, err := cvsPaymentEnvironment(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestBuildCVSConfigRules(t *testing.T) {
	ctx, pool := context.Background(), &pgxpool.Pool{}
	// Disabled: ECPay off, hooks are a 404, the merchant service still exists for settings/collection/release.
	parts, err := buildCVS(ctx, cvsEnv(nil), pool, nil, "LIVE")
	if err != nil || parts.Merchant == nil || parts.Hooks == nil || parts.Buyer != nil || parts.PaymentEnvironment != "LIVE" {
		t.Fatalf("disabled: %+v %v", parts, err)
	}
	rec := httptest.NewRecorder()
	parts.Hooks.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/cvs/ecpay/status/33333333-3333-4333-8333-333333333333", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("hooks with ECPay off: %d", rec.Code)
	}
	parts, err = buildCVS(ctx, cvsEnv(nil), pool, nil, "")
	if err != nil || parts.PaymentEnvironment != "" {
		t.Fatalf("buyer payment off, ECPay off must be fine: %+v %v", parts, err)
	}
	enabled := map[string]string{"CVS_ECPAY_ENABLED": "1", "COMMERCE_CVS_HOOKS_ORIGIN": "https://hooks.example.test"}
	// C9: ECPay on with buyer payment off has no environment to pin.
	if _, err = buildCVS(ctx, cvsEnv(enabled), pool, nil, ""); err == nil {
		t.Fatal("CVS_ECPAY_ENABLED=1 with COMMERCE_PAYMENT_PROFILE unset accepted")
	}
	// ECPay on but no keyring in the environment.
	if _, err = buildCVS(ctx, cvsEnv(enabled), pool, nil, "SANDBOX"); err == nil {
		t.Fatal("CVS_ECPAY_ENABLED=1 without ECPAY_LOGISTICS_KEYRING accepted")
	}
	// A non-https hooks origin never starts.
	if _, err = buildCVS(ctx, cvsEnv(map[string]string{"CVS_ECPAY_ENABLED": "1", "COMMERCE_CVS_HOOKS_ORIGIN": "http://hooks.example.test"}), pool, nil, "SANDBOX"); err == nil {
		t.Fatal("plain-http hooks origin accepted")
	}
	if _, err = buildCVS(ctx, cvsEnv(nil), pool, nil, "BOGUS"); err == nil {
		t.Fatal("unknown payment profile accepted")
	}
	if _, err = buildCVS(ctx, cvsEnv(nil), nil, nil, "LIVE"); err == nil {
		t.Fatal("nil merchant pool accepted")
	}
}
