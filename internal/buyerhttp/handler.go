// Package buyerhttp owns the private, BFF-only buyer transport (catalog, cart, quote, checkout,
// payment and claim routes). It borrows its database pools; publication and buyer authority are
// resolved per request. It never serves the public internet directly, never trusts Host or a tenant
// id from the client, and holds no pricing or stock rule: it maps HTTP onto the storefront, checkout
// and claims packages.
package buyerhttp

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"livecommerce/internal/buyer"
	"livecommerce/internal/checkout"
	"livecommerce/internal/command"
	"livecommerce/internal/domains"
	"livecommerce/internal/httperror"
	"livecommerce/internal/pagination"
	"livecommerce/internal/platform"
	"livecommerce/internal/storefront"
)

const maxJSON = 64 << 10

var idempotencyKey = regexp.MustCompile(`^[A-Za-z0-9_.:-]{8,128}$`)

type handler struct {
	resolver *domains.Resolver
	issuer   *buyer.Service
	pool     *pgxpool.Pool
	checkout *checkout.Service
	payment  *checkout.HostedPaymentStarter
	bffKey   string
}

// New validates both borrowed authorities and builds the private transport.
// The caller retains ownership of every pool and the checkout service.
func New(ctx context.Context, issuerPool, buyerPool *pgxpool.Pool, checkoutService *checkout.Service, bffKey string, ttl time.Duration,
	payment ...*checkout.HostedPaymentStarter) (http.Handler, error) {
	if ctx == nil || checkoutService == nil || !canonicalSecret(bffKey) || len(payment) > 1 {
		return nil, command.ErrInvalid
	}
	var hosted *checkout.HostedPaymentStarter
	if len(payment) == 1 {
		hosted = payment[0]
	}
	issuer, err := buyer.New(issuerPool, ttl)
	if err != nil {
		return nil, err
	}
	resolver, err := domains.New(ctx, issuerPool) // validates issuer authority
	if err != nil {
		return nil, err
	}
	if err = platform.ValidateBuyerPool(ctx, buyerPool); err != nil {
		return nil, err
	}
	return httperror.Middleware(&handler{resolver: resolver, issuer: issuer, pool: buyerPool,
		checkout: checkoutService, payment: hosted, bffKey: bffKey}), nil
}

type routeKind uint8

const (
	unknownRoute routeKind = iota
	sessionRoute
	bootstrapRoute
	retireRoute
	catalogRoute
	optionsRoute
	cartRoute
	quotesRoute
	quoteRoute
	destinationRoute
	destinationItemRoute
	checkoutRoute
	ordersRoute
	orderRoute
	paymentRoute
	paymentPrepareRoute
	paymentHandoffRoute
	paymentRefreshRoute
	paymentCancelRoute
	claimLinkRoute
	claimRedeemRoute
	privacyRoute
	consentsRoute
	privacyExportRoute
	privacyErasureRoute
)

type route struct {
	kind routeKind
	id   string
}

func matchRoute(path string) route {
	switch path {
	case "/v1/buyer/session":
		return route{kind: sessionRoute}
	case "/v1/buyer/session/bootstrap":
		return route{kind: bootstrapRoute}
	case "/v1/buyer/session/retire":
		return route{kind: retireRoute}
	case "/v1/buyer/catalog":
		return route{kind: catalogRoute}
	case "/v1/buyer/checkout-options":
		return route{kind: optionsRoute}
	case "/v1/buyer/cart":
		return route{kind: cartRoute}
	case "/v1/buyer/quotes":
		return route{kind: quotesRoute}
	case "/v1/buyer/destination":
		return route{kind: destinationRoute}
	case "/v1/buyer/checkout":
		return route{kind: checkoutRoute}
	case "/v1/buyer/orders":
		return route{kind: ordersRoute}
	case claimLinkPath:
		return route{kind: claimLinkRoute}
	case claimRedeemPath:
		return route{kind: claimRedeemRoute}
	case privacyPath:
		return route{kind: privacyRoute}
	case consentsPath:
		return route{kind: consentsRoute}
	case privacyExportPath:
		return route{kind: privacyExportRoute}
	case privacyErasurePath:
		return route{kind: privacyErasureRoute}
	}
	if cvs := matchCVSRoute(path); cvs.kind != unknownRoute {
		return cvs
	}
	if rest, ok := strings.CutPrefix(path, "/v1/buyer/orders/"); ok {
		for _, entry := range []struct {
			suffix string
			kind   routeKind
		}{{"/payment/prepare", paymentPrepareRoute}, {"/payment/handoff", paymentHandoffRoute},
			{"/payment/refresh", paymentRefreshRoute}, {"/payment/cancel", paymentCancelRoute}, {"/payment", paymentRoute}} {
			if id, matched := strings.CutSuffix(rest, entry.suffix); matched && id != "" && !strings.Contains(id, "/") {
				return route{kind: entry.kind, id: id}
			}
		}
	}
	for _, entry := range []struct {
		prefix string
		kind   routeKind
	}{{"/v1/buyer/quotes/", quoteRoute}, {"/v1/buyer/destinations/", destinationItemRoute}, {"/v1/buyer/orders/", orderRoute}} {
		if id, ok := strings.CutPrefix(path, entry.prefix); ok && id != "" && !strings.Contains(id, "/") {
			return route{kind: entry.kind, id: id}
		}
	}
	return route{}
}

func allowed(kind routeKind, method string) bool {
	if isCVSRoute(kind) {
		return allowedCVS(kind, method)
	}
	switch kind {
	case sessionRoute:
		return method == http.MethodGet || method == http.MethodPost || method == http.MethodDelete
	case bootstrapRoute, retireRoute:
		return method == http.MethodPost
	case catalogRoute, optionsRoute, ordersRoute, paymentRoute:
		return method == http.MethodGet
	case cartRoute:
		return method == http.MethodGet || method == http.MethodPut
	case quotesRoute, checkoutRoute:
		return method == http.MethodPost
	case paymentPrepareRoute, paymentHandoffRoute, paymentRefreshRoute, paymentCancelRoute:
		return method == http.MethodPost
	case destinationRoute:
		return method == http.MethodGet || method == http.MethodPut
	case quoteRoute, destinationItemRoute, orderRoute:
		return method == http.MethodGet
	case claimLinkRoute:
		return method == http.MethodGet
	case claimRedeemRoute:
		return method == http.MethodPost
	case privacyRoute:
		return method == http.MethodGet
	case consentsRoute:
		return method == http.MethodPut
	case privacyExportRoute, privacyErasureRoute:
		return method == http.MethodPost
	}
	return false
}

func canonicalSecret(value string) bool {
	if len(value) != 43 {
		return false
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == value
}

func oneHeader(r *http.Request, name string) (string, bool) {
	values := r.Header.Values(name)
	returnValue := ""
	if len(values) == 1 {
		returnValue = values[0]
	}
	return returnValue, len(values) == 1
}

func forbiddenInput(r *http.Request) bool {
	queryRoute := r.URL != nil && r.Method == http.MethodGet && r.URL.EscapedPath() == r.URL.Path &&
		(r.URL.Path == "/v1/buyer/catalog" || r.URL.Path == "/v1/buyer/checkout-options" || r.URL.Path == "/v1/buyer/orders")
	if r.URL == nil || r.URL.ForceQuery || (!queryRoute && r.URL.RawQuery != "") {
		return true
	}
	for _, name := range []string{"Cookie", "Origin", "X-Tenant-ID", "X-Store-ID"} {
		if len(r.Header.Values(name)) != 0 {
			return true
		}
	}
	// The claim-link bearer is accepted only on B1/B2 (claims.go); anywhere else it is a
	// forbidden input, even when empty, so it can never ride along to another route.
	claimRoute := r.URL.EscapedPath() == r.URL.Path && (r.URL.Path == claimLinkPath || r.URL.Path == claimRedeemPath)
	return !claimRoute && len(r.Header.Values(claimTokenHeader)) != 0
}

func catalogRequest(raw string) (storefront.CatalogRequest, error) {
	var request storefront.CatalogRequest
	if len(raw) > 2048 || strings.Contains(raw, ";") || strings.HasPrefix(raw, "&") || strings.HasSuffix(raw, "&") || strings.Contains(raw, "&&") {
		return request, command.ErrInvalid
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return request, command.ErrInvalid
	}
	for name, values := range values {
		if len(values) != 1 || values[0] == "" {
			return request, command.ErrInvalid
		}
		switch name {
		case "product_id":
			if !command.ValidID(values[0]) {
				return request, command.ErrInvalid
			}
			request.ProductID = values[0]
		case "limit":
			if len(values[0]) > 3 {
				return request, command.ErrInvalid
			}
			for _, digit := range values[0] {
				if digit < '0' || digit > '9' {
					return request, command.ErrInvalid
				}
			}
			request.Page.Limit, err = strconv.Atoi(values[0])
			if err != nil || request.Page.Limit < 1 || request.Page.Limit > 100 {
				return request, command.ErrInvalid
			}
		case "cursor":
			request.Page.Cursor = values[0]
		default:
			return request, command.ErrInvalid
		}
	}
	return request, nil
}

func optionsRequest(raw string) (checkout.OptionsRequest, error) {
	var request checkout.OptionsRequest
	if len(raw) > 2048 || strings.Contains(raw, ";") || strings.HasPrefix(raw, "&") || strings.HasSuffix(raw, "&") || strings.Contains(raw, "&&") {
		return request, command.ErrInvalid
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return request, command.ErrInvalid
	}
	for name, entries := range values {
		if len(entries) != 1 || entries[0] == "" {
			return request, command.ErrInvalid
		}
		value := entries[0]
		switch name {
		case "market_id":
			if !command.ValidID(value) {
				return request, command.ErrInvalid
			}
			request.MarketID = value
		case "country":
			if len(value) != 2 || value[0] < 'A' || value[0] > 'Z' || value[1] < 'A' || value[1] > 'Z' {
				return request, command.ErrInvalid
			}
			request.Country = value
		case "limit":
			if len(value) > 3 {
				return request, command.ErrInvalid
			}
			for _, digit := range value {
				if digit < '0' || digit > '9' {
					return request, command.ErrInvalid
				}
			}
			request.Page.Limit, err = strconv.Atoi(value)
			if err != nil || request.Page.Limit < 1 || request.Page.Limit > 100 {
				return request, command.ErrInvalid
			}
		case "cursor":
			request.Page.Cursor = value
		default:
			return request, command.ErrInvalid
		}
	}
	return request, nil
}

func ordersRequest(raw string) (pagination.Request, error) {
	request, err := optionsRequest(raw)
	if err != nil || request.MarketID != "" || request.Country != "" || len(request.Page.Cursor) > 1024 {
		return pagination.Request{}, command.ErrInvalid
	}
	return request.Page, nil
}

func bearer(r *http.Request) (string, bool) {
	value, one := oneHeader(r, "Authorization")
	if !one || !strings.HasPrefix(value, "Bearer ") {
		return "", false
	}
	token := strings.TrimPrefix(value, "Bearer ")
	return token, canonicalSecret(token)
}

func keyFor(r *http.Request, noReplayKey bool, write bool) (string, bool) {
	values := r.Header.Values("Idempotency-Key")
	if noReplayKey || !write {
		return "", len(values) == 0
	}
	returnValue, one := oneHeader(r, "Idempotency-Key")
	return returnValue, one && idempotencyKey.MatchString(returnValue)
}

// keylessPaymentPath is true for handoff/refresh/cancel, whose failures must never be auto-retried.
func keylessPaymentPath(path string) bool {
	if !strings.HasPrefix(path, "/v1/buyer/orders/") {
		return false
	}
	for _, suffix := range []string{"/payment/handoff", "/payment/refresh", "/payment/cancel"} {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	issue := r.Method == http.MethodPost && r.URL != nil && r.URL.Path == "/v1/buyer/session"
	// Handoff, refresh and cancel are keyless buyer clicks whose errors are never auto-retried.
	handoff := r.URL != nil && keylessPaymentPath(r.URL.Path)
	fail := func(status int, code string) {
		if issue || handoff {
			httperror.WriteNonRetryable(w, status, code)
		} else {
			httperror.Write(w, status, code)
		}
	}
	if r.URL != nil && (r.URL.Path == claimLinkPath || r.URL.Path == claimRedeemPath) {
		// Claim responses describe one bearer link; keep them out of every shared cache (§7).
		w.Header().Set("Cache-Control", "private, no-store")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(deadline)
	_ = controller.SetWriteDeadline(deadline)
	if forbiddenInput(r) {
		fail(http.StatusForbidden, "forbidden")
		return
	}
	if r.URL.EscapedPath() != r.URL.Path {
		fail(http.StatusNotFound, "not_found")
		return
	}
	providedKey, one := oneHeader(r, "X-Commerce-Buyer-BFF-Key")
	if !one || !canonicalSecret(providedKey) || subtle.ConstantTimeCompare([]byte(providedKey), []byte(h.bffKey)) != 1 {
		fail(http.StatusUnauthorized, "unauthorized")
		return
	}
	origin, one := oneHeader(r, "X-Commerce-Storefront-Origin")
	if !one || origin == "" {
		fail(http.StatusUnprocessableEntity, "invalid_request")
		return
	}
	selected := matchRoute(r.URL.Path)
	if selected.kind == unknownRoute {
		fail(http.StatusNotFound, "not_found")
		return
	}
	if !allowed(selected.kind, r.Method) {
		fail(http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if selected.id != "" && !command.ValidID(selected.id) {
		fail(http.StatusUnprocessableEntity, "invalid_request")
		return
	}
	noReplayKey := issue || selected.kind == bootstrapRoute || selected.kind == retireRoute || isKeylessPaymentRoute(selected.kind) ||
		selected.kind == routeCVSSelectionVerify
	write := r.Method == http.MethodPut || (r.Method == http.MethodPost && !noReplayKey)
	key, valid := keyFor(r, noReplayKey, write)
	// "clm:" cart.set keys are derived by claims.RedeemLink under the opposite lock order
	// (contract live-keyword-claims-v1 §5.6); a direct cart write may never use one.
	if !valid || (selected.kind == cartRoute && strings.HasPrefix(key, claimDerivedKeyPrefix)) {
		fail(http.StatusUnprocessableEntity, "invalid_request")
		return
	}
	token := ""
	if issue {
		if len(r.Header.Values("Authorization")) != 0 {
			fail(http.StatusUnauthorized, "unauthorized")
			return
		}
	} else {
		var ok bool
		token, ok = bearer(r)
		if !ok {
			fail(http.StatusUnauthorized, "unauthorized")
			return
		}
	}
	routeInfo, err := h.resolver.Resolve(ctx, origin)
	if err != nil {
		status, code := classify(err)
		fail(status, code)
		return
	}
	// cvsHTTPError here, once for every route: a coded CVS refusal from any service (checkout Begin's pay-at-pickup PT422/PT429
	// included) is answered with its code, never the generic retryable 503 (TCV15).
	if err = cvsHTTPError(h.dispatch(ctx, w, r, selected, routeInfo.StoreID, token, key)); err != nil {
		status, code := classify(err)
		var coded codedResponse
		if errors.As(err, &coded) && coded.RetryAfterSeconds > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(coded.RetryAfterSeconds)) // 429 pay_at_pickup_limit / rate_limited
		}
		fail(status, code)
	}
}

type responseError struct {
	status int
	code   string
}

func (e responseError) Error() string { return e.code }

func (h *handler) dispatch(ctx context.Context, w http.ResponseWriter, r *http.Request, selected route, storeID, token, key string) error {
	if isPaymentRoute(selected.kind) && h.payment == nil {
		return responseError{http.StatusNotFound, "not_found"}
	}
	if selected.kind == sessionRoute && r.Method == http.MethodPost {
		if err := decodeJSON(r, &struct{}{}); err != nil {
			return err
		}
		capability, err := h.issuer.IssueForTrustedStore(ctx, storeID)
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		writeOK(w, struct {
			Token     string    `json:"token"`
			ExpiresAt time.Time `json:"expires_at"`
		}{capability.Token, capability.ExpiresAt})
		return nil
	}
	if selected.kind == bootstrapRoute {
		if err := decodeJSON(r, &struct{}{}); err != nil {
			return err
		}
		capability, err := h.issuer.RegisterForTrustedStore(ctx, storeID, token)
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		writeOK(w, struct {
			Authenticated bool      `json:"authenticated"`
			ExpiresAt     time.Time `json:"expires_at"`
		}{true, capability.ExpiresAt})
		return nil
	}
	if selected.kind == retireRoute {
		if err := decodeJSON(r, &struct{}{}); err != nil {
			return err
		}
		if err := h.issuer.RetireForTrustedStore(ctx, storeID, token); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	if r.Method == http.MethodGet || r.Method == http.MethodDelete || isKeylessPaymentRoute(selected.kind) {
		if err := noBody(r); err != nil {
			return err
		}
	}
	if selected.kind == sessionRoute {
		if r.Method == http.MethodDelete {
			if err := h.issuer.Revoke(ctx, token, storeID); err != nil {
				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			w.WriteHeader(http.StatusNoContent)
			return nil
		}
		_, err := scoped(ctx, h.pool, token, storeID, func(context.Context, pgx.Tx, buyer.Scope) (bool, error) { return true, nil })
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		writeOK(w, struct {
			Authenticated bool `json:"authenticated"`
		}{true})
		return nil
	}
	// customers-core: the privacy handlers write their own response (attachment or JSON) and return only an error.
	switch selected.kind {
	case privacyRoute:
		return h.privacyGet(ctx, w, r, storeID, token, key)
	case consentsRoute:
		return h.consentPut(ctx, w, r, storeID, token, key)
	case privacyExportRoute:
		return h.privacyExport(ctx, w, r, storeID, token, key)
	case privacyErasureRoute:
		return h.privacyErase(ctx, w, r, storeID, token, key)
	}
	var out any
	var err error
	switch selected.kind {
	case paymentRoute, paymentPrepareRoute, paymentHandoffRoute, paymentRefreshRoute, paymentCancelRoute:
		out, err = h.paymentRequest(ctx, r, selected, storeID, token, key)
	case claimLinkRoute, claimRedeemRoute:
		out, err = h.claimRequest(ctx, r, selected.kind, storeID, token, key)
	case routeCVSSelectionOpen, routeCVSSelectionGet, routeCVSSelectionVerify, routeCVSStoreEnter:
		out, err = h.cvsRequest(ctx, r, selected, storeID, token, key)
	case ordersRoute:
		var request pagination.Request
		request, err = ordersRequest(r.URL.RawQuery)
		if err == nil {
			var page pagination.Page[checkout.OrderSummary]
			page, err = h.checkout.ListOrders(ctx, token, storeID, request)
			if err == nil {
				out = projectOrders(page)
			}
		}
	case optionsRoute:
		var request checkout.OptionsRequest
		request, err = optionsRequest(r.URL.RawQuery)
		if err == nil {
			var page pagination.Page[checkout.Option]
			page, err = h.checkout.ListOptions(ctx, token, storeID, request)
			if err == nil {
				out = projectOptions(page)
			}
		}
	case catalogRoute:
		var request storefront.CatalogRequest
		request, err = catalogRequest(r.URL.RawQuery)
		if err == nil {
			var page pagination.Page[storefront.CatalogItem]
			page, err = scoped(ctx, h.pool, token, storeID, func(c context.Context, tx pgx.Tx, s buyer.Scope) (pagination.Page[storefront.CatalogItem], error) {
				return storefront.ListCatalog(c, tx, s, request)
			})
			if err == nil {
				out = projectCatalog(page)
			}
		}
	case cartRoute:
		if r.Method == http.MethodGet {
			out, err = scoped(ctx, h.pool, token, storeID, storefront.GetCart)
		} else {
			var in storefront.CartInput
			if err = decodeJSON(r, &in); err == nil {
				out, err = scoped(ctx, h.pool, token, storeID, func(c context.Context, tx pgx.Tx, s buyer.Scope) (storefront.Cart, error) {
					return storefront.SetCart(c, tx, s, key, in)
				})
			}
		}
		if err == nil {
			out = projectCart(out.(storefront.Cart))
		}
	case quotesRoute:
		var in storefront.QuoteInput
		if err = decodeJSON(r, &in); err == nil {
			out, err = scoped(ctx, h.pool, token, storeID, func(c context.Context, tx pgx.Tx, s buyer.Scope) (storefront.Quote, error) {
				return storefront.CreateQuote(c, tx, s, key, in)
			})
		}
		if err == nil {
			out = projectQuote(out.(storefront.Quote))
		}
	case quoteRoute:
		out, err = scoped(ctx, h.pool, token, storeID, func(c context.Context, tx pgx.Tx, s buyer.Scope) (storefront.Quote, error) {
			return storefront.GetQuote(c, tx, s, selected.id)
		})
		if err == nil {
			out = projectQuote(out.(storefront.Quote))
		}
	case destinationRoute:
		if r.Method == http.MethodGet {
			var current *storefront.Destination
			current, err = scoped(ctx, h.pool, token, storeID, func(c context.Context, tx pgx.Tx, s buyer.Scope) (*storefront.Destination, error) {
				return storefront.CurrentDestination(c, tx, s)
			})
			result := struct {
				Destination *destinationResponse `json:"destination"`
			}{}
			if err == nil && current != nil {
				projected := projectDestination(*current)
				result.Destination = &projected
			}
			out = result
			break
		}
		var in storefront.DestinationInput
		if err = decodeJSON(r, &in); err == nil {
			out, err = scoped(ctx, h.pool, token, storeID, func(c context.Context, tx pgx.Tx, s buyer.Scope) (storefront.Destination, error) {
				return storefront.SetDestination(c, tx, s, key, in)
			})
		}
		if err == nil {
			out = projectDestination(out.(storefront.Destination))
		}
	case destinationItemRoute:
		out, err = scoped(ctx, h.pool, token, storeID, func(c context.Context, tx pgx.Tx, s buyer.Scope) (storefront.Destination, error) {
			return storefront.GetDestination(c, tx, s, selected.id)
		})
		if err == nil {
			out = projectDestination(out.(storefront.Destination))
		}
	case checkoutRoute:
		var in checkout.Input
		if err = decodeJSON(r, &in); err == nil {
			var result checkout.Result
			result, err = h.checkout.Begin(ctx, token, storeID, key, in)
			if err == nil {
				out = projectCheckout(result)
			}
		}
	case orderRoute:
		var result checkout.Order
		result, err = h.checkout.Get(ctx, token, storeID, selected.id)
		if err == nil {
			out = projectOrder(result)
		}
	}
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if created, ok := out.(createdResponse); ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(created.Body)
		return nil
	}
	writeOK(w, out)
	return nil
}

func scoped[T any](ctx context.Context, pool *pgxpool.Pool, token, storeID string, fn func(context.Context, pgx.Tx, buyer.Scope) (T, error)) (T, error) {
	var out T
	err := buyer.WithScope(ctx, pool, token, storeID, func(c context.Context, tx pgx.Tx, s buyer.Scope) error {
		var operationError error
		out, operationError = fn(c, tx, s)
		return operationError
	})
	return out, err
}

func noBody(r *http.Request) error {
	if r.ContentLength > 0 {
		return responseError{http.StatusUnprocessableEntity, "invalid_request"}
	}
	if r.Body == nil {
		return nil
	}
	var one [1]byte
	n, err := io.ReadFull(r.Body, one[:])
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if bodyTimeout(err) {
		return context.DeadlineExceeded
	}
	if n != 0 || err != nil && !errors.Is(err, io.EOF) {
		return responseError{http.StatusUnprocessableEntity, "invalid_request"}
	}
	return nil
}

func decodeJSON(r *http.Request, value any) error {
	contentType, one := oneHeader(r, "Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if !one || err != nil || mediaType != "application/json" {
		return responseError{http.StatusUnsupportedMediaType, "json_required"}
	}
	if r.ContentLength > maxJSON {
		return responseError{http.StatusUnprocessableEntity, "invalid_request"}
	}
	if r.Body == nil {
		return responseError{http.StatusBadRequest, "invalid_json"}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxJSON+1))
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if bodyTimeout(err) {
		return context.DeadlineExceeded
	}
	if err != nil {
		return responseError{http.StatusBadRequest, "invalid_json"}
	}
	if len(body) > maxJSON {
		return responseError{http.StatusUnprocessableEntity, "invalid_request"}
	}
	if !json.Valid(body) {
		return responseError{http.StatusBadRequest, "invalid_json"}
	}
	// Token scanning sees null even in duplicate object keys that an ordinary
	// map decode would overwrite.
	scanner := json.NewDecoder(bytes.NewReader(body))
	for {
		token, scanErr := scanner.Token()
		if errors.Is(scanErr, io.EOF) {
			break
		}
		if scanErr != nil || token == nil {
			return responseError{http.StatusBadRequest, "invalid_json"}
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(value); err != nil {
		return responseError{http.StatusBadRequest, "invalid_json"}
	}
	return nil
}

func classify(err error) (int, string) {
	var response responseError
	if errors.As(err, &response) {
		return response.status, response.code
	}
	switch {
	case errors.Is(err, buyer.ErrUnauthorized):
		return http.StatusUnauthorized, "unauthorized"
	case errors.Is(err, buyer.ErrRateLimited):
		return http.StatusTooManyRequests, "rate_limited"
	case errors.Is(err, domains.ErrInvalid), errors.Is(err, buyer.ErrInvalid), errors.Is(err, command.ErrInvalid):
		return http.StatusUnprocessableEntity, "invalid_request"
	case errors.Is(err, platform.ErrForbidden):
		return http.StatusForbidden, "forbidden"
	case errors.Is(err, domains.ErrUnavailable), errors.Is(err, command.ErrNotFound):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, command.ErrInsufficient):
		return http.StatusConflict, "insufficient_inventory"
	case errors.Is(err, command.ErrConflict):
		return http.StatusConflict, "conflict"
	default:
		return http.StatusServiceUnavailable, "unavailable"
	}
}

func bodyTimeout(err error) bool {
	var timed net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timed) && timed.Timeout())
}

func writeOK(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(value)
}
