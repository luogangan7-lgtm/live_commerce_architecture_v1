// Package httpapi owns the composition layer for authenticated merchant/admin routes: routing,
// bearer resolution, request bounds and error mapping. Domains do not import it; they receive only
// the transaction and resolved Scope. It never implements a domain rule, never opens a pool of its
// own, and never trusts a tenant or store id from a request body.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"livecommerce/internal/ads"
	"livecommerce/internal/billing"
	"livecommerce/internal/catalog"
	"livecommerce/internal/claims"
	"livecommerce/internal/command"
	"livecommerce/internal/fulfillment"
	"livecommerce/internal/httperror"
	"livecommerce/internal/integrations/accounts"
	"livecommerce/internal/inventory"
	"livecommerce/internal/live"
	"livecommerce/internal/merchantorders"
	"livecommerce/internal/pagination"
	"livecommerce/internal/platform"
)

// NewHandler keeps transport validation separate from domain invariants. There
// is deliberately no public Reserve route: only a validated BeginCheckout may
// eventually call it, never a GET, comment event, or client-selected tenant.
type Options struct {
	SessionStoreList bool
	Accounts         *accounts.Service
	// Studio mounts the live-session planning routes (studio-v1 GET/POST/GET/PATCH) without any media
	// subsystem (R1 ruling G2). Live non-nil implies Studio and adds the MOCK rehearsal routes;
	// BrowserInput additionally adds the input routes.
	Studio       bool
	Live         *live.MediaPlanner
	BrowserInput *live.BrowserInputRuntime
	// ClaimLabels is the server-held manual-label HMAC key (cmd/api loads
	// COMMERCE_CLAIMS_LABEL_KEY). nil leaves the keyword-claims routes unmounted.
	ClaimLabels *claims.LabelKey
	// RefundJobs is the insert-only river_payment client (cmd/api newMerchantRefundJobs). nil leaves the
	// stripe-refund-v1 §7.1 refund routes unmounted.
	RefundJobs *river.Client[pgx.Tx]
	// Ads is the meta-ads-v1 merchant service (cmd/api builds it with the insert-only river client, the FLfB dialog
	// config and the metaads OAuth exchange). nil leaves the ads routes unmounted; mount only after 0080 (contract 4.3).
	Ads *ads.Service
	// Billing is the platform-fee service (cmd/api buildPlatformBilling). nil (LC_BILLING_ENABLED unset)
	// still mounts the billing GET routes; the POSTs answer 503 billing_unavailable.
	Billing *billing.Service
	// CVS mounts the taiwan-cvs-logistics-v1 merchant routes (§8: ECPay connection, settings, label request, print, abandon,
	// collection, pay-at-pickup release). nil leaves them unmounted (cmd/api buildCVS).
	CVS *fulfillment.CVS
	// PaymentEnvironment is the deployment's payment environment, SANDBOX or LIVE (payments.ProfileEnvironment of
	// COMMERCE_PAYMENT_PROFILE, chosen by cmd/api). The refund POST refuses an attempt of another environment
	// (stripe-live-enable-v1 §5.2, S5). Empty means SANDBOX so pre-LIVE callers keep their behavior; any other
	// value not in {SANDBOX, LIVE} leaves the refund routes unmounted.
	PaymentEnvironment string
}

func NewHandler(pool *pgxpool.Pool, options ...Options) http.Handler {
	mux := http.NewServeMux()
	var configured Options
	if len(options) > 0 {
		configured = options[0]
	}
	const base = "/v1/admin/stores/{store_id}"
	mux.HandleFunc("GET "+base+"/catalog-ledger", scoped(pool, "catalog:read", func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request) (any, error) {
		if err := platform.RequirePermission(ctx, tx, s, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "inventory:read"); err != nil {
			return nil, err
		}
		if len(r.URL.RawQuery) > 4096 {
			return nil, command.ErrInvalid
		}
		values, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			return nil, command.ErrInvalid
		}
		for _, v := range values {
			if len(v) != 1 {
				return nil, command.ErrInvalid
			}
		}
		in := catalog.LedgerRequest{WarehouseID: values.Get("warehouse_id"), Query: values.Get("q"), Status: values.Get("status")}
		values.Del("warehouse_id")
		values.Del("q")
		values.Del("status")
		in.Page, err = parsePage(values.Encode())
		if err != nil {
			return nil, err
		}
		return catalog.ListLedger(ctx, tx, s, in)
	}))
	mux.HandleFunc("GET "+base+"/products", listRoute(pool, "catalog:read", func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request, page pagination.Request) (any, error) {
		return catalog.ListProductsPage(ctx, tx, s, page)
	}))
	mux.HandleFunc("GET "+base+"/products/{product_id}/purchase-entry", purchaseEntryRoute(pool))
	mux.HandleFunc("POST "+base+"/products", bodyRoute(pool, "catalog:write", func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request, in catalog.ProductInput) (any, error) {
		return catalog.CreateProduct(ctx, tx, s, r.Header.Get("Idempotency-Key"), in)
	}))
	mux.HandleFunc("PATCH "+base+"/products/{product_id}", bodyRoute(pool, "catalog:write", func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request, in catalog.ProductInput) (any, error) {
		return catalog.UpdateProduct(ctx, tx, s, r.Header.Get("Idempotency-Key"), r.PathValue("product_id"), in)
	}))
	mux.HandleFunc("POST "+base+"/products/{product_id}/archive", bodyRoute(pool, "catalog:write", func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request, in versionInput) (any, error) {
		return catalog.ArchiveProduct(ctx, tx, s, r.Header.Get("Idempotency-Key"), r.PathValue("product_id"), in.ExpectedVersion)
	}))
	mux.HandleFunc("GET "+base+"/products/{product_id}/skus", listRoute(pool, "catalog:read", func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request, page pagination.Request) (any, error) {
		return catalog.ListSKUsPage(ctx, tx, s, r.PathValue("product_id"), page)
	}))
	mux.HandleFunc("POST "+base+"/skus", bodyRoute(pool, "catalog:write", func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request, in catalog.SKUInput) (any, error) {
		return catalog.CreateSKU(ctx, tx, s, r.Header.Get("Idempotency-Key"), in)
	}))
	mux.HandleFunc("PATCH "+base+"/skus/{sku_id}", bodyRoute(pool, "catalog:write", func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request, in catalog.SKUInput) (any, error) {
		return catalog.UpdateSKU(ctx, tx, s, r.Header.Get("Idempotency-Key"), r.PathValue("sku_id"), in)
	}))
	mux.HandleFunc("POST "+base+"/skus/{sku_id}/price", bodyRoute(pool, "catalog:write", func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request, in catalog.PriceInput) (any, error) {
		return catalog.SetSKUPrice(ctx, tx, s, r.Header.Get("Idempotency-Key"), r.PathValue("sku_id"), in)
	}))
	mux.HandleFunc("POST "+base+"/skus/{sku_id}/archive", bodyRoute(pool, "catalog:write", func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request, in versionInput) (any, error) {
		return catalog.ArchiveSKU(ctx, tx, s, r.Header.Get("Idempotency-Key"), r.PathValue("sku_id"), in.ExpectedVersion)
	}))
	mux.HandleFunc("GET "+base+"/warehouses", listRoute(pool, "inventory:read", func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request, page pagination.Request) (any, error) {
		return inventory.ListWarehousesPage(ctx, tx, s, page)
	}))
	mux.HandleFunc("POST "+base+"/warehouses", bodyRoute(pool, "inventory:write", func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request, in warehouseInput) (any, error) {
		return inventory.CreateWarehouse(ctx, tx, s, r.Header.Get("Idempotency-Key"), in.Name)
	}))
	mux.HandleFunc("GET "+base+"/inventory", listRoute(pool, "inventory:read", func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request, page pagination.Request) (any, error) {
		return inventory.ListBalancesPage(ctx, tx, s, page)
	}))
	mux.HandleFunc("POST "+base+"/inventory/adjustments", bodyRoute(pool, "inventory:write", func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request, in inventory.Adjustment) (any, error) {
		return inventory.AdjustOnHand(ctx, tx, s, r.Header.Get("Idempotency-Key"), in)
	}))
	registerSettingsRoutes(mux, pool)
	registerSettingsDiscoveryRoutes(mux, pool)
	registerAccountRoutes(mux, pool, configured.Accounts)
	registerOrderRoutes(mux, pool)
	registerStudioRoutes(mux, pool, configured.Studio || configured.Live != nil, configured.Live, configured.BrowserInput)
	registerClaimRoutes(mux, pool, configured.ClaimLabels)
	paymentEnvironment := configured.PaymentEnvironment
	if paymentEnvironment == "" {
		paymentEnvironment = "SANDBOX"
	}
	registerRefundRoutesIn(mux, pool, configured.RefundJobs, paymentEnvironment)
	registerShipmentRoutes(mux, pool)
	registerAdsRoutes(mux, pool, configured.Ads)
	registerCustomerRoutes(mux, pool)
	registerFinanceRoutes(mux, pool)
	registerBillingRoutes(mux, pool, configured.Billing)
	registerCVSRoutes(mux, pool, configured.CVS)
	foundation := platform.NewHandler(pool, platform.HandlerOptions{SessionStoreList: configured.SessionStoreList})
	if configured.SessionStoreList {
		mux.Handle("GET /v1/admin/stores", foundation)
	}
	for _, pattern := range []string{"GET /healthz", "GET /readyz", "GET " + base, "GET " + base + "/audit-events"} {
		mux.Handle(pattern, foundation)
	}
	return httperror.Middleware(mux)
}

type versionInput struct {
	ExpectedVersion int64 `json:"expected_version"`
}
type warehouseInput struct {
	Name string `json:"name"`
}
type action func(context.Context, pgx.Tx, platform.Scope, *http.Request) (any, error)

func purchaseEntryRoute(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Go's GET ServeMux pattern also matches HEAD; this projection does not.
		if r.Method != http.MethodGet {
			respondError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		if _, present := r.Header[http.CanonicalHeaderKey("Idempotency-Key")]; present || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
			respondError(w, http.StatusUnprocessableEntity, "invalid_request")
			return
		}
		locale, err := purchaseEntryLocale(r.URL)
		if err != nil {
			respondError(w, http.StatusUnprocessableEntity, "invalid_request")
			return
		}
		scoped(pool, "catalog:read", func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request) (any, error) {
			return catalog.ReadPurchaseEntry(ctx, tx, s, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), r.PathValue("product_id"), locale)
		})(w, r)
	}
}

func purchaseEntryLocale(u *url.URL) (string, error) {
	if u.ForceQuery || u.RawQuery == "" || len(u.RawQuery) > 64 {
		return "", command.ErrInvalid
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(values) != 1 || len(values["locale"]) != 1 {
		return "", command.ErrInvalid
	}
	locale := values["locale"][0]
	if locale != "zh-CN" && locale != "zh-TW" && locale != "en" {
		return "", command.ErrInvalid
	}
	return locale, nil
}

func listRoute(pool *pgxpool.Pool, permission string, fn func(context.Context, pgx.Tx, platform.Scope, *http.Request, pagination.Request) (any, error)) http.HandlerFunc {
	return scoped(pool, permission, func(ctx context.Context, tx pgx.Tx, scope platform.Scope, r *http.Request) (any, error) {
		page, err := parsePage(r.URL.RawQuery)
		if err != nil {
			return nil, err
		}
		return fn(ctx, tx, scope, r, page)
	})
}

func parsePage(raw string) (pagination.Request, error) {
	var page pagination.Request
	if len(raw) > 4096 {
		return page, command.ErrInvalid
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return page, command.ErrInvalid
	}
	for key, list := range values {
		if len(list) != 1 {
			return page, command.ErrInvalid
		}
		switch key {
		case "cursor":
			if len(list[0]) > 1024 {
				return page, command.ErrInvalid
			}
			page.Cursor = list[0]
		case "limit":
			if list[0] == "" || strings.Trim(list[0], "0123456789") != "" {
				return page, command.ErrInvalid
			}
			page.Limit, err = strconv.Atoi(list[0])
			if err != nil || page.Limit < 1 || page.Limit > 100 {
				return page, command.ErrInvalid
			}
		default:
			return page, command.ErrInvalid
		}
	}
	return page, nil
}

// bodyRoute rejects unknown fields/trailing values and caps allocation before
// opening a database transaction. It never logs bodies or bearer credentials.
func bodyRoute[T any](pool *pgxpool.Pool, permission string, fn func(context.Context, pgx.Tx, platform.Scope, *http.Request, T) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/json" {
			respondError(w, http.StatusUnsupportedMediaType, "json_required")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		defer r.Body.Close()
		var in T
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err = dec.Decode(&in); err != nil {
			respondError(w, http.StatusBadRequest, "invalid_json")
			return
		}
		var extra any
		if err = dec.Decode(&extra); err != io.EOF {
			respondError(w, http.StatusBadRequest, "invalid_json")
			return
		}
		scoped(pool, permission, func(ctx context.Context, tx pgx.Tx, s platform.Scope, r *http.Request) (any, error) {
			return fn(ctx, tx, s, r, in)
		})(w, r)
	}
}

func scoped(pool *pgxpool.Pool, permission string, fn action) http.HandlerFunc {
	return scopedAs(pool, permission, classify, fn)
}

// scopedAs is scoped with a route family's own error classifier (claims.go maps
// deadlocks and unknown database errors to 503 per its frozen contract).
func scopedAs(pool *pgxpool.Pool, permission string, classifier func(error) (int, string), fn action) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") || strings.ContainsAny(strings.TrimPrefix(header, "Bearer "), " \t\r\n") {
			respondError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		var result any
		err := platform.WithScope(ctx, pool, strings.TrimPrefix(header, "Bearer "), r.PathValue("store_id"), permission, func(tx pgx.Tx, s platform.Scope) error {
			var inner error
			result, inner = fn(ctx, tx, s, r)
			return inner
		})
		if err != nil {
			if errors.Is(err, errAccountRateLimited) {
				w.Header().Set("Retry-After", "60")
			}
			status, code := classifier(err)
			respondError(w, status, code)
			return
		}
		respond(w, http.StatusOK, result)
	}
}

// Map only stable classes. Raw pgconn messages can include customer values.
func classify(err error) (int, string) {
	switch {
	case errors.Is(err, errAccountRateLimited):
		return http.StatusTooManyRequests, "rate_limited"
	case errors.Is(err, errAccountCapacity), errors.Is(err, errAccountUnavailable):
		return http.StatusServiceUnavailable, "unavailable"
	case errors.Is(err, catalog.ErrPurchaseEntryUnavailable):
		return http.StatusServiceUnavailable, "unavailable"
	case errors.Is(err, merchantorders.ErrUnavailable):
		return http.StatusServiceUnavailable, "unavailable"
	case errors.Is(err, live.ErrStudioProjection):
		return http.StatusServiceUnavailable, "unavailable"
	case errors.Is(err, platform.ErrScopeNotFound):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, platform.ErrForbidden):
		return http.StatusForbidden, "forbidden"
	case errors.Is(err, platform.ErrUnauthorized):
		return http.StatusUnauthorized, "unauthorized"
	case errors.Is(err, command.ErrInvalid):
		return http.StatusUnprocessableEntity, "invalid_request"
	case errors.Is(err, command.ErrConflict):
		return http.StatusConflict, "conflict"
	case errors.Is(err, command.ErrNotFound), errors.Is(err, pgx.ErrNoRows):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, command.ErrInsufficient):
		return http.StatusConflict, "insufficient_inventory"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return http.StatusServiceUnavailable, "retry_later"
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23505", "40001", "40P01":
			return http.StatusConflict, "conflict"
		case "23503":
			return http.StatusNotFound, "not_found"
		case "23514", "22003", "22P02":
			return http.StatusUnprocessableEntity, "invalid_request"
		case "55P03", "57014":
			return http.StatusServiceUnavailable, "retry_later"
		}
	}
	return http.StatusInternalServerError, "internal"
}

func respondError(w http.ResponseWriter, status int, code string) {
	httperror.Write(w, status, code)
}
func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
