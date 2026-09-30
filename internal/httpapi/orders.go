package httpapi

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"livecommerce/internal/command"
	"livecommerce/internal/merchantorders"
	"livecommerce/internal/platform"
)

func registerOrderRoutes(mux *http.ServeMux, pool *pgxpool.Pool) {
	const base = "/v1/admin/stores/{store_id}/orders"
	mux.HandleFunc("GET "+base, orderRoute(pool, false))
	mux.HandleFunc("GET "+base+"/{order_id}", orderRoute(pool, true))
}

func orderRoute(pool *pgxpool.Pool, detail bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			respondError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		if _, present := r.Header[http.CanonicalHeaderKey("Idempotency-Key")]; present ||
			r.ContentLength != 0 || len(r.TransferEncoding) != 0 || (r.Body != nil && r.Body != http.NoBody) {
			respondError(w, http.StatusUnprocessableEntity, "invalid_request")
			return
		}
		if detail {
			if r.URL.RawQuery != "" || r.URL.ForceQuery || !command.ValidID(r.PathValue("order_id")) {
				respondError(w, http.StatusUnprocessableEntity, "invalid_request")
				return
			}
			scoped(pool, "orders:read", func(ctx context.Context, tx pgx.Tx, s platform.Scope, request *http.Request) (any, error) {
				return merchantorders.Get(ctx, tx, s, bearerToken(request), request.PathValue("order_id"))
			})(w, r)
			return
		}
		in, err := parseOrdersQuery(r.URL)
		if err != nil {
			respondError(w, http.StatusUnprocessableEntity, "invalid_request")
			return
		}
		scoped(pool, "orders:read", func(ctx context.Context, tx pgx.Tx, s platform.Scope, request *http.Request) (any, error) {
			return merchantorders.List(ctx, tx, s, bearerToken(request), in)
		})(w, r)
	}
}

func parseOrdersQuery(u *url.URL) (merchantorders.ListRequest, error) {
	in := merchantorders.ListRequest{State: "all"}
	if u.ForceQuery || len(u.RawQuery) > 4096 {
		return in, command.ErrInvalid
	}
	if u.RawQuery != "" {
		for _, field := range strings.Split(u.RawQuery, "&") {
			if field == "" || !strings.Contains(field, "=") {
				return in, command.ErrInvalid
			}
		}
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return in, command.ErrInvalid
	}
	for name, value := range values {
		if len(value) != 1 || value[0] == "" {
			return in, command.ErrInvalid
		}
		switch name {
		case "limit":
			if strings.Trim(value[0], "0123456789") != "" {
				return in, command.ErrInvalid
			}
			in.Page.Limit, err = strconv.Atoi(value[0])
			if err != nil || in.Page.Limit < 1 || in.Page.Limit > 100 || strconv.Itoa(in.Page.Limit) != value[0] {
				return in, command.ErrInvalid
			}
		case "cursor":
			if len(value[0]) > 1024 {
				return in, command.ErrInvalid
			}
			in.Page.Cursor = value[0]
		case "state":
			switch value[0] {
			case "all", "DRAFT", "AWAITING_PAYMENT", "CONFIRMED", "CANCELLED", "shipped", "unshipped":
				in.State = value[0]
			default:
				return in, command.ErrInvalid
			}
		default:
			return in, command.ErrInvalid
		}
	}
	return in, nil
}
