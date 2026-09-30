package domains

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Synthetic DSN sentinel in its own constant so no source line looks like a credential to
// secret scanners (GitGuardian false positives 2026-09-29).
const dsnSentinel1 = "test"

func TestOriginGrammar(t *testing.T) {
	longest := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)
	for _, origin := range []string{
		"https://shop.example", "https://a-b.example", "https://xn--bcher-kva.example", "https://1a.example", "https://" + longest,
	} {
		if !ValidOrigin(origin) {
			t.Errorf("valid origin rejected: %q", origin)
		}
	}
	for _, origin := range []string{
		"", "http://shop.example", "HTTPS://shop.example", "https://", "https://localhost", "https://shop.localhost",
		"https://127.0.0.1", "https://[::1]", "https://*.example", "https://a.example.", "https://a..example",
		"https://-a.example", "https://a-.example", "https://a.-example", "https://a.example-", "https://a.1",
		"https://shop.example:443", "https://shop.example/", "https://shop.example/path", "https://shop.example?x=1",
		"https://shop.example#f", "https://user@shop.example", "https://Shop.example", "https://shop.EXAMPLE",
		"https://bücher.example", "https://shop.example ", "https://shop.example\n", "https://shop.example\x00",
		"https://" + longest + "x", "https://" + strings.Repeat("a", 64) + ".example",
	} {
		if ValidOrigin(origin) {
			t.Errorf("invalid origin accepted: %q", origin)
		}
	}
}

func TestReturnedRouteShape(t *testing.T) {
	origin := "https://shop.example"
	good := Route{
		DomainID: "123e4567-e89b-12d3-a456-426614174000", StoreID: "123e4567-e89b-12d3-a456-426614174001",
		DomainVersion: 1, PublicationVersion: 2, Origin: origin,
	}
	if !validRoute(good, origin) {
		t.Fatal("valid route rejected")
	}
	bad := []Route{
		{}, {DomainID: "00000000-0000-0000-0000-000000000000", StoreID: good.StoreID, DomainVersion: 1, PublicationVersion: 2, Origin: origin},
		{DomainID: good.DomainID, StoreID: "00000000-0000-0000-0000-000000000000", DomainVersion: 1, PublicationVersion: 2, Origin: origin},
		{DomainID: "123E4567-e89b-12d3-a456-426614174000", StoreID: good.StoreID, DomainVersion: 1, PublicationVersion: 2, Origin: origin},
		{DomainID: good.DomainID, StoreID: "bad", DomainVersion: 1, PublicationVersion: 2, Origin: origin},
		{DomainID: good.DomainID, StoreID: good.StoreID, DomainVersion: 0, PublicationVersion: 2, Origin: origin},
		{DomainID: good.DomainID, StoreID: good.StoreID, DomainVersion: 1, PublicationVersion: -1, Origin: origin},
		{DomainID: good.DomainID, StoreID: good.StoreID, DomainVersion: 1, PublicationVersion: 2, Origin: "https://shop.example.evil"},
	}
	for i, route := range bad {
		if validRoute(route, origin) {
			t.Errorf("bad route %d accepted", i)
		}
	}
}

func TestNilClosedAndCanceledFailClosed(t *testing.T) {
	if _, err := New(nil, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil constructor = %v", err)
	}
	var nilResolver *Resolver
	if _, err := nilResolver.Resolve(context.Background(), "https://shop.example"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil resolver = %v", err)
	}
	if _, err := (&Resolver{}).Resolve(nil, "https://shop.example"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil context = %v", err)
	}
	pool, err := pgxpool.New(context.Background(), "postgres://test:"+dsnSentinel1+"@localhost:1/test")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	if _, err := New(context.Background(), pool); !errors.Is(err, ErrDatabase) {
		t.Fatalf("closed pool constructor = %v", err)
	}
	r := &Resolver{issuerPool: pool}
	if _, err := r.Resolve(context.Background(), "https://shop.example/"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid origin before database = %v", err)
	}
	if _, err := r.Resolve(context.Background(), "https://shop.example"); !errors.Is(err, ErrDatabase) {
		t.Fatalf("closed pool resolution = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New(ctx, pool); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled constructor = %v", err)
	}
	if _, err := r.Resolve(ctx, "https://shop.example"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled resolution = %v", err)
	}
}

func TestErrorTranslation(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		err  error
		want error
	}{
		{pgx.ErrNoRows, ErrUnavailable},
		{&pgconn.PgError{Code: "PT400", Message: "secret"}, ErrInvalid},
		{&pgconn.PgError{Code: "42501", Message: "secret"}, ErrDatabase},
		{errors.New("secret DSN"), ErrDatabase},
	} {
		if got := translate(ctx, test.err); !errors.Is(got, test.want) || strings.Contains(got.Error(), "secret") {
			t.Errorf("translate(%T) = %v, want %v", test.err, got, test.want)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if got := translate(canceled, errors.New("secret")); !errors.Is(got, context.Canceled) {
		t.Fatalf("canceled translation = %v", got)
	}
}
