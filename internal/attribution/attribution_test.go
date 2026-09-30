package attribution

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"livecommerce/internal/command"
)

// attribution_test.go: pure-logic vectors (no PG). The hex vectors were computed independently with shasum/openssl.

func TestHashPhoneVectors(t *testing.T) {
	for in, want := range map[string]string{
		"16505551212":   "e323ec626319ca94ee8bff2e4c87cf613be6ea19919ed1364124e16807ab3176", // F16 example digits
		"+16505551212":  "e323ec626319ca94ee8bff2e4c87cf613be6ea19919ed1364124e16807ab3176",
		"+886912345678": "cf676475cec2ee7b7591f39bdefe7ef3dca63b14c1cdba3b19ce4219cbda2b09",
	} {
		if got, ok := HashPhone(in); !ok || got != want {
			t.Errorf("HashPhone(%q) = %q,%v want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "+", "0912345678", "+0886912345678", "1650555", "1650555121212345", "+1 650 555 1212",
		"+1-650-555-1212", "16505551212x", "٣٣٣٣٣٣٣٣٣٣", "++16505551212"} {
		if got, ok := HashPhone(in); ok || got != "" {
			t.Errorf("HashPhone(%q) = %q,%v want omit", in, got, ok)
		}
	}
}

func TestExternalIDVector(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	const tenant, store, owner = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"
	const want = "8b0d95f1f7b9aeedc8f474ff555c2d506ee4059d8270f9f3775f4a7578773ed8"
	if got := ExternalID(key, tenant, store, owner); got != want {
		t.Fatalf("ExternalID = %q want %q", got, want)
	}
	// Every input matters: another store, owner, tenant or key gives another id; an empty key gives none.
	base := ExternalID(key, tenant, store, owner)
	for name, got := range map[string]string{
		"store":  ExternalID(key, tenant, owner, owner),
		"owner":  ExternalID(key, tenant, store, store),
		"tenant": ExternalID(key, store, store, owner),
		"key":    ExternalID([]byte("fedcba9876543210fedcba9876543210"), tenant, store, owner),
	} {
		if got == base || len(got) != 64 {
			t.Errorf("%s change kept the id (%q)", name, got)
		}
	}
	if ExternalID(nil, tenant, store, owner) != "" {
		t.Fatal("empty key must yield no id")
	}
}

func TestEventID(t *testing.T) {
	const attempt = "0a1b2c3d-0000-4000-8000-000000000001"
	if got := EventID(attempt); got != "lc-purchase-"+attempt {
		t.Fatalf("EventID = %q", got)
	}
}

func TestCleanUserAgent(t *testing.T) {
	long := strings.Repeat("é", 600)
	for name, tc := range map[string]struct{ in, want string }{
		"plain":            {"Mozilla/5.0 (Linux; Android 14)", "Mozilla/5.0 (Linux; Android 14)"},
		"control stripped": {"Mozilla\r\n/5.0\x00\x7f", "Mozilla/5.0"},
		"invalid utf8":     {"abc\xffdef", "abcdef"},
		"trimmed":          {"  x  ", "x"},
		"empty":            {"", ""},
		"only control":     {"\r\n\t", ""},
		"truncated":        {long, strings.Repeat("é", 512)},
	} {
		if got := cleanUserAgent(tc.in); got != tc.want {
			t.Errorf("%s: got %q want %q", name, got, tc.want)
		}
	}
}

// fakeTx records Exec calls; any other pgx.Tx method panics (nil embedded interface), proving the hook uses nothing else.
type fakeTx struct {
	pgx.Tx
	calls [][]any
}

func (f *fakeTx) Exec(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
	f.calls = append(f.calls, args)
	return pgconn.CommandTag{}, nil
}

func TestPutCAPIContext(t *testing.T) {
	hash := make([]byte, 32)
	const store = "22222222-2222-4222-8222-222222222222"
	for name, tc := range map[string]struct {
		tx    *fakeTx
		hash  []byte
		store string
		ua    string
		err   error
		calls int
	}{
		"short hash":  {&fakeTx{}, hash[:31], store, "ua", command.ErrInvalid, 0},
		"bad store":   {&fakeTx{}, hash, "not-a-uuid", "ua", command.ErrInvalid, 0},
		"empty ua":    {&fakeTx{}, hash, store, "", nil, 0},
		"control ua":  {&fakeTx{}, hash, store, "\r\n", nil, 0},
		"valid":       {&fakeTx{}, hash, store, "Mozilla/5.0\r\n", nil, 1},
		"long ua cut": {&fakeTx{}, hash, store, strings.Repeat("a", 900), nil, 1},
	} {
		err := PutCAPIContext(context.Background(), tc.tx, tc.hash, tc.store, tc.ua)
		if err != tc.err || len(tc.tx.calls) != tc.calls {
			t.Errorf("%s: err=%v calls=%d want %v/%d", name, err, len(tc.tx.calls), tc.err, tc.calls)
		}
	}
	tx := &fakeTx{}
	_ = PutCAPIContext(context.Background(), tx, hash, store, "Mozilla/5.0\r\n")
	if got := tx.calls[0][2]; got != "Mozilla/5.0" {
		t.Fatalf("stored user agent = %q", got)
	}
	if err := PutCAPIContext(context.Background(), nil, hash, store, "ua"); err != command.ErrInvalid {
		t.Fatalf("nil tx = %v", err)
	}
}
