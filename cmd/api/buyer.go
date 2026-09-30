package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"livecommerce/internal/attribution"
	"livecommerce/internal/buyerhttp"
	"livecommerce/internal/checkout"
	"livecommerce/internal/httperror"
	"livecommerce/internal/identityhttp"
	"livecommerce/internal/platform"
)

var errBuyerConfig = errors.New("invalid buyer configuration")

// Intercept buyer paths and their cleaning aliases before ServeMux. The original
// path is passed unchanged, so the strict buyer router rejects aliases instead
// of redirecting a credential-bearing request to another URL.
func mountBuyer(fallback, buyerHandler http.Handler) http.Handler {
	if buyerHandler == nil {
		return fallback
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/buyer") || strings.HasPrefix(path.Clean(r.URL.Path), "/v1/buyer") {
			buyerHandler.ServeHTTP(w, r)
			return
		}
		fallback.ServeHTTP(w, r)
	})
}

type buyerConfig struct {
	enabled                          bool
	issuerDSN, buyerDSN, checkoutDSN string
	bffKey                           string
	ttl                              time.Duration
	payment                          buyerPaymentConfig
}

// The buyer capability is distinct from a merchant login. Disabled mode must
// not read its DSNs or credentials, and enabling it never exposes the Go port.
func loadBuyerConfig(getenv func(string) string, addr string) (buyerConfig, error) {
	var c buyerConfig
	enabled, err := flag(getenv("COMMERCE_BUYER_ENABLED"))
	if err != nil {
		return c, errBuyerConfig
	}
	if !enabled {
		return c, nil
	}
	if !privateIdentityAddress(addr) {
		return c, errBuyerConfig
	}
	c.issuerDSN = getenv("COMMERCE_BUYER_ISSUER_DATABASE_URL")
	c.buyerDSN = getenv("COMMERCE_BUYER_DATABASE_URL")
	c.checkoutDSN = getenv("COMMERCE_CHECKOUT_DATABASE_URL")
	c.bffKey = getenv("COMMERCE_BUYER_BFF_KEY")
	c.ttl, err = time.ParseDuration(getenv("COMMERCE_BUYER_SESSION_TTL"))
	if err != nil || c.ttl < time.Minute || c.ttl > 30*24*time.Hour || c.ttl%time.Second != 0 ||
		strings.TrimSpace(c.issuerDSN) == "" || strings.TrimSpace(c.buyerDSN) == "" || strings.TrimSpace(c.checkoutDSN) == "" ||
		!identityhttp.ValidSecret(c.bffKey) || c.bffKey == getenv("COMMERCE_BFF_KEY") {
		return buyerConfig{}, errBuyerConfig
	}
	c.enabled = true
	c.payment, err = loadBuyerPaymentConfig(getenv, addr)
	if err != nil {
		return buyerConfig{}, errBuyerConfig
	}
	return c, nil
}

func buildBuyerHandler(ctx context.Context, c buyerConfig) (http.Handler, func(), error) {
	h, _, closePools, err := buildBuyerWithCVS(ctx, c, nil, os.Getenv)
	return h, closePools, err
}

// buildBuyerWithCVS is buildBuyerHandler plus the taiwan-cvs-logistics-v1 surface: with the main (merchant) pool it builds the CVS
// parts once, so the merchant service, the public hooks and the buyer service share one ECPay client and directory cache, and
// the checkout service learns the deployment payment environment and the buyer CVS surface. mainPool nil = no CVS (unit tests).
func buildBuyerWithCVS(ctx context.Context, c buyerConfig, mainPool *pgxpool.Pool, getenv func(string) string) (http.Handler, cvsParts, func(), error) {
	if !c.enabled {
		return nil, cvsParts{}, func() {}, nil
	}
	issuer, err := platform.OpenBuyerIssuerPool(ctx, c.issuerDSN)
	if err != nil {
		return nil, cvsParts{}, nil, errBuyerConfig
	}
	runtime, err := platform.OpenBuyerPool(ctx, c.buyerDSN)
	if err != nil {
		issuer.Close()
		return nil, cvsParts{}, nil, errBuyerConfig
	}
	checkoutPool, err := platform.OpenCheckoutPool(ctx, c.checkoutDSN)
	if err != nil {
		runtime.Close()
		issuer.Close()
		return nil, cvsParts{}, nil, errBuyerConfig
	}
	var hostedPool *pgxpool.Pool
	closePools := func() {
		if hostedPool != nil {
			hostedPool.Close()
		}
		checkoutPool.Close()
		runtime.Close()
		issuer.Close()
	}
	// River is used only to insert the expiry task in Begin's transaction. API
	// startup does not start workers or gain provider dispatch authority. The
	// expiry schema keeps River maintenance separate from payment and external jobs.
	jobs, err := river.NewClient(riverpgxv5.New(checkoutPool), &river.Config{Schema: "river_expiry"})
	if err != nil {
		closePools()
		return nil, cvsParts{}, nil, errBuyerConfig
	}
	service, err := checkout.New(ctx, checkoutPool, jobs)
	if err != nil {
		closePools()
		return nil, cvsParts{}, nil, errBuyerConfig
	}
	var parts cvsParts
	if mainPool != nil {
		if parts, err = buildCVS(ctx, getenv, mainPool, checkoutPool, c.payment.profile); err != nil {
			closePools()
			return nil, cvsParts{}, nil, errBuyerConfig
		}
		service = service.WithPaymentEnvironment(parts.PaymentEnvironment).WithBuyerCVS(parts.Buyer)
	}
	payment, openedHostedPool, err := buildBuyerPayment(ctx, c.payment)
	if err != nil {
		closePools()
		return nil, cvsParts{}, nil, errBuyerConfig
	}
	hostedPool = openedHostedPool
	h, err := buyerhttp.New(ctx, issuer, runtime, service, c.bffKey, c.ttl, payment)
	if err != nil {
		closePools()
		return nil, cvsParts{}, nil, errBuyerConfig
	}
	// ads-capi C6: the public Meta product feed, beside the buyer router, on the buyer runtime pool.
	return mountFeed(h, httperror.Middleware(attribution.FeedHandler(runtime)), c.bffKey), parts, closePools, nil
}

// feedPath is the one route mountFeed serves (meta-ads-v1 §7; the storefront app proxies /feeds/meta.csv to it).
const feedPath = "/v1/buyer/feeds/meta.csv"

// mountFeed serves feedPath from feed and everything else from next. The feed data is public, but the route keeps the
// buyer API's rule that only the storefront BFF (which derives the origin from the verified Host) may call it: a wrong or
// missing X-Commerce-Buyer-BFF-Key is 401 before any SQL, compared in constant time.
func mountFeed(next, feed http.Handler, bffKey string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != feedPath {
			next.ServeHTTP(w, r)
			return
		}
		if keys := r.Header.Values("X-Commerce-Buyer-BFF-Key"); len(keys) != 1 || subtle.ConstantTimeCompare([]byte(keys[0]), []byte(bffKey)) != 1 {
			httperror.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				httperror.Write(w, http.StatusUnauthorized, "unauthorized")
			})).ServeHTTP(w, r)
			return
		}
		feed.ServeHTTP(w, r)
	})
}
