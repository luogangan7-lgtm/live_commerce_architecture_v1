// cvs.go assembles the Taiwan CVS surface of taiwan-cvs-logistics-v1 for the API process: the merchant service, the buyer service
// and the two public provider hooks (map return, status). It reads CVS_ECPAY_ENABLED, CVS_ECPAY_LIVE_CREATE, COMMERCE_CVS_HOOKS_ORIGIN
// and ECPAY_LOGISTICS_KEYRING only through the injected getenv, never logs or returns their values, and starts no worker: the River
// client is insert-only (the worker process runs the ecpay.cvs_create route).
//
// Non-goals: no route mounting (main.go / buyer.go call buildCVS and mount the parts: integrator hook), no ECPay call at startup, no
// key generation. Deployment payment environment (unit default C9): COMMERCE_PAYMENT_PROFILE, already validated by the buyer payment
// loader; PROVIDER_MOCK maps to SANDBOX, empty (buyer payment off) means no ECPay path can run.

package main

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"livecommerce/internal/checkout"
	"livecommerce/internal/fulfillment"
	"livecommerce/internal/integrations/shipping/ecpay"
)

var errCVSConfig = errors.New("invalid CVS configuration")

// cvsParts is what buildCVS returns; every field may be nil/empty when the matching surface is not configured.
type cvsParts struct {
	Merchant           *fulfillment.CVS   // merchant routes (httpapi Options.CVS)
	Buyer              *checkout.BuyerCVS // buyer routes and options (checkout.Service.WithBuyerCVS)
	Hooks              http.Handler       // /v1/cvs/ecpay/ (404 handler when CVS_ECPAY_ENABLED is off)
	PaymentEnvironment string             // "SANDBOX" | "LIVE" | "" for checkout.Service.WithPaymentEnvironment
}

// cvsPaymentEnvironment maps COMMERCE_PAYMENT_PROFILE to the ECPay environment a deployment may hold (R2-3, stripe-live-enable LD1/LQ5).
func cvsPaymentEnvironment(profile string) (string, error) {
	switch profile {
	case "":
		return "", nil
	case "PROVIDER_MOCK", "SANDBOX":
		return "SANDBOX", nil
	case "LIVE":
		return "LIVE", nil
	}
	return "", errCVSConfig
}

func buildCVS(ctx context.Context, getenv func(string) string, pool, checkoutPool *pgxpool.Pool, paymentProfile string) (cvsParts, error) {
	if ctx == nil || getenv == nil || pool == nil {
		return cvsParts{}, errCVSConfig
	}
	env, err := cvsPaymentEnvironment(paymentProfile)
	if err != nil {
		return cvsParts{}, err
	}
	// ecpay.LoadConfig: Enabled with a non-https-origin hooks URL is an error; nothing else is validated here.
	ecfg, err := ecpay.LoadConfig(getenv)
	if err != nil {
		return cvsParts{}, errCVSConfig
	}
	cfg := fulfillment.CVSConfig{ECPay: ecfg, PaymentEnvironment: env}
	var keys *ecpay.Keyring
	var client *ecpay.Client
	if ecfg.Enabled {
		// CVS_ECPAY_ENABLED=1 needs buyer payment on: the payment environment is the source of the ECPay environment pin (C9).
		if env == "" {
			return cvsParts{}, errCVSConfig
		}
		if keys, err = ecpay.LoadKeyring(getenv); err != nil {
			return cvsParts{}, errCVSConfig
		}
		if client, err = ecpay.NewClient(ecpay.Environment(env), nil); err != nil {
			return cvsParts{}, errCVSConfig
		}
	}
	// The runtime pool is used solely by River's transactional insert path (external_operation_v1 job); no queue or worker starts here.
	jobs, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: "river"})
	if err != nil {
		return cvsParts{}, errCVSConfig
	}
	merchant, err := fulfillment.NewCVS(pool, jobs, keys, client, cfg)
	if err != nil {
		return cvsParts{}, errCVSConfig
	}
	parts := cvsParts{Merchant: merchant, Hooks: merchant.HooksHandler(), PaymentEnvironment: env}
	if checkoutPool != nil {
		if parts.Buyer, err = checkout.NewBuyerCVS(checkoutPool, keys, client, cfg); err != nil {
			return cvsParts{}, errCVSConfig
		}
	}
	return parts, nil
}
