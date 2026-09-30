// Package domains owns resolving a published storefront from an exact, trusted origin to its
// published store route (domain, store, domain and publication versions), on the issuer pool.
//
// It never trusts Host or forwarded headers on its own (the BFF passes the verified origin), never
// matches by prefix or wildcard, and never grants buyer or merchant authority: a Route only says
// which store a public origin is published for.
package domains

import (
	"context"
	"errors"
	"strings"
	"time"

	"livecommerce/internal/command"
	"livecommerce/internal/platform"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInvalid     = errors.New("invalid storefront origin")
	ErrUnavailable = errors.New("storefront unavailable")
	ErrDatabase    = errors.New("storefront database unavailable")
)

const requestTimeout = 5 * time.Second

// Route is an admission snapshot; it grants no buyer or cart authority.
type Route struct {
	DomainID           string
	StoreID            string
	DomainVersion      int64
	PublicationVersion int64
	Origin             string
}

// Resolver borrows its issuer pool; the caller owns and closes the pool.
type Resolver struct {
	issuerPool *pgxpool.Pool
}

func New(ctx context.Context, issuerPool *pgxpool.Pool) (*Resolver, error) {
	if ctx == nil || issuerPool == nil {
		return nil, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := platform.ValidateBuyerIssuerPool(ctx, issuerPool); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			if errors.Is(err, context.Canceled) {
				return nil, context.Canceled
			}
			return nil, context.DeadlineExceeded
		}
		return nil, ErrDatabase
	}
	return &Resolver{issuerPool: issuerPool}, nil
}

func (r *Resolver) Resolve(ctx context.Context, origin string) (Route, error) {
	if ctx == nil || r == nil || r.issuerPool == nil {
		return Route{}, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return Route{}, err
	}
	if !ValidOrigin(origin) {
		return Route{}, ErrInvalid
	}
	callCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var route Route
	err := r.issuerPool.QueryRow(callCtx, `SELECT domain_id::text,store_id::text,domain_version,publication_version,origin
		FROM buyer.resolve_published_store($1)`, origin).
		Scan(&route.DomainID, &route.StoreID, &route.DomainVersion, &route.PublicationVersion, &route.Origin)
	if err != nil {
		return Route{}, translate(callCtx, err)
	}
	if !validRoute(route, origin) {
		return Route{}, ErrDatabase
	}
	return route, nil
}

func translate(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUnavailable
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "PT400" {
		return ErrInvalid
	}
	return ErrDatabase
}

func validRoute(route Route, origin string) bool {
	return command.ValidID(route.DomainID) && command.ValidID(route.StoreID) &&
		route.DomainID != "00000000-0000-0000-0000-000000000000" &&
		route.StoreID != "00000000-0000-0000-0000-000000000000" &&
		route.DomainVersion > 0 && route.PublicationVersion > 0 &&
		route.Origin == origin && ValidOrigin(route.Origin)
}

// ValidOrigin matches the canonical HTTPS domain grammar admitted by the
// storefront publication schema, for both buyer and merchant read paths.
func ValidOrigin(origin string) bool {
	if !strings.HasPrefix(origin, "https://") {
		return false
	}
	host := strings.TrimPrefix(origin, "https://")
	if len(host) > 253 || strings.HasSuffix(host, ".localhost") {
		return false
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 || len(labels[len(labels)-1]) == 0 || labels[len(labels)-1][0] < 'a' || labels[len(labels)-1][0] > 'z' {
		return false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			ch := label[i]
			if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
				return false
			}
		}
	}
	return true
}
